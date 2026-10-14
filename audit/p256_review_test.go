package audit_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

func p256Decision(t *testing.T, g *gate, id string) agent.Decision {
	t.Helper()
	msg := agent.ApprovalDecisionBytes(g.subject(t), id, true, testDecidedAt)
	sig, err := g.p256[id].Sign(msg)
	if err != nil {
		t.Fatal(err)
	}
	return agent.Decision{RunID: gateRun, ToolUseID: "c1", ApproverID: id, Approved: true, DecidedAt: testDecidedAt, Alg: audit.AlgECDSAP256, Signature: sig}
}

func countDecisionRecords(t *testing.T, g *gate) int {
	t.Helper()
	recs, _ := g.store.History(context.Background(), gateRun)
	n := 0
	for _, r := range recs {
		if agent.IsApprovalDecision(r, "c1") {
			n++
		}
	}
	return n
}

// Retrying the identical decision after a lost response: with a randomized scheme the retry is a
// new record. Without a check both are journaled; the gate counts the approver once.
func TestReviewSF_ResignedIdenticalDecision_NoCheck(t *testing.T) {
	g := newGate(2, []string{"alice", "bob"}, []string{"alice", "bob"})
	g.p256 = map[string]audit.ECDSAP256Signer{"alice": newP256Signer(t), "bob": newP256Signer(t)}
	wantPaused(t, g.run(), 0)
	for i := 0; i < 2; i++ {
		if err := agent.SubmitDecision(context.Background(), g.store, p256Decision(t, g, "alice")); err != nil {
			t.Fatal(err)
		}
	}
	if n := countDecisionRecords(t, g); n != 2 {
		t.Fatalf("records = %d", n)
	}
	// alice counted once: still 1 of 2.
	wantPaused(t, g.run(), 1)
	if err := agent.SubmitDecision(context.Background(), g.store, p256Decision(t, g, "bob")); err != nil {
		t.Fatal(err)
	}
	if err := g.run(); err != nil {
		t.Fatal(err)
	}
	logS := newP256Signer(t)
	logV, _ := audit.VerifierOf(logS)
	pkg, err := audit.Evidence(context.Background(), g.store, gateRun, logS, 1700000000, audit.WithToolCall("c1"))
	if err != nil {
		t.Fatal(err)
	}
	acts, err := audit.ApprovalEvidence(context.Background(), g.store, gateRun, "c1", pkg.STH)
	if err != nil {
		t.Fatal(err)
	}
	pkg.Actions = append(pkg.Actions, acts[:len(acts)-1]...)
	if err := pkg.Seal(logS); err != nil {
		t.Fatal(err)
	}
	v, err := audit.VerifyApprovals(pkg.Actions, "c1", g.policy, g.resolver(), logV)
	t.Logf("verdict OK=%v counted=%v ignored=%+v problems=%v err=%v", v.OK, v.Counted, v.Ignored, v.Problems, err)
	if err != nil || !v.OK {
		t.Fatalf("a duplicate re-signed record makes the verdict fail: %+v %v", v, err)
	}
}

// With WithDecisionCheck, the retry of an identical decision is not a no-op for a randomized scheme.
func TestReviewSF_ResignedIdenticalDecision_WithCheck(t *testing.T) {
	g := newGate(2, []string{"alice", "bob"}, []string{"alice", "bob"})
	g.p256 = map[string]audit.ECDSAP256Signer{"alice": newP256Signer(t), "bob": newP256Signer(t)}
	wantPaused(t, g.run(), 0)
	chk := agent.WithDecisionCheck(g.resolver())
	if err := agent.SubmitDecision(context.Background(), g.store, p256Decision(t, g, "alice"), chk); err != nil {
		t.Fatal(err)
	}
	err := agent.SubmitDecision(context.Background(), g.store, p256Decision(t, g, "alice"), chk)
	if errors.Is(err, agent.ErrAlreadyDecided) {
		t.Fatalf("retry of the identical decision (documented as a no-op) = %v", err)
	}
}

