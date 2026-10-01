package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bide-ai/bide/internal/strictjson"
	"github.com/bide-ai/bide/internal/toolhook"
)

// quorumTally evaluates the m-of-n gate for tu. It re-reads the run's journal (the
// authoritative source of the decisions SubmitDecision recorded) and counts it with
// TallyApprovals, the same rule offline verification runs. final reports whether the gate
// has reached a terminal outcome: passed (Need approvals) or unreachable (too few approvers
// remain who have not validly denied). A non-final tally means the run must stay paused.
//
// Only a terminal tally is journaled, as a StepValue named ApprovalTallyStep(tu.ID), before
// the tool runs. Steps are append-once by name, so writing a still-pending count would freeze
// a stale tally; recording only the terminal outcome keeps one final, provable record per
// resolved gate, carrying the policy it enforced and every decision record it read. Once that
// record exists it is authoritative: a replay reuses it rather than recounting, so the outcome
// cannot drift if keys or policy change later.
// decodeTally reads the terminal tally an m-of-n gate journaled (r, a StepValue record of run
// runID), strictly, as audit.VerifyApprovals reads it (no duplicate or case-variant name, no
// unknown field): a tally that read one way here and another to the audit would let the gate run
// a tool the audit says was not approved. Every reader of a recorded tally reads it through here.
func decodeTally(runID string, r Record) (ApprovalTally, error) {
	var t ApprovalTally
	if err := strictjson.Unmarshal(r.Result, &t, nil); err != nil {
		return ApprovalTally{}, fmt.Errorf("decode %s (run %s): %w (%w)", r.Name, runID, err, ErrStorage)
	}
	return t, nil
}

func (a *Agent) quorumTally(ctx context.Context, runID string, tu ToolUse, pol *ApprovalPolicy) (ApprovalTally, bool, error) {
	if err := pol.Validate(); err != nil {
		return ApprovalTally{}, false, fmt.Errorf("agent: tool %q: %w", tu.Name, err)
	}
	if a.approverVerifiers == nil {
		return ApprovalTally{}, false, fmt.Errorf("agent: tool %q has an m-of-n Approval policy but no approver verifiers are configured (see WithApproverVerifiers): %w", tu.Name, ErrConfig)
	}
	// One seat per signing key (F5), checked on every evaluation and before a recorded tally is
	// read, so a resolver changed since the last run is checked again. A recorded tally is still
	// reused, not recounted: one written before this check existed stands as recorded.
	if _, _, err := approverSeats(*pol, a.approverVerifiers); err != nil {
		return ApprovalTally{}, false, fmt.Errorf("agent: tool %q: %w", tu.Name, err)
	}
	recs, err := a.store.History(ctx, runID)
	if err != nil {
		return ApprovalTally{}, false, fmt.Errorf("load history %s: %w (%w)", runID, err, ErrStorage)
	}
	name := ApprovalTallyStep(tu.ID)
	for _, r := range recs {
		if r.Name == name && r.Kind == StepValue {
			t, err := decodeTally(runID, r)
			if err != nil {
				return ApprovalTally{}, false, err
			}
			return t, true, nil
		}
	}
	subject := ApprovalSubject{RunID: runID, ToolUseID: tu.ID, ToolName: tu.Name, Args: tu.Args}
	tally, _ := TallyApprovals(recs, subject, *pol, a.approverVerifiers)
	if !tally.Passed() && !tally.Unreachable() {
		return tally, false, nil
	}
	tally, err = step(ctx, a.store, runID, name, func(context.Context) (ApprovalTally, error) { return tally, nil }, StepSafety(Safety{ReadOnly: true}))
	if err != nil {
		return ApprovalTally{}, false, fmt.Errorf("record %s (run %s): %w (%w)", name, runID, err, ErrStorage)
	}
	return tally, true, nil
}

// toolNameFor finds the tool name for a tool-use ID across the journaled model turns.
func toolNameFor(recs []Record, id string) (string, bool) {
	for _, r := range recs {
		if r.Kind != StepModel || r.Message == nil {
			continue
		}
		for _, tu := range r.Message.toolUses() {
			if tu.ID == id {
				return tu.Name, true
			}
		}
	}
	return "", false
}

