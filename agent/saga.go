package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

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
// including sub-agent trees). Uncompensated lists the writes the rollback did not undo: a
// completed write with no compensator, a call whose outcome is unknown (the rollback halted on it,
// with a *ResumeHalt in CompensateErr, whether or not it has a compensator), or a call whose tool
// is no longer registered. They are side effects that may need manual cleanup. CompensateErr is
// non-nil if the rollback stopped (a compensator failed, or an outcome is unknown), so writes
// before it remain uncompensated. A programmatic sub-run whose declared agent (WithSubRuns) could
// not be used is listed as "<tool> (sub-run \"<name>\": <reason>)", next to its writes.
//
// UnknownOutcome lists the saga steps that failed with an unknown outcome, the failure that
// aborted the saga included: a retry-safe step that returned ErrToolOutcomeUnknown, or an error
// after its WithTimeout deadline, may have committed before it was cut off. The rollback does not
// run their compensators (it has no result to compensate, and the effect may not have happened),
// and it does not list them as Uncompensated writes; it reports them here, so they are never
// silent. Check each against its downstream system.
type SagaAborted struct {
	RunID          string
	Cause          error
	Compensated    []string
	Uncompensated  []string
	UnknownOutcome []string
	CompensateErr  error
}

func (e *SagaAborted) Error() string {
	msg := fmt.Sprintf("saga %s aborted (%v); compensated %v", e.RunID, e.Cause, e.Compensated)
	if len(e.Uncompensated) > 0 {
		msg += fmt.Sprintf("; UNCOMPENSATED writes %v", e.Uncompensated)
	}
	if len(e.UnknownOutcome) > 0 {
		msg += fmt.Sprintf("; steps with UNKNOWN OUTCOME, possibly committed %v", e.UnknownOutcome)
	}
	if e.CompensateErr != nil {
		msg += fmt.Sprintf("; rollback INCOMPLETE: %v", e.CompensateErr)
	}
	return msg
}

func (e *SagaAborted) Unwrap() error { return e.Cause }

// unknownStepOutcome reports whether a saga step's failure err leaves its outcome unknown: it
// wraps ErrToolOutcomeUnknown (the tool said so, or returned an error after its deadline). A
// sub-agent's own abort (*SagaAborted) is not: its sub-run rolled itself back, and the rollback
// walks it and reports its unknown steps by name.
func unknownStepOutcome(err error) bool {
	if _, sub := errors.AsType[*SagaAborted](err); sub {
		return false
	}
	return errors.Is(err, ErrToolOutcomeUnknown)
}

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
// marker but before any result), resume returns *OutcomeUnknown instead: you can't safely
// auto-roll-back a step that may have committed; a human decides. A failure a human then
// records with ResolveHaltRef (Outcome.IsError) is a failed step: the next RunSaga rolls back.
func (a *Agent) RunSaga(ctx context.Context, runID, input string) (Message, error) {
	return a.runSaga(ctx, runID, input, nil)
}

// runSaga is the shared body of RunSaga and StreamSaga; emit (may be nil) receives
// lifecycle events as the loop runs.
func (a *Agent) runSaga(ctx context.Context, runID, input string, emit func(AgentEvent)) (Message, error) {
	out, _, _, err := a.runSagaWithTelemetry(ctx, runID, input, emit)
	return out, err
}

// protocol:delegation begin DOpen DRb DRbEnd RbOpen RbLoop RbSub RbSubRet RbBind RbRec RbBindRet RbComp RbRe RbReRun RbReRet RbReW

// runSagaWithTelemetry is the body of runSaga that also returns usage and turn count, for
// RunSagaResult.
func (a *Agent) runSagaWithTelemetry(ctx context.Context, runID, input string, emit func(AgentEvent)) (Message, usageTotals, int, error) {
	if err := checkRunID(ctx, runID); err != nil {
		return Message{}, usageTotals{}, 0, err
	}
	if err := a.checkTools(); err != nil {
		return Message{}, usageTotals{}, 0, err // before a rollback, which looks compensators up by name
	}
	recs, err := a.store.History(ctx, runID)
	if err != nil {
		return Message{}, usageTotals{}, 0, err
	}
	cause, aborting, err := sagaFailure(runID, recs)
	if err != nil {
		return Message{}, usageTotals{}, 0, err
	}
	if aborting {
		if err := linkSubRun(ctx, runID, a.store); err != nil { // run does this for the other drives
			return Message{}, usageTotals{}, 0, err
		}
		// Re-entered after it aborted (a sub-saga whose parent had not recorded the failure): its
		// usage goes to the tool call that started it, as run reports it (see callUsage).
		reportUsage(ctx, runID, journalTotals(recs))
		return Message{}, usageTotals{}, 0, a.rollback(ctx, runID, errors.New(cause), cause)
	}

	out, usage, turns, err := a.run(ctx, runID, []Message{UserText(input)}, true, emit)
	var trip *sagaTrip
	if errors.As(err, &trip) {
		return Message{}, usageTotals{}, 0, a.rollback(ctx, runID, trip.cause, trip.journaled)
	}
	return out, usage, turns, err
}

