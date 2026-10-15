package audit_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/journaltest"
)

// The evidence of an expired gate verifies as not approved, and a decision written after the expiry
// (written straight into the journal, since SubmitDecision refuses it) is ignored with
// agent.ReasonAfterExpiry.
func TestVerifyApprovals_DecisionWrittenAfterExpiry(t *testing.T) {
	g := newGate(1, []string{"alice", "bob", "carol"}, []string{"alice", "bob", "carol"})
	g.policy.Timeout = time.Hour
	t0 := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	now := t0
	g.clock = func() time.Time { return now }
	if err := g.run(); err == nil {
		t.Fatal("no pause")
	}
	now = t0.Add(2 * time.Hour)
	if err := g.run(); err != nil {
		t.Fatalf("run past the deadline: %v", err)
	}
	sig := ed25519.Sign(g.privs["bob"], agent.ApprovalDecisionBytes(g.subject(t), "bob", true, testDecidedAt))
	d := agent.Decision{RunID: gateRun, ToolUseID: "c1", ApproverID: "bob", Approved: true, DecidedAt: testDecidedAt, Alg: audit.AlgEd25519, Signature: sig}
	// SubmitDecision refuses it (the gate recorded its expiry), whatever the submitter's clock says;
	// a writer with access to the journal can still put it there, after the expiry.
	if err := agent.SubmitDecision(context.Background(), g.store, d, agent.WithClock(func() time.Time { return t0.Add(30 * time.Minute) })); !errors.Is(err, agent.ErrApprovalExpired) {
		t.Fatalf("SubmitDecision after the recorded expiry = %v; want ErrApprovalExpired", err)
	}
	if _, err := journaltest.Put(context.Background(), g.store, gateRun, "approval:c1:bob:after-expiry", agent.Record{Kind: agent.StepApproval, ToolUseID: "c1", Approved: true,
		ApproverSignature: &agent.ApproverSignature{Approver: "bob", ApproverAlg: audit.AlgEd25519, Signature: sig, DecidedAt: agent.FormatTimestamp(testDecidedAt), RecordedAt: agent.FormatTimestamp(t0.Add(30 * time.Minute))}}); err != nil {
		t.Fatal(err)
	}
	acts := evidenceFor(t, g)
	for _, r := range mustHistory(t, g.store, gateRun) {
		if r.Kind == agent.StepApproval && r.Approver() == "bob" {
			pb, err := audit.ProveApproval(context.Background(), g.store, gateRun, r.Name, acts[0].Bundle.STH)
			if err != nil {
				t.Fatal(err)
			}
			acts = append(acts, audit.EvidenceAction{Label: "bob", Kind: audit.KindApproval, Ref: r.Name, Bundle: pb})
		}
	}
	v, err := audit.VerifyApprovals(acts, "c1", g.policy, g.resolver(), edV(g.logPub))
	if !errors.Is(err, audit.ErrNotVerified) {
		t.Fatalf("VerifyApprovals on an expired gate = %v; want not verified", err)
	}
	var labelled bool
	for _, ig := range v.Ignored {
		labelled = labelled || ig.Approver == "bob" && ig.Reason == agent.ReasonAfterExpiry
	}
	if !labelled {
		t.Fatalf("ignored = %+v; want bob's decision labelled %q", v.Ignored, agent.ReasonAfterExpiry)
	}
}
