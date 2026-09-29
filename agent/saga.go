package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Compensator is an optional interface a Tool implements to declare how to UNDO its side
// effect. In a saga run (RunSaga), if a step fails after earlier writes succeeded, the
// completed compensatable writes are rolled back in reverse order — automatically, and
// recursively through sub-agent trees.
//
// This is the transactional / saga agent: "charged the card and booked the flight, then
// failed on the hotel → cleanly refund + cancel." It's the gsm compensation idea applied
// to the agent runtime — the journal records exactly which writes completed, so
// well-founded reverse compensation falls out of the durable substrate.
//
// A saga step must be ATOMIC: it must not leave a partial external side effect before
// returning an error, because the failing step itself is not compensated (there's no
// recorded result to drive Compensate). Make forward steps all-or-nothing, or idempotent.
type Compensator interface {
	// Compensate undoes a completed call. args are the tool's original arguments; result
	// is what Call returned. Must be idempotent — on a crash mid-rollback it may re-run.
	Compensate(ctx context.Context, args, result json.RawMessage) error
}

// CompensatedFunc is a typed tool that declares both its forward action and its
// compensator. undo receives the same typed input and the output do produced: the call's
// recorded arguments decoded as the forward call decoded them, strictly (see Func). A record
// written before tool arguments decoded strictly may hold arguments only encoding/json accepts;
// the forward call of that time decoded them with encoding/json, so compensation does too, and
// undoes the value that call acted on. Arguments neither decodes are ErrProtocol.
//
// The recorded arguments are the model's. A tool middleware that rewrites a compensable call's
// arguments changes what do receives but not what undo receives.
func CompensatedFunc[In, Out any](
	name, description string,
	safety Safety,
	do func(context.Context, In) (Out, error),
	undo func(context.Context, In, Out) error,
) Tool {
	return &compTool[In, Out]{Tool: Func(name, description, safety, do), undo: undo}
}

type compTool[In, Out any] struct {
	Tool
	undo func(context.Context, In, Out) error
}

func (t *compTool[In, Out]) Compensate(ctx context.Context, args, result json.RawMessage) error {
	var in In
	if err := decodeRecordedArgs(args, &in); err != nil {
		return fmt.Errorf("saga compensate %q: decode recorded args: %w (%w)", t.Name(), err, ErrProtocol)
	}
	var out Out
	if len(result) > 0 {
		if err := json.Unmarshal(result, &out); err != nil {
			return fmt.Errorf("saga compensate %q: decode recorded result: %w (%w)", t.Name(), err, ErrProtocol)
		}
	}
	return t.undo(ctx, in, out)
}

// sagaTrip signals, inside the loop, that a saga step failed and the run must roll back.
type sagaTrip struct {
	toolName, toolUseID string
	cause               error
	// journaled is the text the StepSagaFail record holds for cause: its error text as redacted
	// for the journal (see toolErrorText).
	journaled string
}

func (e *sagaTrip) Error() string { return fmt.Sprintf("saga step %q failed: %v", e.toolName, e.cause) }

// SagaAborted is returned by RunSaga when a step failed and the transaction was rolled
// back. Compensated lists tools whose side effects were undone (reverse of execution,
// including sub-agent trees). Uncompensated lists completed WRITES that had no compensator
// — dangling side effects needing manual cleanup. CompensateErr is non-nil if a
// compensator itself failed: rollback stopped, so writes before it remain uncompensated.
type SagaAborted struct {
	RunID         string
	Cause         error
	Compensated   []string
	Uncompensated []string
	CompensateErr error
}

func (e *SagaAborted) Error() string {
	msg := fmt.Sprintf("saga %s aborted (%v); compensated %v", e.RunID, e.Cause, e.Compensated)
	if len(e.Uncompensated) > 0 {
		msg += fmt.Sprintf("; UNCOMPENSATED writes (no compensator) %v", e.Uncompensated)
	}
	if e.CompensateErr != nil {
		msg += fmt.Sprintf("; rollback INCOMPLETE: %v", e.CompensateErr)
	}
	return msg
}

func (e *SagaAborted) Unwrap() error { return e.Cause }

// RunSaga runs the agent as a transaction: on success it behaves like Run; if a step
// fails after earlier writes succeeded, it compensates the completed writes in reverse
// order (recursing into sub-agent trees) and returns *SagaAborted.
//
// The abort is derived from the journal (a durable StepSagaFail record), so a crash at
// any point resumes correctly: on re-entry a recorded failure sends us straight to
// rollback, and each compensation is a durable memoized step: once recorded it never
// runs again, and a crash mid-compensation re-runs it (so Compensate must be idempotent).
//
// Note: if a non-retriable step's outcome is genuinely unknown (crashed after its attempt
// marker but before any result), resume returns *ResumeHalt instead — you can't safely
// auto-roll-back a step that may have committed; a human decides.
func (a *Agent) RunSaga(ctx context.Context, runID, input string) (Message, error) {
	return a.runSaga(ctx, runID, input, nil)
}

