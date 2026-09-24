package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// compensator. undo receives the same typed input and the output do produced.
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
	if len(args) > 0 {
		_ = json.Unmarshal(args, &in)
	}
	var out Out
	if len(result) > 0 {
		_ = json.Unmarshal(result, &out)
	}
	return t.undo(ctx, in, out)
}

// sagaTrip signals, inside the loop, that a saga step failed and the run must roll back.
type sagaTrip struct {
	toolName, toolUseID string
	cause               error
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
// rollback, and each compensation is itself a durable memoized step (at-most-once).
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
	recs, err := a.store.History(ctx, runID)
	if err != nil {
		return Message{}, err
	}
	if cause, aborting := sagaFailure(recs); aborting {
		return Message{}, a.rollback(ctx, runID, errors.New(cause))
	}

	out, err := a.run(ctx, runID, input, true, emit)
	var trip *sagaTrip
	if errors.As(err, &trip) {
		return Message{}, a.rollback(ctx, runID, trip.cause)
	}
	return out, err
}

func (a *Agent) rollback(ctx context.Context, runID string, cause error) error {
	comp, uncomp, cerr := a.rollbackRun(ctx, runID)
	return &SagaAborted{RunID: runID, Cause: cause, Compensated: comp, Uncompensated: uncomp, CompensateErr: cerr}
}

// rollbackRun compensates a run's completed writes in reverse execution order, recursing
// into sub-agent child runs so a whole agent tree rolls back as a unit (distributed
// saga). Each compensation is a durable, memoized step, so it runs at-most-once and a
// crash mid-rollback resumes cleanly.
func (a *Agent) rollbackRun(ctx context.Context, runID string) (compensated, uncompensated []string, err error) {
	recs, e := a.store.History(ctx, runID)
	if e != nil {
		return nil, nil, e
	}
	for i := len(recs) - 1; i >= 0; i-- {
		r := recs[i]
		if r.Kind != StepToolResult || r.IsError {
			continue
		}
		name, ok := toolNameFor(recs, r.ToolUseID)
		if !ok {
			continue
		}
		tool := a.tools[name]
		if tool == nil {
			continue
		}

		// Sub-agent: recurse into its child run (using the SUB-agent's own tools), so its
		// writes are compensated too. This is the distributed saga.
		if sat, ok := tool.(*subAgentTool); ok {
			cc, cu, ce := sat.sub.rollbackRun(ctx, runID+"/"+r.ToolUseID)
			compensated = append(compensated, cc...)
			uncompensated = append(uncompensated, cu...)
			if ce != nil {
				return compensated, uncompensated, ce
			}
			continue
		}

		if comp, ok := tool.(Compensator); ok {
			args, _ := argsFor(recs, r.ToolUseID)
			if _, ce := a.store.Do(ctx, runID, "@saga/compensate/"+r.ToolUseID, func(ctx context.Context) (Record, error) {
				if e := comp.Compensate(ctx, args, r.Result); e != nil {
					return Record{}, e
				}
				return Record{Kind: StepValue}, nil
			}); ce != nil {
				return compensated, uncompensated, ce // stop; earlier writes stay uncompensated
			}
			compensated = append(compensated, name)
			continue
		}

		// A completed WRITE with no compensator → dangling. Surface it; never report a
		// clean rollback while side effects remain.
		if s := tool.Safety(); !s.ReadOnly && !s.Idempotent {
			uncompensated = append(uncompensated, name)
		}
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
