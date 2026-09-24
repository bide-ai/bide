package govern_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"

	gsm "github.com/blackwell-systems/gsm"
	agent "github.com/dayna/go-agents"
	"github.com/dayna/go-agents/audit"
	"github.com/dayna/go-agents/govern"
)

// buildGoverned returns a compensation-bearing machine: an approval that must be reverted when the
// case is flagged. The repair fires, so it is strictly beyond the CRDT fragment.
func buildGoverned(t *testing.T) (*gsm.Report, string) {
	t.Helper()
	r := gsm.NewRegistry("kyc-decision")
	approved := r.Bool("approved")
	flag := r.Bool("flagged")
	r.Rule("no_approve_when_flagged").
		Require(gsm.Or(gsm.Is(approved, 0), gsm.Is(flag, 0))).
		RepairWith(gsm.SetTo(approved, 0)).
		Add()
	r.On("flag").Does(gsm.SetTo(flag, 1)).Add()
	r.On("approve").Does(gsm.SetTo(approved, 1)).Add()
	_, rep, err := r.Build()
	if err != nil {
		t.Fatalf("build governed: %v\n%s", err, rep)
	}
	digest, err := r.PolicyDigest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	return rep, digest
}

// buildCRDT returns a compensation-free machine: two independent flags with no invariant to
// violate, so normalization never repairs. This is the CRDT fragment.
func buildCRDT(t *testing.T) (*gsm.Report, string) {
	t.Helper()
	r := gsm.NewRegistry("dual-flag")
	a := r.Bool("a")
	b := r.Bool("b")
	r.On("set_a").Does(gsm.SetTo(a, 1)).Add()
	r.On("set_b").Does(gsm.SetTo(b, 1)).Add()
	_, rep, err := r.Build()
	if err != nil {
		t.Fatalf("build crdt: %v\n%s", err, rep)
	}
	digest, err := r.PolicyDigest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	return rep, digest
}

// TestCertifyConvergence_Classifies confirms the certificate reports convergence for both machines
// and correctly separates the compensation-bearing (governed) machine from the compensation-free
// (CRDT) one, which is the CRDT.v subsumption result made visible at the SDK boundary.
func TestCertifyConvergence_Classifies(t *testing.T) {
	govRep, govDigest := buildGoverned(t)
	crdtRep, crdtDigest := buildCRDT(t)

	gc := govern.CertifyConvergence(govRep, govDigest)
	cc := govern.CertifyConvergence(crdtRep, crdtDigest)

	if !gc.Converges || !cc.Converges {
		t.Fatalf("both machines built, so both must certify convergent: governed=%v crdt=%v", gc.Converges, cc.Converges)
	}
	if gc.CompensationFree {
		t.Fatalf("governed machine has a repair (MaxRepairLen=%d) so it must not be classified compensation-free", gc.MaxRepairLen)
	}
	if gc.MaxRepairLen == 0 {
		t.Fatalf("governed machine should have a positive max repair depth, got 0")
	}
	if !cc.CompensationFree {
		t.Fatalf("crdt machine never repairs (MaxRepairLen=%d) so it must be classified compensation-free", cc.MaxRepairLen)
	}
	if cc.MaxRepairLen != 0 {
		t.Fatalf("crdt machine should have max repair depth 0, got %d", cc.MaxRepairLen)
	}
	if !gc.CC || !cc.CC {
		t.Fatalf("both machines built, so CC must hold: governed=%v crdt=%v", gc.CC, cc.CC)
	}
}

// TestConvergenceCertificate_AnchorsAndProves confirms the certificate rides the same Merkle rails
// as the policy leaf: it can be committed under a signed tree head and proven included, so an
// auditor verifies that the anchored policy was certified convergent in the same committed tree.
func TestConvergenceCertificate_AnchorsAndProves(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	const runID = "run1"

	rep, digest := buildGoverned(t)
	cert := govern.CertifyConvergence(rep, digest)
	certBytes, err := cert.Marshal()
	if err != nil {
		t.Fatalf("marshal certificate: %v", err)
	}

	if _, err := audit.RecordConvergence(ctx, store, runID, certBytes, digest); err != nil {
		t.Fatalf("RecordConvergence: %v", err)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	th, err := audit.NewTreeHead(ctx, store, runID, 1)
	if err != nil {
		t.Fatalf("NewTreeHead: %v", err)
	}
	sth := audit.SignTreeHead(th, priv)

	pb, err := audit.ProveConvergence(ctx, store, runID, digest, sth)
	if err != nil {
		t.Fatalf("ProveConvergence: %v", err)
	}
	if ok, err := pb.Verify(pub); err != nil || !ok {
		t.Fatalf("convergence bundle did not verify: ok=%v err=%v", ok, err)
	}

	// The anchored certificate says what it should: convergent, and for this policy digest.
	var content audit.ConvergenceContent
	if err := json.Unmarshal(pb.Record.Result, &content); err != nil {
		t.Fatalf("convergence content: %v", err)
	}
	if content.Digest != digest {
		t.Fatalf("anchored digest %q != policy digest %q", content.Digest, digest)
	}
	var got govern.ConfluenceCertificate
	if err := json.Unmarshal(content.Certificate, &got); err != nil {
		t.Fatalf("certificate decode: %v", err)
	}
	if !got.Converges || got.PolicyDigest != digest {
		t.Fatalf("anchored certificate mismatch: %+v", got)
	}
}

// TestRecordConvergence_Idempotent confirms recording the same certificate twice yields one leaf.
func TestRecordConvergence_Idempotent(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	const runID = "run1"
	rep, digest := buildCRDT(t)
	certBytes, err := govern.CertifyConvergence(rep, digest).Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := audit.RecordConvergence(ctx, store, runID, certBytes, digest); err != nil {
		t.Fatalf("RecordConvergence 1: %v", err)
	}
	if _, err := audit.RecordConvergence(ctx, store, runID, certBytes, digest); err != nil {
		t.Fatalf("RecordConvergence 2: %v", err)
	}
	recs, err := store.History(ctx, runID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	n := 0
	for _, r := range recs {
		if r.Kind == agent.StepValue {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly one convergence leaf, got %d", n)
	}
}
