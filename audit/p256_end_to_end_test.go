package audit_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
)

func newP256Signer(t *testing.T) audit.ECDSAP256Signer {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return audit.ECDSAP256Signer{Priv: k}
}

// An m-of-n gate whose approvers sign with different schemes (alice Ed25519, bob and carol ECDSA
// P-256), a P-256 log key signing the tree head, and a P-256 sealed evidence package: the whole
// chain runs and verifies offline.
func TestP256_ApprovalsAndEvidenceEndToEnd(t *testing.T) {
	ctx := context.Background()
	g := newGate(2, []string{"alice", "bob", "carol"}, []string{"alice", "bob", "carol"})
	g.p256 = map[string]audit.ECDSAP256Signer{"bob": newP256Signer(t), "carol": newP256Signer(t)}
	logS := newP256Signer(t)
	logV, err := audit.VerifierOf(logS)
	if err != nil {
		t.Fatal(err)
	}

	wantPaused(t, g.run(), 0)
	g.decide(t, "carol", true, true) // a tampered P-256 signature never counts
	wantPaused(t, g.run(), 0)
	g.decide(t, "alice", true, false)
	g.decide(t, "bob", true, false)
	if err := g.run(); err != nil {
		t.Fatalf("run at quorum: %v", err)
	}

	pkg, err := audit.Evidence(ctx, g.store, gateRun, logS, 1700000000, audit.WithToolCall("c1"))
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Alg != audit.AlgECDSAP256 || pkg.STH.Alg != audit.AlgECDSAP256 || len(pkg.Signature) != 64 {
		t.Fatalf("alg %q sth alg %q seal %d bytes", pkg.Alg, pkg.STH.Alg, len(pkg.Signature))
	}
	acts, err := audit.ApprovalEvidence(ctx, g.store, gateRun, "c1", pkg.STH)
	if err != nil {
		t.Fatal(err)
	}
	pkg.Actions = append(pkg.Actions, acts[:len(acts)-1]...)
	if err := pkg.Seal(logS); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(pkg)
	if err != nil {
		t.Fatal(err)
	}
	var got audit.EvidencePackage
	if err := audit.UnmarshalStrict(raw, &got); err != nil {
		t.Fatal(err)
	}
	rep, err := got.Verify(logV)
	if err != nil || !rep.OK {
		t.Fatalf("verify: ok=%v err=%v problems=%v", rep.OK, err, rep.Problems)
	}
	v, err := audit.VerifyApprovals(got.Actions, "c1", g.policy, g.resolver(), logV)
	if err != nil || !v.OK || !slices.Equal(v.Counted, []string{"alice", "bob"}) {
		t.Fatalf("verdict %+v err %v", v, err)
	}
	if ignoredReason(v, "carol") != agent.ReasonBadSig {
		t.Fatalf("ignored = %+v, want carol's P-256 signature rejected", v.Ignored)
	}

	// A decision journaled under the wrong scheme for its approver is refused at submission.
	bobMsg := agent.ApprovalDecisionBytes(g.subject(t), "bob", true, testDecidedAt)
	sig, _ := g.p256["bob"].Sign(bobMsg)
	d := agent.Decision{RunID: gateRun, ToolUseID: "c1", ApproverID: "alice", Approved: true, DecidedAt: testDecidedAt, Alg: audit.AlgECDSAP256, Signature: sig}
	if err := agent.SubmitDecision(ctx, g.store, d, agent.WithDecisionCheck(g.resolver())); err == nil || !errors.Is(err, agent.ErrInvalidApproval) {
		t.Fatalf("a P-256 decision for an Ed25519 approver = %v, want ErrInvalidApproval", err)
	}

	// A key of another scheme does not verify the package.
	otherV, _ := audit.VerifierOf(newP256Signer(t))
	if rep, _ := got.Verify(otherV); rep.OK {
		t.Fatal("verified under another P-256 key")
	}
	edV2 := edV(g.logPub)
	if rep, _ := got.Verify(edV2); rep.OK {
		t.Fatal("a P-256 package verified under an Ed25519 key")
	}
}

// A P-256 log key anchors the signed tree head after every step, and each anchored head verifies.
// P-256 signatures are randomized, so the heads are not byte-reproducible; each still verifies.
func TestP256_AuditedStoreAnchorsSTH(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MustJournal(agent.NewMemStore())
	s := newP256Signer(t)
	anchorLog := audit.NewMemAnchorLog()
	as, err := audit.NewAuditedStore(j.Store(), s, anchorLog)
	if err != nil {
		t.Fatal(err)
	}
	var ts int64
	store := agenttest.MustJournal(as.WithClock(func() int64 { ts++; return ts }))
	tool := agent.MustFunc("lookup", "", func(_ context.Context, _ struct{}) (string, error) { return "ok", nil }, agent.WithSafety(agent.Safety{ReadOnly: true}))
	if _, err := agenttest.MustNew(&twoTurnModel{}, store, agent.WithTools(tool)).Run(ctx, "run", agent.UserText("hi")); err != nil {
		t.Fatal(err)
	}
	v, _ := audit.VerifierOf(s)
	entries := anchorLog.Entries()
	if len(entries) == 0 {
		t.Fatal("nothing anchored")
	}
	for i, e := range entries {
		if e.STH.Alg != audit.AlgECDSAP256 || e.STH.Verify(v) != nil {
			t.Fatalf("entry %d: alg %q verify %v", i, e.STH.Alg, e.STH.Verify(v))
		}
	}
	// Proof bundles for the run verify under the P-256 key.
	recs, _ := j.History(ctx, "run")
	last := entries[len(entries)-1]
	pb, err := audit.ProveRecord(ctx, j, "run", len(recs)-1, last.STH)
	if err != nil {
		t.Fatal(err)
	}
	if err := pb.Verify(v); err != nil {
		t.Fatalf("proof bundle under P-256: %v", err)
	}
}