// pending reports whether an assistant message has tool calls without recorded results.
// terminalCallDone reports whether the latest assistant turn in msgs called the terminal tool
// named tool and that call succeeded (its result is recorded and is not an error), and returns
// that turn. The result is read from msgs, so a resumed run that crashed after recording the
// call ends the same way a live one does. With no terminal tool (tool is ""), no call matches.
func terminalCallDone(msgs []Message, tool string) (Message, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != RoleAssistant {
			continue
		}
		calls := map[string]bool{}
		for _, tu := range msgs[i].toolUses() {
			if tu.Name == tool {
				calls[tu.ID] = true
			}
		}
		for _, m := range msgs[i+1:] {
			for _, p := range m.Parts {
				if tr, ok := p.(ToolResult); ok && calls[tr.ToolUseID] && !tr.IsError {
					return msgs[i], true
				}
			}
		}
		return Message{}, false
	}
	return Message{}, false
}

// toolResultMessage is the conversation message for a recorded tool result.
func toolResultMessage(r Record) Message {
	return Message{Role: RoleTool, Parts: []Part{ToolResult{ToolUseID: r.ToolUseID, Result: r.Result, IsError: r.IsError}}}
}

// callTool runs call under ctx, bounded by timeout when it is positive (a tool's
// ToolSpec.Timeout). call reports the call's final state (see callOpen). late reports an error
// returned, by a call that reached the tool, once that deadline had passed. The caller checks ctx
// first (ctxDone): an error after ctx itself was done is the run's cancellation, not a late error.
// A call that never reached the tool is never late: its tool did not run. A result is returned as
// the call returned it, whenever it came.
func callTool(ctx context.Context, timeout time.Duration, call func(context.Context) (json.RawMessage, int32, error)) (res json.RawMessage, state int32, late bool, err error) {
	if timeout <= 0 {
		res, state, err = call(ctx)
		return res, state, false, err
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, state, err = call(tctx)
	return res, state, err != nil && state == callReached && ctxDone(tctx), err
}

// ctxDone reports whether ctx is done or its deadline has passed. A context's Err is set by a
// timer that may not have run yet when a call that watched ctx.Deadline() itself returns, so the
// deadline is read too: an error returned at or after the deadline is judged by the deadline.
func ctxDone(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	dl, ok := ctx.Deadline()
	return ok && !time.Now().Before(dl)
}

// doneCause is why a ctx that ctxDone reports done is done: its cause, or DeadlineExceeded when
// the deadline has passed and its timer has not run yet.
func doneCause(ctx context.Context) error {
	if c := context.Cause(ctx); c != nil {
		return c
	}
	return context.DeadlineExceeded
}

// recordedSafety is the Safety a call's result records: the one it ran under.
func recordedSafety(spec ToolSpec) *Safety {
	s := spec.Safety
	return &s
}

// sortSpecs sets specList to a.specs sorted by name, the list each model request is sent. The
// specs are the snapshot New (or cloneWith) took of each tool's spec when it was registered (see
// SpecOf): the agent decides every call from it, so a tool whose Spec or Safety method would
// answer differently later cannot change a decision the agent makes for the run's calls.
func (a *Agent) sortSpecs() {
	a.specList = make([]ToolSpec, 0, len(a.specs))
	for _, s := range a.specs {
		a.specList = append(a.specList, *s)
	}
	slices.SortFunc(a.specList, func(x, y ToolSpec) int { return strings.Compare(x.Name, y.Name) })
}

// requestTools returns the tool specs for one model request: a copy of the sorted snapshot, so a
// middleware that reorders or appends to Request.Tools does not change the next request's.
func (a *Agent) requestTools() []ToolSpec {
	if len(a.specList) == 0 {
		return nil
	}
	return slices.Clip(slices.Clone(a.specList))
}

// toolCallFor is the ToolCall the middleware chain receives for tu in run runID: tu, the
// registered tool's spec (the zero spec for an unknown name), and the agent's redactor.
func (a *Agent) toolCallFor(runID string, tu ToolUse) ToolCall {
	var s ToolSpec // the zero spec (a side effect) for a name no tool has
	if p := a.specs[tu.Name]; p != nil {
		s = *p
		s.Approval = s.Approval.Clone() // the middleware's copy: changing it changes nothing here
	}
	return ToolCall{Use: tu, Spec: s, RunID: runID, redact: a.toolErrRedact, modelArgs: tu.Args, origName: tu.Name, origID: tu.ID}
}

// A call's state records, by compare-and-swap only, whether its tool was called. It starts open.
//
//	open    -> reached        the base handler, immediately before the tool is called
//	open    -> refused        the base handler refuses the call (renamed, no such tool, context done)
//	refused -> reached        a later invocation of next, while the chain is still running, calls it
//	open    -> closed         the chain returned without the base handler reaching the tool
//	refused -> refusedClosed  the chain returned after the base handler refused the call
//
// reached, closed and refusedClosed are terminal: no transition leaves them, so an invocation of
// next that comes after the chain returned (a middleware that left it running) finds the call
// closed and is refused. A call is known not to have reached its tool only when its final state is
// refusedClosed, or closed with a chain error that wraps ErrToolNotCalled.
const (
	callOpen int32 = iota
	callReached
	callRefused
	callClosed
	callRefusedClosed
)

// enterTool moves st to reached, from open or refused (a retry of a refused call while the chain
// runs), and reports false, leaving st as it is, once the chain has closed the call. A call already
// reached stays so.
func enterTool(st *atomic.Int32) bool {
	for {
		switch s := st.Load(); s {
		case callReached:
			return true
		case callOpen, callRefused:
			if st.CompareAndSwap(s, callReached) {
				return true
			}
		default: // callClosed, callRefusedClosed: terminal
			return false
		}
	}
}

// A call's began word records whether any invocation of the base handler began the tool's Call:
// beganNone, then either beganYes (set by the base handler, by compare-and-swap, immediately before
// it calls the tool) or beganSealed (set by the loop when the chain returns, by compare-and-swap,
// if no invocation had begun). Both are terminal. So a reached call whose began word the loop seals
// never called its tool, and never will: an invocation that reaches the tool's Call afterwards finds
// the word sealed and refuses. This covers a reached call whose handler returned before the tool (a
// saga-arguments write the saga's cancellation cut off).
const (
	beganNone int32 = iota
	beganYes
	beganSealed
)

// beginCall moves b to beganYes. first reports that this invocation began the call; sealed, that
// the loop sealed it first, so the tool must not be called. A call already begun stays begun.
func beginCall(b *atomic.Int32) (first, sealed bool) {
	if b.CompareAndSwap(beganNone, beganYes) {
		return true, false
	}
	return false, b.Load() == beganSealed
}

// A call's out word is the tool's own outcome, as the base handler saw its Call return: not run,
// running, succeeded, failed (it returned an error, before its context was done, that does not
// wrap ErrToolOutcomeUnknown), or unknown. When the chain returns an error for a side effect whose
// tool began and did not itself fail (a middleware turned a success into an error, or returned a
// later invocation's refusal, or left the call running), the loop cannot record a known failure:
// "failed", like "not called", needs positive proof, so the side effect's outcome is unknown.
const (
	toolNotRun int32 = iota
	toolRunning
	toolSucceeded
	toolFailed
	toolUnknown
)

// closeCall moves st to its closed state when the chain returns (open to closed, refused to
// refusedClosed; reached stays) and returns the final state.
func closeCall(st *atomic.Int32) int32 {
	if !st.CompareAndSwap(callOpen, callClosed) {
		st.CompareAndSwap(callRefused, callRefusedClosed)
	}
	return st.Load()
}

// toolHandler builds the wrapped tool-execution chain once per run: a base handler that
// dispatches by name to the registered tool, wrapped by the middleware in order. It returns the
// chain's entry point for a call of run runID, which also reports the call's final state (see
// callOpen): reached, refused, or closed.
func (a *Agent) toolHandler(runID string) func(context.Context, ToolUse) (json.RawMessage, int32, error) {
	// A tool that is not retry-safe runs at most once per tool call, however often a middleware
	// calls next: the call's began word, set by compare-and-swap here in the base handler, decides,
	// so no middleware can get around it. (A resume builds a new chain, and the journal's attempt
	// marker decides there.)
	h := ToolHandler(func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
		tu := call.Use
		// refuse ends the call without calling the tool: the state says so, and the error wraps
		// ErrToolNotCalled for a ToolCall that carries no state (one a middleware built itself).
		refuse := func(err error) (json.RawMessage, error) {
			if call.state != nil {
				call.state.CompareAndSwap(callOpen, callRefused)
			}
			return nil, fmt.Errorf("%w (%w)", err, ErrToolNotCalled)
		}
		// The call dispatches as the model made it: a middleware may rewrite the arguments, but a
		// call renamed to another tool, or under another call's ID, would run a tool the model did
		// not call, or claim and record under the wrong call. A ToolCall the middleware built
		// itself, rather than a copy of the one it was passed, carries no original to check, and
		// is refused the same way.
		if call.origID == "" || tu.ID != call.origID || tu.Name != call.origName {
			return refuse(fmt.Errorf("tool middleware changed call %s (tool %q) to call %s (tool %q); middleware may change a call's arguments, not its tool or ID: %w",
				call.origID, cutName(call.origName), cutName(tu.ID), cutName(tu.Name), ErrConfig))
		}
		t, ok := a.tools[tu.Name]
		if !ok {
			return refuse(fmt.Errorf("call to unknown tool %q: %w", cutName(tu.Name), ErrUnknownTool))
		}
		// A deadline that passed in the middleware (a rate limiter's wait) leaves the tool uncalled:
		// the call fails as a known timeout rather than start an effect already out of time.
		if ctxDone(ctx) {
			return refuse(fmt.Errorf("tool %q (call %s) was not started: its context was done before the call: %w", tu.Name, tu.ID, doneCause(ctx)))
		}
		// A guard of this module (audit: the bound grant has not expired) refuses a call its tool
		// must not run; the refusal is a known failure, recorded.
		if guard := toolhook.CallGuard; guard != nil {
			if err := guard(ctx); err != nil {
				return refuse(err)
			}
		}
		// From here the call is reached: the chain can no longer close it, so nothing after this
		// (the accepted arguments' record, the tool) runs for a call the loop has already decided.
		if call.state != nil && !enterTool(call.state) {
			// The chain already returned (a middleware left next running): the loop has decided
			// the call's outcome without this invocation, so it must not reach the tool.
			return nil, fmt.Errorf("tool %q (call %s) was not started: the tool middleware chain had already returned: %w", tu.Name, tu.ID, ErrToolNotCalled)
		}
		// A failure here returns before the tool's Call began: the loop seals the call's began word
		// and counts the call as not called (see beganNone).
		if err := journalAcceptedArgs(ctx, t, call); err != nil {
			return nil, err
		}
		// Begin the call, immediately before the tool. The registered spec decides, never
		// call.Spec: a tool that is not retry-safe is begun once per call, and an invocation that
		// finds it begun is "already ran", which is then true; its outcome is the earlier
		// invocation's, which this one does not know (see toolNotRun).
		if call.began != nil {
			switch first, sealed := beginCall(call.began); {
			case sealed:
				return nil, fmt.Errorf("tool %q (call %s) was not started: the tool middleware chain had already returned: %w", tu.Name, tu.ID, ErrToolNotCalled)
			case !first && !a.specs[tu.Name].Safety.RetrySafe():
				return nil, fmt.Errorf("tool %q (call %s) already ran and is not retry-safe: %w", tu.Name, tu.ID, ErrToolReinvoked)
			}
		}
		if call.out != nil {
			call.out.Store(toolRunning)
		}
		res, err := t.Call(ctx, tu.Args)
		if call.out != nil {
			// The tool's own outcome: a known failure only if it said so itself, before its context
			// was done and without ErrToolOutcomeUnknown.
			switch {
			case err == nil:
				call.out.Store(toolSucceeded)
			case errors.Is(err, ErrToolOutcomeUnknown) || ctxDone(ctx):
				call.out.Store(toolUnknown)
			default:
				call.out.Store(toolFailed)
			}
		}
		return res, err
	})
	for i := len(a.toolMW) - 1; i >= 0; i-- {
		h = a.toolMW[i](h)
	}
	return func(ctx context.Context, tu ToolUse) (json.RawMessage, int32, error) {
		call := a.toolCallFor(runID, tu)
		var st, began, out atomic.Int32 // callOpen, beganNone, toolNotRun
		call.state, call.began, call.out = &st, &began, &out
		res, err := h(ctx, call)
		state := closeCall(&st)
		if state == callReached && began.CompareAndSwap(beganNone, beganSealed) {
			// Reached, but no invocation began the tool's Call, and now none can: not called.
			state = callRefusedClosed
		}
		if state == callReached && err != nil && out.Load() != toolFailed && !ctxDone(ctx) && a.unprovenFailure(ctx, tu.Name) {
			// The tool began, and did not itself fail: it is still running (a next left running),
			// it succeeded (a middleware turned that into an error), its outcome is unknown, or a
			// later invocation's refusal is what the chain returned. "Failed" needs positive proof
			// too: the side effect's outcome is unknown. (With ctx done the loop already records
			// nothing, or judges the error late, so the error keeps its own category.)
			//
			// A retry-safe tool's error stays an ordinary failure, which the model may retry, except
			// in a saga for one that changes state (Idempotent, not ReadOnly): the rollback skips a
			// step that failed as one that made no change, so a step that may have changed state is
			// recorded with an unknown outcome and reported in SagaAborted.UnknownOutcome.
			err = fmt.Errorf("tool %q (call %s): the chain returned an error, but the tool itself did not fail: %w (%w)", tu.Name, tu.ID, err, ErrToolOutcomeUnknown)
		}
		return res, state, err
	}
}

