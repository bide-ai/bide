package audit_test

import (
	"context"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/taintfixture"
)

// Probe: the evidence and the offline recount of a call and of its escalation instance are separate: each is
// recounted under its own gate, and a decision for one never counts on the other.
func TestApprovalEvidence_EscalationInstanceIsRecountedUnderItsOwnGate(t *testing.T) {
	ctx := context.Background()
	f := taintfixture.New(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"own1"}})
	if err := f.Drive(); !agent.IsPause(err) {
		t.Fatalf("drive 1 = %v", err)
	}
	if err := f.Submit("c2", "c2", "own1", true); err != nil {
		t.Fatal(err)
	}
	if err := f.Drive(); !agent.IsPause(err) {
		t.Fatalf("drive 2 = %v, want a pause on the escalation instance", err)
	}
	// A decision signed for the call is refused on the instance, and the reverse; written past the check it
	// is in the journal and the recount below ignores it.
	if err := f.Submit("c2#taint", "c2", "esc1", true); err == nil {
		t.Fatal("a decision signed for the call was recorded on its escalation instance")
	}
	if err := f.Submit("c2", "c2#taint", "own1", true); err == nil {
		t.Fatal("a decision signed for the instance was recorded on the call itself")
	}
	if err := f.SubmitUnchecked("c2#taint", "c2", "esc2", true); err != nil {
		t.Fatal(err)
	}
	if err := f.Submit("c2#taint", "c2#taint", "esc1", true); err != nil {
		t.Fatal(err)
	}
	// esc2's real decision is recorded under another name than the planted one.
	f.At = f.At.Add(time.Second)
	if err := f.Submit("c2#taint", "c2#taint", "esc2", true); err != nil {
		t.Fatal(err)
	}
	if err := f.Drive(); err != nil || f.Wrote != 1 {
		t.Fatalf("drive 3 = %v, writes %d", err, f.Wrote)
	}

	th, err := audit.NewTreeHead(ctx, f.Journal, taintfixture.Run, 1700000000)
	if err != nil {
		t.Fatal(err)
	}
	sth := signTH(t, th, f.LogPriv)
	verify := func(call string, pol agent.ApprovalPolicy) audit.ApprovalVerdict {
		t.Helper()
		acts, err := audit.ApprovalEvidence(ctx, f.Journal, taintfixture.Run, call, sth)
		if err != nil {
			t.Fatalf("ApprovalEvidence(%s): %v", call, err)
		}
		v, _ := audit.VerifyApprovals(acts, call, pol, f.Verifiers, edV(f.LogPub))
		return v
	}
	own, esc := *f.Own, *f.Esc
	if v := verify("c2", own); !v.OK || len(v.Counted) != 1 || v.Counted[0] != "own1" {
		t.Fatalf("the call under its own gate: %+v", v)
	}
	if v := verify("c2#taint", esc); !v.OK || len(v.Counted) != 2 {
		t.Fatalf("the instance under the escalation gate: %+v", v)
	}
	// The planted esc2 decision (signed for the call) is disclosed and ignored, not counted.
	if v := verify("c2#taint", esc); len(v.Ignored) == 0 {
		t.Fatalf("the decision signed for the call is not among the ignored: %+v", v)
	}
	// Under the wrong gate neither passes: the recount never counts a decision for the other instance.
	if v := verify("c2", esc); v.OK || len(v.Counted) != 0 {
		t.Fatalf("the call recounted under the escalation gate: %+v, want nothing counted", v)
	}
	if v := verify("c2#taint", own); v.OK || len(v.Counted) != 0 {
		t.Fatalf("the instance recounted under the tool's own gate: %+v, want nothing counted", v)
	}
}
