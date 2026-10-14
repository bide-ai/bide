package audit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

func rvDecision(t *testing.T, g *gate, id string, approved bool, at time.Time) agent.Decision {
	t.Helper()
	msg := agent.ApprovalDecisionBytes(g.subject(t), id, approved, at)
	sig, err := g.p256[id].Sign(msg)
	if err != nil {
		t.Fatal(err)
	}
	return agent.Decision{RunID: gateRun, ToolUseID: "c1", ApproverID: id, Approved: approved, DecidedAt: at, Alg: audit.AlgECDSAP256, Signature: sig}
}

// A different decision (verdict, time) is never treated as the same retry, and another approver's
// record never absorbs a decision.
func TestReviewSF2_RetryRuleDoesNotAbsorbDifferentDecisions(t *testing.T) {
	ctx := context.Background()
	g := newGate(2, []string{"alice", "bob"}, []string{"alice", "bob"})
	g.p256 = map[string]audit.ECDSAP256Signer{"alice": newP256Signer(t), "bob": newP256Signer(t)}
	wantPaused(t, g.run(), 0)
	chk := agent.WithDecisionCheck(g.resolver())
	at := testDecidedAt
	if err := agent.SubmitDecision(ctx, g.store, rvDecision(t, g, "alice", true, at), chk); err != nil {
		t.Fatal(err)
	}
	if err := agent.SubmitDecision(ctx, g.store, rvDecision(t, g, "alice", false, at), chk); !errors.Is(err, agent.ErrAlreadyDecided) {
		t.Fatalf("deny after approve = %v", err)
	}
	if err := agent.SubmitDecision(ctx, g.store, rvDecision(t, g, "alice", true, at.Add(time.Second)), chk); !errors.Is(err, agent.ErrAlreadyDecided) {
		t.Fatalf("approve at another time = %v", err)
	}
	if err := agent.SubmitDecision(ctx, g.store, rvDecision(t, g, "bob", true, at), chk); err != nil {
		t.Fatal(err)
	}
	if n := countDecisionRecords(t, g); n != 2 {
		t.Fatalf("records = %d, want 2", n)
	}
	for _, id := range []string{"alice", "bob", "alice"} {
		if err := agent.SubmitDecision(ctx, g.store, rvDecision(t, g, id, true, at), chk); err != nil {
			t.Fatalf("retry %s: %v", id, err)
		}
	}
	if n := countDecisionRecords(t, g); n != 2 {
		t.Fatalf("records after retries = %d", n)
	}
	if err := g.run(); err != nil {
		t.Fatal(err)
	}
}

// A prior record that does not verify (same approver, verdict and time) must not absorb a good
// decision: the good one is written and counts.
func TestReviewSF2_ForgedPriorRecordDoesNotAbsorbRetry(t *testing.T) {
	ctx := context.Background()
	g := newGate(1, []string{"alice"}, []string{"alice"})
	g.p256 = map[string]audit.ECDSAP256Signer{"alice": newP256Signer(t)}
	wantPaused(t, g.run(), 0)
	bad := rvDecision(t, g, "alice", true, testDecidedAt)
	bad.Signature = append([]byte(nil), bad.Signature...)
	bad.Signature[0] ^= 0xff
	if err := agent.SubmitDecision(ctx, g.store, bad); err != nil {
		t.Fatal(err)
	}
	if err := agent.SubmitDecision(ctx, g.store, rvDecision(t, g, "alice", true, testDecidedAt), agent.WithDecisionCheck(g.resolver())); err != nil {
		t.Fatal(err)
	}
	if err := g.run(); err != nil {
		t.Fatalf("the good decision was dropped: %v", err)
	}
}