// protocol:lifecycle begin DRollback DAbort

// protocol:toolcall begin DRollback DRbStep DRbWait DRbComp DRbNext

// rollback compensates runID's writes and returns *SagaAborted with cause. causeText is the text the
// saga's failure record holds for cause, redacted for the journal; the terminal marker records it,
// never cause's own text.
func (a *Agent) rollback(ctx context.Context, runID string, cause error, causeText string) error {
	comp, uncomp, unknown, cerr := a.rollbackRun(ctx, runID, rootRunID(ctx, runID))
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
	return &SagaAborted{RunID: runID, Cause: cause, Compensated: comp, Uncompensated: uncomp, UnknownOutcome: unknown, CompensateErr: cerr}
}

// protocol:lifecycle end

// rollbackRun compensates a run's writes in reverse call order, recursing into sub-agent
// child runs so a whole agent tree rolls back as a unit (distributed saga). Each compensation
// is a durable, memoized step: a completed one never re-runs, and a crash mid-compensation
// re-runs it on resume (at-least-once; see Compensator).
//
// It walks every tool call the model made, not only those with a recorded result, because a
// call cut off by the abort (or by a crash) may still have taken effect:
//
//   - a call with no result but an attempt marker (a side effect that started) has an unknown
//     outcome, so the rollback stops there with a *OutcomeUnknown: a human, or a reconciler via
//     ResolveHaltRef, records what happened, and the next RunSaga resumes the rollback. The halt
//     names root, the top-level run to re-invoke, even when the call is in a sub-agent's run;
//   - a side effect with neither result nor marker never started, and is skipped;
//   - a retry-safe call with a compensator and no result is run again to learn its result
//     (safe by its declaration), then compensated;
//   - a sub-agent call is always rolled back into, whether or not it finished;
//   - so is every programmatic sub-run a call started (RunInfo.SubRunFor, linked in the journal by
//     linkSubRun), latest first, after the call itself: with the agent its tool declares for it
//     (WithSubRuns), or, with none, from the sub-run's journal alone, which reports its writes
//     as uncompensated.
func (a *Agent) rollbackRun(ctx context.Context, runID, root string) (compensated, uncompensated, unknown []string, err error) {
	recs, e := a.store.History(ctx, runID)
	if e != nil {
		return nil, nil, nil, e
	}
	var calls []ToolUse
	results := map[string]Record{}
	values := map[string]json.RawMessage{} // StepValue records by key: the accepted arguments (sagaArgsStep)
	failed := map[string]bool{}
	failedUnknown := map[string]bool{} // failed steps whose outcome is unknown: they may have committed
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
			failedUnknown[r.ToolUseID] = r.OutcomeUnknown
		case StepValue:
			values[r.Name] = r.Result
		}
	}
	links := subRunLinks(recs)
	// walkSubRuns rolls back the programmatic sub-runs the call tu started, latest first.
	walkSubRuns := func(tu ToolUse) error {
		names := links[encodeID(tu.ID)]
		for j := len(names) - 1; j >= 0; j-- {
			cc, cu, ck, ce := a.rollbackSubRun(ctx, runID, root, tu, names[j])
			compensated = append(compensated, cc...)
			uncompensated = append(uncompensated, cu...)
			unknown = append(unknown, ck...)
			if ce != nil {
				return ce
			}
		}
		return nil
	}
	// protocol:claims begin Open
	// An attempt recorded as never started changed nothing (see attempt.go).
	for _, r := range liveAttempts(recs) {
		if isToolAttempt(r) { // a Step's marker is not a call's
			started[r.ToolUseID] = true
			attemptedAt[r.ToolUseID] = r.AttemptedAt
		}
	}
	// protocol:claims end
	// A sub-agent whose own saga failed aborted this one, and rolled itself back before this
	// rollback began. Its rollback may have stopped part-way (a crash, an unknown outcome, a
	// failing compensator), and only this walk resumes it, so it is walked first, as it ran first.
	// A finished one walks again without undoing anything twice (each compensation is a memoized
	// step) and reports what it undid, so the tree's lists are whole.
	for i := len(calls) - 1; i >= 0; i-- {
		tu := calls[i]
		if !failed[tu.ID] {
			continue
		}
		if e := walkSubRuns(tu); e != nil {
			return compensated, uncompensated, unknown, e
		}
		sat, ok := asSubAgent(a.tools[tu.Name])
		if !ok {
			continue
		}
		subID := SubRunID(runID, tu.ID)
		sctx, be := bindRollback(ctx, a.tools[tu.Name], subID)
		if be != nil {
			uncompensated = append(uncompensated, tu.Name)
			return compensated, uncompensated, unknown, be
		}
		cc, cu, ck, ce := sat.sub.rollbackRun(sctx, subID, root)
		compensated = append(compensated, cc...)
		uncompensated = append(uncompensated, cu...)
		unknown = append(unknown, ck...)
		if ce != nil {
			return compensated, uncompensated, unknown, ce
		}
	}
	// Once a call is handled (whichever way the loop body ends it, a continue included), the
	// programmatic sub-runs it started are walked; the failed calls' were walked above. A body that
	// returns stops the rollback before them.
	var subErr error
	next := func(i int) int {
		if tu := calls[i]; !failed[tu.ID] {
			if subErr = walkSubRuns(tu); subErr != nil {
				return -1
			}
		}
		return i - 1
	}
	defer func() {
		if err == nil && subErr != nil {
			err = subErr
		}
	}()
	for i := len(calls) - 1; i >= 0; i = next(i) {
		tu := calls[i]
		if failed[tu.ID] {
			if failedUnknown[tu.ID] {
				// Its outcome is unknown (it said so, or returned an error after its deadline): it
				// may have committed. Its compensation is not run blind, on a result it never
				// returned; it is reported, so the rollback is never silent about it.
				unknown = append(unknown, tu.Name)
			}
			continue // the step whose failure aborted the saga: walked above if a sub-agent; otherwise it made no change
		}
		res, done := results[tu.ID]
		if done && res.IsError {
			// A failed call made no change (saga steps must be atomic), unless an earlier attempt of
			// a compensable retry-safe write journaled its arguments, its "may have begun" marker,
			// before this result (a denial) was recorded: that attempt may have taken effect, and
			// no result of it was recorded to compensate, so it is reported.
			if _, comp := a.tools[tu.Name].(Compensator); comp && res.Safety != nil && res.Safety.retrySafeWrite() {
				if _, began := values[sagaArgsStep(tu.ID)]; began {
					unknown = append(unknown, tu.Name)
				}
			}
			continue
		}
		if done && res.Safety != nil && res.Safety.ReadOnly {
			continue // it ran as ReadOnly, so it changed nothing, whatever its tool is declared as now
		}
		tool := a.tools[tu.Name]
		if tool == nil {
			// The call's tool is no longer registered, so neither its safety nor its compensator
			// is known. Decide from the journal alone: a call attempted as a side effect with no
			// recorded outcome may have taken effect, so stop for a human as below; any other
			// call may have taken effect too (a completed write, or a retry-safe call cut off),
			// and nothing here can undo it, so report it rather than a clean rollback.
			if !done && started[tu.ID] {
				uncompensated = append(uncompensated, tu.Name)
				return compensated, uncompensated, unknown, toolHalt(runID, root, tu.ID, tu.Name, markerTime(attemptedAt[tu.ID]), HaltCrashed)
			}
			uncompensated = append(uncompensated, tu.Name)
			continue
		}

		// Sub-agent: recurse into its child run (using the SUB-agent's own tools), so its
		// writes are compensated too, even if the call was cut off before it returned.
		if sat, ok := asSubAgent(tool); ok {
			subID := SubRunID(runID, tu.ID)
			sctx, be := bindRollback(ctx, tool, subID)
			if be != nil {
				uncompensated = append(uncompensated, tu.Name)
				return compensated, uncompensated, unknown, be
			}
			cc, cu, ck, ce := sat.sub.rollbackRun(sctx, subID, root)
			compensated = append(compensated, cc...)
			uncompensated = append(uncompensated, cu...)
			unknown = append(unknown, ck...)
			if ce != nil {
				return compensated, uncompensated, unknown, ce
			}
			continue
		}

		// A completed call's result records the safety it ran under (a ReadOnly one is skipped
		// above), so it is a write here even if its tool has been relabelled ReadOnly since. A
		// call with no result has no record of its safety, and goes by the tool's safety now.
		safety := a.specs[tu.Name].Safety
		if !done && safety.ReadOnly {
			continue
		}
		comp, canUndo := tool.(Compensator)
		if !done {
			switch {
			case started[tu.ID]:
				// Started, no recorded outcome: it may have taken effect. Stop for a human.
				uncompensated = append(uncompensated, tu.Name)
				return compensated, uncompensated, unknown, toolHalt(runID, root, tu.ID, tu.Name, markerTime(attemptedAt[tu.ID]), HaltCrashed)
			case !safety.RetrySafe():
				continue // no attempt marker: it never started
			case !canUndo:
				uncompensated = append(uncompensated, tu.Name) // may have run; nothing can undo it
				continue
			default:
				// Retry-safe: running it again is safe, and yields the result to compensate. It runs
				// through the tool middleware, like the live call it repeats, and in this run's
				// saga context, so the arguments it accepts are journaled as the live call's were.
				toolH := a.toolHandler(runID)
				spec := a.specs[tu.Name]
				rec, ce := a.store.Do(ctx, runID, ToolResultStep(tu.ID), func(ctx context.Context) (Record, error) {
					live := &callUsage{} // the re-run's own call, from which it may resume its programmatic sub-runs
					out, state, _, e := callTool(withCallUsage(withRunContext(ctx, a.store, runID, tu.ID, true), live), spec.Timeout, func(ctx context.Context) (json.RawMessage, int32, error) { return toolH(ctx, tu) })
					live.callReturned()
					if e != nil {
						return Record{}, e
					}
					if state != callReached {
						// A middleware answered (a cache hit) without the re-run reaching the tool:
						// that is not the step's result, and nothing was learned about it.
						return Record{}, fmt.Errorf("tool %q (call %s): the rollback's re-run was answered without reaching the tool: %w", tu.Name, tu.ID, ErrToolOutcomeUnknown)
					}
					return Record{Kind: StepToolResult, ToolUseID: tu.ID, Result: out, Safety: recordedSafety(*spec), Approval: spec.Approval.Clone()}, nil
				})
				// The re-run may have journaled the arguments it accepted, and started programmatic
				// sub-runs, whatever its outcome: read them back, so the sub-runs are walked after it
				// (an unknown outcome included).
				again, he := a.store.History(ctx, runID)
				if he != nil {
					uncompensated = append(uncompensated, tu.Name)
					return compensated, uncompensated, unknown, he
				}
				for _, r := range again {
					if r.Kind == StepValue && r.Name == sagaArgsStep(tu.ID) {
						values[r.Name] = r.Result
					}
				}
				links = subRunLinks(again)
				if ce != nil {
					// The re-run gives no result to compensate. When its outcome is unknown (the tool
					// said so, a middleware turned its success into an error, the chain answered
					// while it ran), the step is reported as unknown, as the live path reports it,
					// and the rollback goes on: re-running it again would meet the same answer, and
					// the rollback would never finish. Any other failure stops the rollback.
					if errors.Is(ce, ErrToolOutcomeUnknown) {
						unknown = append(unknown, tu.Name)
						continue
					}
					uncompensated = append(uncompensated, tu.Name)
					return compensated, uncompensated, unknown, fmt.Errorf("saga rollback: learn the outcome of %q (call %s): %w", tu.Name, tu.ID, ce)
				}
				res = rec
			}
		}

		if canUndo {
			// The arguments the tool accepted, as journaled when a middleware changed them;
			// otherwise (or in a journal written before they were journaled) the model's.
			args, ok := values[sagaArgsStep(tu.ID)]
			if !ok {
				args, _ = argsFor(recs, tu.ID)
			}
			if _, ce := a.store.Do(ctx, runID, sagaCompensateStep(tu.ID), func(ctx context.Context) (Record, error) {
				if e := comp.Compensate(ctx, args, res.Result); e != nil {
					return Record{}, e
				}
				return Record{Kind: StepValue}, nil
			}); ce != nil {
				return compensated, uncompensated, unknown, ce // stop; earlier writes stay uncompensated
			}
			compensated = append(compensated, tu.Name)
			continue
		}

		// A completed write with no compensator → dangling. Idempotent is not effect-free (a
		// status set twice is still set), so only ReadOnly calls are exempt. Surface it; never
		// report a clean rollback while side effects remain.
		uncompensated = append(uncompensated, tu.Name)
	}
	return compensated, uncompensated, unknown, nil
}