// unprovenFailure reports whether an error the chain returned for a call of the named tool, which
// began and did not itself fail, leaves the call's outcome unknown: for a side effect always, and
// for a retry-safe tool that changes state (Idempotent, not ReadOnly) inside a saga.
func (a *Agent) unprovenFailure(ctx context.Context, name string) bool {
	s := a.specs[name].Safety
	return !s.RetrySafe() || !s.ReadOnly && InSaga(ctx)
}

// journalAcceptedArgs records, before the side effect fires, the arguments a compensable call in
// a saga is about to run with, when a tool middleware changed them from the model's (see
// sagaArgsStep): compensation then undoes what the tool did, even when the call's outcome is
// later resolved by ResolveHaltRef. It is a memoized step, so a retry-safe call that runs again
// keeps the first record; a middleware that rewrites arguments must rewrite them the same way
// every time. Unchanged arguments, or a call outside a saga, journal nothing, so compensation
// reads the model's arguments, as for a journal written before this record existed. A ToolCall
// a middleware built itself carries no model arguments, so its arguments are journaled unless
// they are empty.
func journalAcceptedArgs(ctx context.Context, t Tool, call ToolCall) error {
	tu := call.Use
	if _, ok := t.(Compensator); !ok || !InSaga(ctx) {
		return nil
	}
	if bytes.Equal(call.modelArgs, tu.Args) {
		return nil
	}
	store, runID, _ := runContext(ctx) // the loop and the rollback both set it
	if _, err := store.Do(ctx, runID, sagaArgsStep(tu.ID), func(context.Context) (Record, error) {
		return Record{Kind: StepValue, Result: tu.Args}, nil
	}); err != nil {
		return fmt.Errorf("journal the arguments tool %q (call %s) accepted: %w (%w)", tu.Name, tu.ID, err, ErrStorage)
	}
	return nil
}
