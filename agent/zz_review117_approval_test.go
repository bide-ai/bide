package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// R117-5 (decision 2). A policy meant as an m-of-n gate whose approver list came up empty (read
// from config, say) is taken as SingleApproval: one unsigned Approve, from any caller, with no
// approver identity or signature, runs the call. Before P12 the same policy was ErrConfig. The
// operator asked for Need 1 of a named set; the shape rule silently drops the set.
func TestR117_EmptyApproverListIsAnUnsignedSingleGate(t *testing.T) {
	approvers := []string{} // e.g. strings.Fields(os.Getenv("APPROVERS")) with the variable unset
	var calls atomic.Int32
	wire := Func("wire", "", Safety{}, func(context.Context, struct{}) (string, error) {
		calls.Add(1)
		return "sent", nil
	}, WithApproval(&ApprovalPolicy{Need: 1, Approvers: approvers}))
	ctx := context.Background()
	store := NewMemStore()
	m := NewScriptedModel(ToolTurn("c1", "wire", `{}`), TextTurn("done"))
	_, err := New(m, store, wire).Run(ctx, "r1", "pay")
	var pa *ApprovalPending
	if !errors.As(err, &pa) {
		t.Fatalf("Run: %v, want *ApprovalPending", err)
	}
	if err := Approve(ctx, store, "r1", "c1", true); err != nil { // no identity, no signature
		t.Fatal(err)
	}
	if _, err := New(m, store, wire).Run(ctx, "r1", "pay"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if calls.Load() == 1 {
		t.Fatal("a Need-1-of-a-named-set policy with an empty set ran after one unsigned Approve; want ErrConfig at construction")
	}
}
