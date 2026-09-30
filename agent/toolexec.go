package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
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
	recs, err := a.store.History(ctx, runID)
	if err != nil {
		return ApprovalTally{}, false, fmt.Errorf("load history %s: %w (%w)", runID, err, ErrStorage)
	}
	name := ApprovalTallyStep(tu.ID)
	for _, r := range recs {
		if r.Name == name && r.Kind == StepValue {
			var t ApprovalTally
			if err := json.Unmarshal(r.Result, &t); err != nil {
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

// toolHandler builds the wrapped tool-execution chain once per run: a base handler that
// dispatches by name to the registered tool, wrapped by the middleware in order.
func (a *Agent) toolHandler() ToolHandler {
	// ran holds the tool-use IDs of tools that are not retry-safe and have been invoked in this
	// run. It lives here, in the base handler, rather than in the context, so no middleware can
	// get around it: such a tool runs at most once per tool call, however often a middleware
	// calls next. (A resume builds a new chain, and the journal's attempt marker decides there.)
	var ran sync.Map
	h := ToolHandler(func(ctx context.Context, tu ToolUse) (json.RawMessage, error) {
		t, ok := a.tools[tu.Name]
		if !ok {
			return nil, fmt.Errorf("call to unknown tool %q: %w", cutName(tu.Name), ErrUnknownTool)
		}
		if !t.Safety().RetrySafe() {
			if _, again := ran.LoadOrStore(tu.ID, true); again {
				return nil, fmt.Errorf("tool %q (call %s) already ran and is not retry-safe: %w", tu.Name, tu.ID, ErrToolReinvoked)
			}
		}
		if err := journalAcceptedArgs(ctx, t, tu); err != nil {
			return nil, err
		}
		return t.Call(ctx, tu.Args)
	})
	for i := len(a.toolMW) - 1; i >= 0; i-- {
		h = a.toolMW[i](h)
	}
	return func(ctx context.Context, tu ToolUse) (json.RawMessage, error) {
		if t, ok := a.tools[tu.Name]; ok {
			ctx = WithToolSafety(ctx, t.Safety())
		}
		ctx = context.WithValue(ctx, modelArgsKey{}, tu.Args)
		if a.toolErrRedact != nil {
			ctx = context.WithValue(ctx, toolErrRedactKey{}, a.toolErrRedact)
		}
		return h(ctx, tu)
	}
}

// journalAcceptedArgs records, before the side effect fires, the arguments a compensable call in
// a saga is about to run with, when a tool middleware changed them from the model's (see
// sagaArgsStep): compensation then undoes what the tool did, even when the call's outcome is
// later resolved by ResolveHaltRef. It is a memoized step, so a retry-safe call that runs again
// keeps the first record; a middleware that rewrites arguments must rewrite them the same way
// every time. Unchanged arguments, or a call outside a saga, journal nothing, so compensation
// reads the model's arguments, as for a journal written before this record existed.
func journalAcceptedArgs(ctx context.Context, t Tool, tu ToolUse) error {
	if _, ok := t.(Compensator); !ok || !InSaga(ctx) {
		return nil
	}
	if model, _ := ctx.Value(modelArgsKey{}).(json.RawMessage); bytes.Equal(model, tu.Args) {
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
