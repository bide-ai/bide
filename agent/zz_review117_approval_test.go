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
	err := panicsWith(func() {
		Func("wire", "", Safety{}, func(context.Context, struct{}) (string, error) {
			calls.Add(1)
			return "sent", nil
		}, WithApproval(&ApprovalPolicy{Need: 1, Approvers: approvers}))
	})
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("WithApproval(Need 1 of an empty set) = %v; want ErrConfig at construction, never the one-decision gate", err)
	}
	// Only SingleApproval asks for the one-decision gate.
	if err := panicsWith(func() {
		Func("wire", "", Safety{}, func(context.Context, struct{}) (string, error) { return "", nil }, WithApproval(SingleApproval()))
	}); err != nil {
		t.Fatalf("WithApproval(SingleApproval()): %v", err)
	}
	// A SingleApproval whose fields were changed is not the one-decision gate either.
	p := SingleApproval()
	p.Approvers = []string{"ops"}
	p.Need = 2
	if err := checkApproval(p); !errors.Is(err, ErrConfig) {
		t.Fatalf("a changed SingleApproval: %v, want ErrConfig", err)
	}
}
