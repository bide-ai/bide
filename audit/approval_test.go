package audit_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// ProveApproval proves one decision by its record name; the disclosed record's signature
// verifies under the approver's key over the call's decision bytes.
func TestProveApproval(t *testing.T) {
	ctx := context.Background()
	g := passedGate(t)
	sth := g.sth(t, 1700000000)
	recs, err := g.store.History(ctx, gateRun)
	if err != nil {
		t.Fatal(err)
	}
	var alice agent.Record
	for _, r := range recs {
		if agent.IsApprovalDecision(r, "c1") && r.Approver == "alice" {
			alice = r
		}
	}
	pb, err := audit.ProveApproval(ctx, g.store, gateRun, alice.Name, sth)
	if err != nil {
		t.Fatalf("ProveApproval: %v", err)
	}
	if ok, err := pb.Verify(g.logPub); err != nil || !ok || pb.Inclusion.Size != sth.Size {
		t.Fatalf("bundle: ok=%v err=%v size=%d, want it to verify under the STH", ok, err, pb.Inclusion.Size)
	}
	r := pb.Record
	if !ed25519.Verify(g.pubs["alice"], agent.ApprovalDecisionBytes(g.subject(t), r.Approver, r.Approved), r.Signature) {
		t.Fatal("disclosed signature does not verify under alice's key")
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if ok, _ := pb.Verify(other); ok {
		t.Fatal("bundle verified under the wrong log key")
	}
	if _, err := audit.ProveApproval(ctx, g.store, gateRun, "approval:c1:nobody:00", sth); err == nil {
		t.Fatal("ProveApproval for an unknown record should error")
	}
	if _, err := audit.ProveApproval(ctx, g.store, gateRun, agent.ApprovalTallyStep("c1"), sth); err == nil {
		t.Fatal("ProveApproval for a non-decision record should error")
	}
}

// ApprovalEvidence only applies to an m-of-n gated call whose gate has decided.
func TestApprovalEvidence_Errors(t *testing.T) {
	ctx := context.Background()

	// A paused gate has no terminal tally yet.
	g := newGate(2, []string{"alice", "bob"}, []string{"alice", "bob"})
	wantPaused(t, g.run(), 0)
	if _, err := audit.ApprovalEvidence(ctx, g.store, gateRun, "c1", g.sth(t, 1)); err == nil {
		t.Fatal("ApprovalEvidence for an undecided gate should error")
	}

	// An unknown call, and a call on a completed gated run that is not the gated one.
	g = passedGate(t)
	if _, err := audit.ApprovalEvidence(ctx, g.store, gateRun, "nope", g.sth(t, 1)); err == nil {
		t.Fatal("ApprovalEvidence for an unknown call should error")
	}
}
