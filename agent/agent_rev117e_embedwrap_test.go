package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
)

// logged is the usual decorator: it embeds a Tool and overrides Call. Tool's interface has no Spec
// method, so the struct's method set has none either, and SpecOf falls back to the old method set.
type logged struct {
	Tool
	seen *atomic.Int32
}

func (l logged) Call(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	l.seen.Add(1)
	return l.Tool.Call(ctx, args)
}

// Before P12 the approval gate was part of Safety (Safety{RequiresApproval: true}), which an
// embedding decorator forwards. Now it lives only in ToolSpec.Approval, read through a Spec method
// the decorator does not have: the gate is dropped without an error, and the side effect runs
// without the approval the tool was built to require.
func TestRev117e_EmbeddingDecoratorDropsApprovalGate(t *testing.T) {
	var sent, seen atomic.Int32
	send := Func("send", "", Safety{}, func(context.Context, struct{}) (string, error) { sent.Add(1); return "sent", nil },
		WithApproval(SingleApproval()))
	if SpecOf(send).Approval == nil {
		t.Fatal("setup: the inner tool is gated")
	}
	m := NewScriptedModel(ToolTurn("c1", "send", `{}`), TextTurn("done"))
	_, err := New(m, NewMemStore(), logged{Tool: send, seen: &seen}).Run(context.Background(), "r", "go")
	var ap *ApprovalPending
	if !errors.As(err, &ap) {
		t.Fatalf("Run = %v, sent %d time(s): the gated tool ran without approval through its decorator (SpecOf(wrapper).Approval = %v)",
			err, sent.Load(), SpecOf(logged{Tool: send, seen: &seen}).Approval)
	}
}
