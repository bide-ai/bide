package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
)

// specOnlyTool describes itself with a Spec method (the new method set).
type specOnlyTool struct {
	spec  ToolSpec
	calls *atomic.Int32
}

func (t specOnlyTool) Name() string                { return t.spec.Name }
func (t specOnlyTool) Description() string         { return t.spec.Description }
func (t specOnlyTool) ArgsSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t specOnlyTool) Safety() Safety              { return t.spec.Safety }
func (t specOnlyTool) Spec() ToolSpec              { return t.spec }
func (t specOnlyTool) Call(context.Context, json.RawMessage) (json.RawMessage, error) {
	t.calls.Add(1)
	return json.RawMessage(`"sent"`), nil
}

// A tool's own Spec may return a SingleApproval whose fields were changed (Validate and
// MarshalJSON both refuse it as ErrConfig). New does not check the spec's Approval, and the
// one-decision gate is enforced without Validate, so the call is approved and its side effect
// fires; only then does the result record fail to encode. The effect fired with no recorded
// outcome, and the run halts on it, where a check in New would have refused the tool up front.
func TestRev117e_MutatedSingleApprovalFiresThenCannotRecord(t *testing.T) {
	p := SingleApproval()
	p.Need = 2
	var calls atomic.Int32
	tool := specOnlyTool{spec: ToolSpec{Name: "send", Approval: p}, calls: &calls}
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "send", `{}`), TextTurn("done"))
	a := mustNew(m, store, WithTools(tool))
	ctx := context.Background()
	_, err := a.Run(ctx, "r", UserText("go"))
	var ap *ApprovalPending
	if !errors.As(err, &ap) {
		if errors.Is(err, ErrConfig) && calls.Load() == 0 {
			return // refused up front: sound
		}
		t.Fatalf("first Run = %v, want ApprovalPending or ErrConfig", err)
	}
	if err := Approve(ctx, store, "r", "c1", true); err != nil {
		t.Fatal(err)
	}
	_, err = a.Run(ctx, "r", UserText("go"))
	if calls.Load() > 0 && err != nil {
		t.Fatalf("the tool ran %d time(s) and the run then failed with %v", calls.Load(), err)
	}
}