// protocol:toolcall end

// subRunLinks returns the names of the programmatic sub-runs each call started, keyed by the
// call's encoded tool-use ID (encodeID, which is a digest for a long ID), in the order they were
// started, from a saga's links to them (subRunLinkStep).
func subRunLinks(recs []Record) map[string][]string {
	var links map[string][]string
	for _, r := range recs {
		rest, ok := strings.CutPrefix(r.Name, "@subrun/")
		if !ok || r.Kind != StepValue {
			continue
		}
		encTU, _, ok := strings.Cut(rest, "/")
		var name string
		if !ok || json.Unmarshal(r.Result, &name) != nil || r.Name != subRunLinkPrefixEnc(encTU)+encodeID(name) {
			continue // not a link this version writes
		}
		if links == nil {
			links = map[string][]string{}
		}
		links[encTU] = append(links[encTU], name)
	}
	return links
}

// rollbackSubRun rolls back the programmatic sub-run name of run runID's call tu: with the agent
// tu's tool declares for it (WithSubRuns), under the context the tool's wrappers bind for it. With
// no agent declared, or one that cannot be used (it journals to another store, or the WithSubRuns
// function panicked), the sub-run is walked from its journal alone (an agent with no tools, over
// the store linkSubRun made it share), so each of its writes is reported uncompensated, and one
// whose outcome is unknown stops the rollback for a human; an unusable declaration also lists the
// call itself, with the reason (see SagaAborted).
func (a *Agent) rollbackSubRun(ctx context.Context, runID, root string, tu ToolUse, name string) (compensated, uncompensated, unknown []string, err error) {
	subID := RunInfo{RunID: runID, ToolUseID: tu.ID}.SubRunFor(name)
	tool := a.tools[tu.Name]
	var sub *Agent
	if tool != nil {
		var why string
		sub, why = declaredSubRunAgent(tool, name)
		if why == "" && sub != nil && !sameStore(sub.store, a.store) {
			sub, why = nil, "the agent WithSubRuns returned journals to another store than the run's"
		}
		if why != "" {
			uncompensated = append(uncompensated, fmt.Sprintf("%s (sub-run %q: %s)", tu.Name, name, why))
		}
	}
	if sub == nil {
		sub = &Agent{store: a.store}
	}
	sctx := ctx
	if tool != nil {
		var be error
		if sctx, be = bindRollback(ctx, tool, subID); be != nil {
			return nil, append(uncompensated, tu.Name), nil, be
		}
	}
	cc, cu, ck, err := sub.rollbackRun(sctx, subID, root)
	return cc, append(uncompensated, cu...), ck, err
}

