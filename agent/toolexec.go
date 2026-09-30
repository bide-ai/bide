package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bide-ai/bide/internal/strictjson"
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
			// Read strictly, as audit.VerifyApprovals reads it (no duplicate or case-variant name,
			// no unknown field): a tally that read one way here and another to the audit would let
			// the gate run a tool the audit says was not approved.
			var t ApprovalTally
			if err := strictjson.Unmarshal(r.Result, &t, nil); err != nil {
				return ApprovalTally{}, false, fmt.Errorf("decode %s (run %s): %w (%w)", name, runID, err, ErrStorage)
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
// ToolSpec.Timeout). late reports an error returned after that deadline had passed while ctx
// itself was still live: the tool's own deadline, not the run's, cut the call short. A result is
// returned as the call returned it, whenever it came.
func callTool(ctx context.Context, timeout time.Duration, call func(context.Context) (json.RawMessage, error)) (res json.RawMessage, late bool, err error) {
	if timeout <= 0 {
		res, err = call(ctx)
		return res, false, err
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err = call(tctx)
	return res, err != nil && tctx.Err() != nil && ctx.Err() == nil, err
}

// recordedSafety is the Safety a call's result records: the one it ran under.
func recordedSafety(spec ToolSpec) *Safety {
	s := spec.Safety
	return &s
}

// indexTools snapshots the spec of every tool in a.tools (see SpecOf): specs by name, and
// specList sorted by name, which each model request is sent. The agent decides every call from
// this snapshot, so a tool whose Spec or Safety method would answer differently later cannot
// change a decision the agent has made or will make for the run's calls.
func (a *Agent) indexTools() {
	a.specs = make(map[string]ToolSpec, len(a.tools))
	a.specList = make([]ToolSpec, 0, len(a.tools))
	for name, t := range a.tools {
		s := SpecOf(t)
		a.specs[name] = s
		a.specList = append(a.specList, s)
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
	s := a.specs[tu.Name]
	s.Approval = s.Approval.clone() // the middleware's copy: changing it changes nothing here
	return ToolCall{Use: tu, Spec: s, RunID: runID, redact: a.toolErrRedact, modelArgs: tu.Args}
}

// toolHandler builds the wrapped tool-execution chain once per run: a base handler that
// dispatches by name to the registered tool, wrapped by the middleware in order. It returns the
// chain's entry point for a call of run runID.
func (a *Agent) toolHandler(runID string) func(context.Context, ToolUse) (json.RawMessage, error) {
	// ran holds the tool-use IDs of tools that are not retry-safe and have been invoked in this
	// run. It lives here, in the base handler, rather than in the context, so no middleware can
	// get around it: such a tool runs at most once per tool call, however often a middleware
	// calls next. (A resume builds a new chain, and the journal's attempt marker decides there.)
	var ran sync.Map
	h := ToolHandler(func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
		tu := call.Use
		t, ok := a.tools[tu.Name]
		if !ok {
			return nil, fmt.Errorf("call to unknown tool %q: %w", cutName(tu.Name), ErrUnknownTool)
		}
		// The registered spec decides, never call.Spec, which a middleware may have changed.
		if !a.specs[tu.Name].Safety.RetrySafe() {
			if _, again := ran.LoadOrStore(tu.ID, true); again {
				return nil, fmt.Errorf("tool %q (call %s) already ran and is not retry-safe: %w", tu.Name, tu.ID, ErrToolReinvoked)
			}
		}
		if err := journalAcceptedArgs(ctx, t, call); err != nil {
			return nil, err
		}
		return t.Call(ctx, tu.Args)
	})
	for i := len(a.toolMW) - 1; i >= 0; i-- {
		h = a.toolMW[i](h)
	}
	return func(ctx context.Context, tu ToolUse) (json.RawMessage, error) {
		return h(ctx, a.toolCallFor(runID, tu))
	}
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