// runSaga is the shared body of RunSaga and StreamSaga; emit (may be nil) receives
// lifecycle events as the loop runs.
func (a *Agent) runSaga(ctx context.Context, runID, input string, emit func(AgentEvent)) (Message, error) {
	if err := a.checkTools(); err != nil {
		return Message{}, err // before a rollback, which looks compensators up by name
	}
	recs, err := a.store.History(ctx, runID)
	if err != nil {
		return Message{}, err
	}
	if cause, aborting := sagaFailure(recs); aborting {
		return Message{}, a.rollback(ctx, runID, errors.New(cause), cause)
	}

	out, _, _, err := a.run(ctx, runID, []Message{UserText(input)}, true, emit)
	var trip *sagaTrip
	if errors.As(err, &trip) {
		return Message{}, a.rollback(ctx, runID, trip.cause, trip.journaled)
	}
	return out, err
}

// runSagaWithTelemetry is the counterpart of runSaga that returns usage and turn count
// for RunSagaResult. It uses the richer run return values directly.
func (a *Agent) runSagaWithTelemetry(ctx context.Context, runID, input string, emit func(AgentEvent)) (Message, usageTotals, int, error) {
	if err := a.checkTools(); err != nil {
		return Message{}, usageTotals{}, 0, err // before a rollback, which looks compensators up by name
	}
	recs, err := a.store.History(ctx, runID)
	if err != nil {
		return Message{}, usageTotals{}, 0, err
	}
	if cause, aborting := sagaFailure(recs); aborting {
		return Message{}, usageTotals{}, 0, a.rollback(ctx, runID, errors.New(cause), cause)
	}

	out, usage, turns, err := a.run(ctx, runID, []Message{UserText(input)}, true, emit)
	var trip *sagaTrip
	if errors.As(err, &trip) {
		return Message{}, usageTotals{}, 0, a.rollback(ctx, runID, trip.cause, trip.journaled)
	}
	return out, usage, turns, err
}

// rollback compensates runID's writes and returns *SagaAborted with cause. causeText is the text the
// saga's failure record holds for cause, redacted for the journal; the terminal marker records it,
// never cause's own text.
func (a *Agent) rollback(ctx context.Context, runID string, cause error, causeText string) error {
	comp, uncomp, cerr := a.rollbackRun(ctx, runID, rootRunID(ctx, runID))
	if cerr == nil {
		// The rollback finished: the run is over. Mark it terminal so a recovery supervisor
		// leaves it alone. A rollback that stopped (an unknown outcome, a failed compensator) is
		// not marked, so it is re-driven once the cause is resolved.
		if _, err := a.store.Do(ctx, runID, runAbortedStep, func(context.Context) (Record, error) {
			return Record{Kind: StepValue, Result: mustJSON(causeText)}, nil
		}); err != nil {
			cerr = fmt.Errorf("saga %s: record the finished rollback: %w (%w)", runID, err, ErrStorage)
		}
	}
	return &SagaAborted{RunID: runID, Cause: cause, Compensated: comp, Uncompensated: uncomp, CompensateErr: cerr}
}

