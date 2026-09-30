package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// A model turn that reuses a tool-use ID from an earlier turn is a protocol fault: the loop
// keys results and journal steps by that ID, so the second call would be skipped as if it
// had already run and the run would report success with the call never executed.
func TestRun_ToolUseIDReusedAcrossTurnsIsAnError(t *testing.T) {
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{
		toolTurn("call_lookup0", "lookup", `{"q":"first"}`),
		toolTurn("call_lookup0", "lookup", `{"q":"second"}`),
		textTurn("done"),
	}}
	_, err := New(m, NewMemStore(), tool).Run(context.Background(), "r", "go")
	if !errors.Is(err, ErrToolUseIDReused) || !errors.Is(err, ErrProtocol) || !errors.Is(err, ErrModel) {
		t.Fatalf("err = %v (tool ran %d times), want ErrToolUseIDReused wrapping ErrProtocol and ErrModel", err, calls)
	}
	if calls != 1 {
		t.Fatalf("tool ran %d times, want 1", calls)
	}
}

// Two calls in one turn under the same ID are the same fault.
func TestRun_ToolUseIDTwiceInOneTurnIsAnError(t *testing.T) {
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{{
		{Event: ToolCallDelta{Index: 0, ID: "c", Name: "lookup", ArgsFragment: json.RawMessage(`{}`)}},
		{Event: ToolCallDelta{Index: 1, ID: "c", Name: "lookup", ArgsFragment: json.RawMessage(`{}`)}},
		{Event: Finish{Reason: "tool_use"}},
	}, textTurn("done")}}
	_, err := New(m, NewMemStore(), tool).Run(context.Background(), "r", "go")
	if !errors.Is(err, ErrToolUseIDReused) {
		t.Fatalf("err = %v (tool ran %d times), want ErrToolUseIDReused", err, calls)
	}
	if calls != 0 {
		t.Fatalf("tool ran %d times, want 0", calls)
	}
}

// A tool call with no ID cannot be keyed at all.
func TestRun_EmptyToolUseIDIsAnError(t *testing.T) {
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{toolTurn("", "lookup", `{}`), textTurn("done")}}
	_, err := New(m, NewMemStore(), tool).Run(context.Background(), "r", "go")
	if !errors.Is(err, ErrToolUseIDReused) {
		t.Fatalf("err = %v (tool ran %d times), want ErrToolUseIDReused", err, calls)
	}
}

// The check runs inside the model handler, below middleware, so a retry middleware sees the
// fault and a fresh attempt that issues a new ID completes the run. The rejected turn is never
// journaled.
func TestRun_ToolUseIDReusedIsRetriedByMiddleware(t *testing.T) {
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{
		toolTurn("c1", "lookup", `{"q":"first"}`),
		toolTurn("c1", "lookup", `{"q":"second"}`), // rejected
		toolTurn("c2", "lookup", `{"q":"second"}`), // the retry
		textTurn("done"),
	}}
	retryOnce := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			resp, err := next(ctx, call)
			if errors.Is(err, ErrModel) {
				return next(ctx, call)
			}
			return resp, err
		}
	}
	store := NewMemStore()
	out, err := New(m, store, tool).Use(retryOnce).Run(context.Background(), "r", "go")
	if err != nil {
		t.Fatal(err)
	}
	if out.Text() != "done" || calls != 2 {
		t.Fatalf("out = %q, tool ran %d times; want done and 2", out.Text(), calls)
	}
	recs, _ := store.History(context.Background(), "r")
	for _, r := range recs {
		if r.Kind == StepModel {
			for _, tu := range r.Message.toolUses() {
				if tu.Args != nil && string(tu.Args) == `{"q":"second"}` && tu.ID == "c1" {
					t.Fatal("the rejected turn was journaled")
				}
			}
		}
	}
}

// A journal written before the check existed (an old Gemini run whose second call reused the
// first call's ID) still replays: recorded turns are not re-checked, only live ones.
func TestRun_ReplaysJournalWithReusedToolUseID(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	asst := func(id string) *Message {
		return &Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: id, Name: "lookup", Args: json.RawMessage(`{}`)}}}
	}
	for _, r := range []struct {
		name string
		rec  Record
	}{
		{"@llm/0", Record{Kind: StepModel, Message: asst("call_lookup0")}},
		{"call_lookup0", Record{Kind: StepToolResult, ToolUseID: "call_lookup0", Result: json.RawMessage(`1`)}},
		{"@llm/1", Record{Kind: StepModel, Message: asst("call_lookup0")}},
	} {
		if _, err := store.Do(ctx, "r", r.name, func(context.Context) (Record, error) { return r.rec, nil }); err != nil {
			t.Fatal(err)
		}
	}
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{textTurn("done")}}
	out, err := New(m, store, tool).Run(ctx, "r", "go")
	if err != nil {
		t.Fatalf("replay of a recorded journal failed: %v", err)
	}
	if out.Text() != "done" || calls != 0 {
		t.Fatalf("out = %q, tool ran %d times; want done and 0", out.Text(), calls)
	}
}

// RunTyped: a final_answer rejected on one turn must not leave its ID to shadow a valid
// final_answer on the next. Before the check, the second call was skipped as already done and
// the run failed with ErrProtocol; now the reuse is reported as the model fault it is.
func TestRunTyped_ReusedFinalAnswerIDIsReported(t *testing.T) {
	m := &countModel{inner: NewScriptedModel(
		ToolTurn("call_final_answer0", finalAnswerTool, `{"name":5}`),
		ToolTurn("call_final_answer0", finalAnswerTool, `{"name":"ok"}`),
		TextTurn("done"),
	)}
	_, err := RunTyped[typedAnswer](context.Background(), New(m, NewMemStore()).WithMaxTurns(4), "r", "go")
	if !errors.Is(err, ErrToolUseIDReused) {
		t.Fatalf("err = %v, want ErrToolUseIDReused", err)
	}
}

// The check must also cover a response that a middleware returns without getting it from the
// model handler below it (a hedge backup, a fallback model, a cache): the journal records what
// the chain returns, so a reused ID from there skips the call just the same.
func TestRun_ToolUseIDReusedBySubstitutedResponseIsAnError(t *testing.T) {
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "lookup", `{}`), textTurn("done")}}
	turn := 0
	substitute := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			turn++
			if turn == 2 { // answer the second turn from elsewhere, reusing the first turn's ID
				return ModelResponse{Message: Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "c1", Name: "lookup", Args: json.RawMessage(`{}`)}}}}, nil
			}
			return next(ctx, call)
		}
	}
	_, err := New(m, NewMemStore(), tool).Use(substitute).WithMaxTurns(4).Run(context.Background(), "r", "go")
	if !errors.Is(err, ErrToolUseIDReused) {
		t.Fatalf("err = %v (tool ran %d times), want ErrToolUseIDReused", err, calls)
	}
	if calls != 1 {
		t.Fatalf("tool ran %d times, want 1", calls)
	}
}
