package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// Each condition sentinel wraps its category, so errors.Is matches at both tiers.
func TestErrors_ConditionsWrapCategories(t *testing.T) {
	cases := []struct {
		cond, cat error
	}{
		{ErrUnknownTool, ErrTool},
		{ErrToolArgs, ErrTool},
		{ErrNoRecordedOutput, ErrModel},
		{ErrTruncatedToolArgs, ErrProtocol},
		{ErrBudgetExceeded, ErrBudget},
	}
	for _, c := range cases {
		if !errors.Is(c.cond, c.cat) {
			t.Errorf("errors.Is(%v, %v) = false, want true", c.cond, c.cat)
		}
	}
	// Categories are distinct — no accidental cross-matching.
	if errors.Is(ErrModel, ErrTool) || errors.Is(ErrStorage, ErrModel) {
		t.Fatal("distinct categories must not match each other")
	}
}

// A model calling an unregistered tool yields an error classified as ErrUnknownTool
// (and therefore ErrTool).
func TestErrors_UnknownToolClassified(t *testing.T) {
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "nope", `{}`)}}
	a := mustNew(m, memJournal())

	_, err := a.Run(context.Background(), "r", "hi")
	if !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("err = %v, want errors.Is ErrUnknownTool", err)
	}
	if !errors.Is(err, ErrTool) {
		t.Fatalf("err = %v, want errors.Is ErrTool (category)", err)
	}
}

// Bad tool arguments classify as ErrToolArgs (and ErrTool), while the underlying JSON
// error stays inspectable in the chain.
func TestErrors_ToolArgsClassified(t *testing.T) {
	tool := Func("adder", "", Safety{ReadOnly: true},
		func(_ context.Context, in struct {
			A int `json:"a"`
		}) (int, error) {
			return in.A, nil
		})

	_, err := tool.Call(context.Background(), json.RawMessage(`{"a":"not-an-int"}`))
	if !errors.Is(err, ErrToolArgs) {
		t.Fatalf("err = %v, want errors.Is ErrToolArgs", err)
	}
	if !errors.Is(err, ErrTool) {
		t.Fatalf("err = %v, want errors.Is ErrTool (category)", err)
	}
}

// A truncated tool-call stream classifies as ErrTruncatedToolArgs (and ErrProtocol).
func TestErrors_TruncatedToolArgsClassified(t *testing.T) {
	ch := make(chan Emit, 3)
	ch <- Emit{Event: ToolCallDelta{Index: 0, ID: "c1", Name: "lookup", ArgsFragment: json.RawMessage(`{"q":`)}}
	ch <- Emit{Event: ToolCallDelta{Index: 0, ArgsFragment: json.RawMessage(`"x"`)}} // no closing brace
	ch <- Emit{Event: Finish{Reason: "tool_use"}}
	close(ch)

	_, _, err := NewStream(ch).Message()
	if !errors.Is(err, ErrTruncatedToolArgs) {
		t.Fatalf("err = %v, want errors.Is ErrTruncatedToolArgs", err)
	}
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("err = %v, want errors.Is ErrProtocol (category)", err)
	}
}

// Control-flow signals stay concrete types matched with errors.As, and are NOT
// swept up by the category sentinels.
func TestErrors_ControlFlowStillTyped(t *testing.T) {
	var calls int
	tool := &countingTool{name: "charge", approval: SingleApproval(), calls: &calls}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}
	a := mustNew(m, memJournal(), WithTools(tool))

	_, err := a.Run(context.Background(), "r", "pay")
	var pend *PendingApproval
	if !errors.As(err, &pend) {
		t.Fatalf("err = %v, want *PendingApproval", err)
	}
	if errors.Is(err, ErrTool) {
		t.Fatal("an approval pause is not a tool failure")
	}
}