// declaredSubRunAgent returns the agent t declares for its sub-run name (subRunAgentFor), or a
// reason it has none to give: the WithSubRuns function panicked.
func declaredSubRunAgent(t Tool, name string) (sub *Agent, why string) {
	defer func() {
		if p := recover(); p != nil {
			sub, why = nil, fmt.Sprintf("the WithSubRuns function panicked: %v", p)
		}
	}()
	return subRunAgentFor(t, name), ""
}

// sagaFailure reports whether the journal records a saga step failure (the durable abort
// trigger), and its cause.
//
// A step's failure is normally recorded as a StepSagaFail. A crash can come between the failure
// and that record: the step then has an attempt marker and no outcome, the run halts, and the
// operator records the verified outcome with ResolveHaltRef. A failure recorded that way is a failed
// step too, and aborts the saga as the StepSagaFail would have. In a saga, the only other failed
// result a call can have is a human's denial, which the model reacts to, as it does outside one.
//
// A recorded tally is read strictly (see decodeTally); one that does not decode is an error.
func sagaFailure(runID string, recs []Record) (string, bool, error) {
	denied := map[string]bool{}   // calls denied by a 1-of-1 decision
	values := map[string]Record{} // StepValue records by name, for m-of-n tallies
	for _, r := range recs {
		switch {
		case r.Kind == StepApproval && r.Approver == "" && !r.Approved:
			denied[r.ToolUseID] = true
		case r.Kind == StepValue:
			values[r.Name] = r
		}
	}
	isDenial := func(id string) (bool, error) {
		if denied[id] {
			return true, nil
		}
		v, ok := values[ApprovalTallyStep(id)]
		if !ok {
			return false, nil
		}
		t, err := decodeTally(runID, v)
		return !t.Passed(), err
	}
	for _, r := range recs {
		switch {
		case r.Kind == StepSagaFail:
		case r.Kind == StepToolResult && r.IsError:
			if d, err := isDenial(r.ToolUseID); err != nil {
				return "", false, err
			} else if d {
				continue
			}
		default:
			continue
		}
		var s string
		if json.Unmarshal(r.Result, &s) != nil {
			s = string(r.Result)
		}
		return s, true, nil
	}
	return "", false, nil
}

// protocol:delegation end

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
	b, _ := marshalJournal(s)
	return b
}