// The documented seal message length (54 bytes) is wrong: the tag is 28 bytes and the digest 32.

// A private key whose public half is not D*G must not sign: it would produce anchors that verify
// under no key. (Ed25519Signer has checkPrivateKey for the same reason.)
func TestReviewSF_P256MismatchedKeyHalves(t *testing.T) {
	a, b := newP256Signer(t), newP256Signer(t)
	bad := audit.ECDSAP256Signer{Priv: &ecdsa.PrivateKey{PublicKey: a.Priv.PublicKey, D: b.Priv.D}}
	sig, err := bad.Sign([]byte("m"))
	if err == nil {
		v, _ := audit.VerifierOf(bad)
		if !v.Verify([]byte("m"), sig) {
			t.Fatal("mismatched key halves sign without error, and the signature verifies under no key")
		}
	}
}

func TestReviewSF_BareHexNeverP256(t *testing.T) {
	s := newP256Signer(t)
	hexKey := hex.EncodeToString(s.PublicKey())
	if _, err := audit.ParsePublicKey(hexKey); err == nil {
		t.Fatal("bare 65-byte hex parsed")
	}
	if _, err := audit.ParsePublicKey("ecdsa-p256:" + hexKey[2:]); err == nil {
		t.Fatal("key without 04 prefix parsed")
	}
	// An Ed25519 key text under the ecdsa scheme, and a P-256 point under ed25519.
	if _, err := audit.ParsePublicKey("ed25519:" + hexKey); err == nil {
		t.Fatal("P-256 point read as ed25519")
	}
}

// Cross-scheme signatures: P-256 sig under Ed25519 key and the reverse.
func TestReviewSF_CrossSchemeSignatures(t *testing.T) {
	p := newP256Signer(t)
	pv, _ := audit.VerifierOf(p)
	edpub, edpriv, _ := ed25519.GenerateKey(rand.Reader)
	ev := audit.Ed25519Verifier{Pub: edpub}
	m := []byte("message")
	ps, _ := p.Sign(m)
	es := ed25519.Sign(edpriv, m)
	if ev.Verify(m, ps) || pv.Verify(m, es) {
		t.Fatal("cross-scheme signature verified")
	}
	// KeyID prefixes keep schemes apart even when the key bytes were the same.
	if audit.KeyID("ed25519", p.PublicKey()) == audit.KeyID("ecdsa-p256", p.PublicKey()) {
		t.Fatal("key id collision across schemes")
	}
}

// An evidence package re-encoded with reordered fields and whitespace still verifies (the seal is
// over the struct), and a relabel to v5 or to an unknown format does not.
func TestReviewSF_EvidenceCanonicalizationAndRelabel(t *testing.T) {
	ctx := context.Background()
	j, runID := buildBigRun(t, 3)
	s := newP256Signer(t)
	v, _ := audit.VerifierOf(s)
	pkg, err := audit.Evidence(ctx, j, runID, s, 1700000000)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(pkg)
	var m map[string]json.RawMessage
	json.Unmarshal(raw, &m)
	for _, f := range []string{"bide.audit.evidence.v5", "bide.audit.evidence.v4", "", "bide.audit.evidence.v7"} {
		m["format"], _ = json.Marshal(f)
		b, _ := json.Marshal(m)
		var got audit.EvidencePackage
		if err := audit.UnmarshalStrict(b, &got); err != nil {
			continue // refused at parse: fine
		}
		if rep, err := got.Verify(v); err == nil && rep.OK {
			t.Fatalf("a v6 package relabelled %q verifies", f)
		}
	}
	// A duplicate key is refused by the strict parser.
	dup := bytes.Replace(raw, []byte(`"label"`), []byte(`"label":"x","label"`), 1)
	var got audit.EvidencePackage
	if bytes.Equal(dup, raw) {
		dup = bytes.Replace(raw, []byte(`{`), []byte(`{"run_id":"a",`), 1)
	}
	if err := audit.UnmarshalStrict(dup, &got); err == nil {
		t.Fatal("duplicate key accepted")
	}
}