// rollbackRun compensates a run's writes in reverse call order, recursing into sub-agent
// child runs so a whole agent tree rolls back as a unit (distributed saga). Each compensation
// is a durable, memoized step: a completed one never re-runs, and a crash mid-compensation
// re-runs it on resume (at-least-once; see Compensator).
//
// It walks every tool call the model made, not only those with a recorded result, because a
// call cut off by the abort (or by a crash) may still have taken effect:
//
//   - a call with no result but an attempt marker (a side effect that started) has an unknown
//     outcome, so the rollback stops there with a *ResumeHalt: a human, or a reconciler via
//     ResolveHalt, records what happened, and the next RunSaga resumes the rollback. The halt
//     names root, the top-level run to re-invoke, even when the call is in a sub-agent's run;
//   - a side effect with neither result nor marker never started, and is skipped;
//   - a retry-safe call with a compensator and no result is run again to learn its result
//     (safe by its declaration), then compensated;
//   - a sub-agent call is always rolled back into, whether or not it finished.
func (a *Agent) rollbackRun(ctx context.Context, runID, root string) (compensated, uncompensated []string, err error) {
	recs, e := a.store.History(ctx, runID)
	if e != nil {
		return nil, nil, e
	}
	var calls []ToolUse
	results := map[string]Record{}
	failed := map[string]bool{}
	attemptedAt := map[string]int64{}
	started := map[string]bool{}
	for _, r := range recs {
		switch r.Kind {
		case StepModel:
			if r.Message != nil {
				calls = append(calls, r.Message.toolUses()...)
			}
		case StepToolResult:
			results[r.ToolUseID] = r
		case StepSagaFail:
			failed[r.ToolUseID] = true
		case StepAttempt:
			if r.ToolUseID != "" {
				started[r.ToolUseID] = true
				attemptedAt[r.ToolUseID] = r.AttemptedAt
			}
		}
	}
	for i := len(calls) - 1; i >= 0; i-- {
		tu := calls[i]
		tool := a.tools[tu.Name]
		if tool == nil || failed[tu.ID] {
			continue // unknown tool, or the step whose failure aborted the saga (not compensated)
		}
		res, done := results[tu.ID]
		if done && res.IsError {
			continue // a failed call made no change (saga steps must be atomic)
		}

		// Sub-agent: recurse into its child run (using the SUB-agent's own tools), so its
		// writes are compensated too, even if the call was cut off before it returned.
		if sat, ok := tool.(*subAgentTool); ok {
			cc, cu, ce := sat.sub.rollbackRun(ctx, runID+"/"+tu.ID, root)
			compensated = append(compensated, cc...)
			uncompensated = append(uncompensated, cu...)
			if ce != nil {
				return compensated, uncompensated, ce
			}
			continue
		}

		safety := tool.Safety()
		if safety.ReadOnly {
			continue
		}
		comp, canUndo := tool.(Compensator)
		if !done {
			switch {
			case started[tu.ID]:
				// Started, no recorded outcome: it may have taken effect. Stop for a human.
				var at time.Time
				if ms := attemptedAt[tu.ID]; ms != 0 {
					at = time.UnixMilli(ms)
				}
				uncompensated = append(uncompensated, tu.Name)
				return compensated, uncompensated, &ResumeHalt{RunID: runID, RootRunID: root, ToolUseID: tu.ID, ToolName: tu.Name, AttemptedAt: at}
			case !safety.RetrySafe():
				continue // no attempt marker: it never started
			case !canUndo:
				uncompensated = append(uncompensated, tu.Name) // may have run; nothing can undo it
				continue
			default:
				// Retry-safe: running it again is safe, and yields the result to compensate.
				rec, ce := a.store.Do(ctx, runID, tu.ID, func(ctx context.Context) (Record, error) {
					out, e := tool.Call(ctx, tu.Args)
					if e != nil {
						return Record{}, e
					}
					return Record{Kind: StepToolResult, ToolUseID: tu.ID, Result: out}, nil
				})
				if ce != nil {
					uncompensated = append(uncompensated, tu.Name)
					return compensated, uncompensated, fmt.Errorf("saga rollback: learn the outcome of %q (call %s): %w", tu.Name, tu.ID, ce)
				}
				res = rec
			}
		}

		if canUndo {
			args, _ := argsFor(recs, tu.ID)
			if _, ce := a.store.Do(ctx, runID, "@saga/compensate/"+tu.ID, func(ctx context.Context) (Record, error) {
				if e := comp.Compensate(ctx, args, res.Result); e != nil {
					return Record{}, e
				}
				return Record{Kind: StepValue}, nil
			}); ce != nil {
				return compensated, uncompensated, ce // stop; earlier writes stay uncompensated
			}
			compensated = append(compensated, tu.Name)
			continue
		}

		// A completed write with no compensator → dangling. Idempotent is not effect-free (a
		// status set twice is still set), so only ReadOnly calls are exempt. Surface it; never
		// report a clean rollback while side effects remain.
		uncompensated = append(uncompensated, tu.Name)
	}
	return compensated, uncompensated, nil
}

// sagaFailure reports whether the journal records a saga step failure (the durable abort
// trigger), and its cause.
func sagaFailure(recs []Record) (string, bool) {
	for _, r := range recs {
		if r.Kind == StepSagaFail {
			var s string
			_ = json.Unmarshal(r.Result, &s)
			return s, true
		}
	}
	return "", false
}

// argsFor finds the original arguments of a tool call from the journaled model turns.
func argsFor(recs []Record, toolUseID string) (json.RawMessage, bool) {
	for _, r := range recs {
		if r.Kind != StepModel || r.Message == nil {
			continue
		}
		for _, tu := range r.Message.toolUses() {
			if tu.ID == toolUseID {
				return tu.Args, true
			}
		}
	}
	return nil, false
}

func mustJSON(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// decodeRecordedArgs decodes a call's recorded arguments into in as its forward call decoded
// them: strictly, with decodeArgs, which every call journaled since tool arguments decode strictly
// passed. A record that does not decode strictly was written before that, when the forward call
// decoded with encoding/json (empty arguments as the zero value), so it is decoded that way.
func decodeRecordedArgs[In any](args json.RawMessage, in *In) error {
	if err := decodeArgs(args, in); err == nil {
		return nil
	}
	if len(args) == 0 {
		return nil
	}
	return json.Unmarshal(args, in)
}
