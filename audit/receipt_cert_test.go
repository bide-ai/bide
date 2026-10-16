package audit_test

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/testca"
)

// A decision receipt of a certificate-bound decision carries the chain, so it verifies on its own: the
// signature over the v4 bytes (which bind the leaf's hash), and, given the gate's trust anchors, the
// chain and the identity (docs/design/approver-identity.md, "Decision receipts").

type certReceiptFx struct {
	g     *gate
	leaf  *testca.Leaf
	ci    *audit.CertIdentity
	chain [][]byte
	d     agent.Decision
	rec   agent.Record
	idx   int
}

// newCertReceiptFx is a one-approver gate ("alice") paused on c1, with a chain root -> inter -> leaf
// whose leaf key (Ed25519) signs a v4 decision at testDecidedAt.
func newCertReceiptFx(t *testing.T) *certReceiptFx {
	t.Helper()
	g := newGate(1, []string{"alice"}, []string{"alice"})
	wantPaused(t, g.run(), 0)
	root := testca.NewRoot(t, "root", testca.Opts{})
	inter := root.NewIntermediate(t, "inter", testca.Opts{})
	leaf := inter.NewLeaf(t, testca.Opts{Key: testca.Ed25519, Mutate: func(c *x509.Certificate) {
		c.NotBefore, c.NotAfter = testDecidedAt.Add(-time.Hour), testDecidedAt.Add(time.Hour)
	}})
	ci, err := audit.IdentityPolicy{
		Anchors: []*x509.Certificate{root.Cert}, Inter: []audit.SPKIHash{audit.SPKIHashOf(inter.Cert)},
		Identity: map[string]string{"alice": testca.Person(1)}, IDKind: audit.IDPersonURI, EKU: testca.EKU, Policy: testca.PolicyOID,
		Canonical: audit.NewCanonicalizer(audit.CanonicalConfig{Host: "idp.bank.example"}),
	}.Compile()
	if err != nil {
		t.Fatal(err)
	}
	f := &certReceiptFx{g: g, leaf: leaf, ci: ci, chain: leaf.Chain()}
	s := g.subject(t)
	sig := ed25519.Sign(leaf.Key.(ed25519.PrivateKey), agent.ApprovalDecisionBytesCert(s, "alice", true, testDecidedAt, agent.CertLeafHash(leaf.DER)))
	f.d = agent.Decision{RunID: gateRun, ToolUseID: "c1", ApproverID: "alice", Approved: true, DecidedAt: testDecidedAt,
		Alg: audit.AlgEd25519, Signature: sig, Cert: f.chain}
	if err := agent.SubmitDecision(context.Background(), g.store, f.d, agent.WithClock(func() time.Time { return receiptRecordedAt })); err != nil {
		t.Fatal(err)
	}
	for i, r := range journalOf(t, g) {
		if r.Kind == agent.StepApproval && r.Approver() == "alice" {
			f.rec, f.idx = r, i
		}
	}
	return f
}

func (f *certReceiptFx) receipt(t *testing.T) audit.DecisionReceipt {
	t.Helper()
	rc, err := audit.NewDecisionReceipt(f.g.subject(t), f.d, f.rec, f.idx)
	if err != nil {
		t.Fatal(err)
	}
	return rc
}

func leafVerifier(t *testing.T, f *certReceiptFx) audit.Verifier {
	t.Helper()
	v, err := audit.NewVerifier(audit.AlgEd25519, f.leaf.Cert.PublicKey.(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestCertReceipt_CarriesTheChainAndVerifies(t *testing.T) {
	f := newCertReceiptFx(t)
	rc := f.receipt(t)
	if len(rc.Cert) != 2 || string(rc.Cert[0]) != string(f.leaf.DER) {
		t.Fatalf("receipt chain has %d certificates", len(rc.Cert))
	}
	// The arguments digest is the same one a bare-key receipt of this call carries.
	if rc.ArgsSHA != audit.ApprovalArgsDigest(f.g.subject(t)) {
		t.Fatal("args digest differs")
	}
	// It round-trips as JSON with the chain in "cert", and a bare-key receipt has no such member.
	b, err := json.Marshal(rc)
	if err != nil || !strings.Contains(string(b), `"cert":[`) {
		t.Fatalf("%v %s", err, b)
	}
	var back audit.DecisionReceipt
	if err := json.Unmarshal(b, &back); err != nil || len(back.Cert) != 2 {
		t.Fatalf("%v", err)
	}
	if err := back.Verify(leafVerifier(t, f)); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	bare := newReceiptWorld(t)
	if bb, _ := json.Marshal(bare.receipt); strings.Contains(string(bb), `"cert"`) {
		t.Fatalf("a bare-key receipt carries a cert member: %s", bb)
	}
}

func TestCertReceipt_VerifyRefusesWhatIsNotTheSignedCertificate(t *testing.T) {
	f := newCertReceiptFx(t)
	v := leafVerifier(t, f)
	// The chain stripped: the signature is over v4 bytes, which v3 bytes cannot reproduce.
	stripped := f.receipt(t)
	stripped.Cert = nil
	if err := stripped.Verify(v); !errors.Is(err, audit.ErrNotVerified) {
		t.Fatalf("stripped: %v", err)
	}
	// Another certificate for the same key swapped in: the leaf hash differs.
	other := testca.NewRoot(t, "root2", testca.Opts{}).NewLeaf(t, testca.Opts{KeyWith: f.leaf.Key})
	swapped := f.receipt(t)
	swapped.Cert = other.Chain()
	if err := swapped.Verify(v); !errors.Is(err, audit.ErrNotVerified) {
		t.Fatalf("swapped: %v", err)
	}
	// A v3 signature with a chain attached is not a certificate-bound decision.
	g := newGate(1, []string{"alice"}, []string{"alice"})
	wantPaused(t, g.run(), 0)
	rc := decideReceipt(t, g, "alice")
	rc.Cert = f.chain
	pub := g.pubs["alice"]
	if err := rc.Verify(audit.Ed25519Verifier{Pub: pub}); !errors.Is(err, audit.ErrNotVerified) {
		t.Fatalf("v3 with a chain: %v", err)
	}
}

func TestCertReceipt_VerifyCertificate(t *testing.T) {
	f := newCertReceiptFx(t)
	rc := f.receipt(t)
	a := testDecidedAt.Add(time.Second)
	if err := rc.VerifyCertificate(f.ci, a); err != nil {
		t.Fatalf("VerifyCertificate: %v", err)
	}
	want := func(name string, err error, reason string) {
		t.Helper()
		var ce *audit.CertError
		if err == nil || !errors.Is(err, audit.ErrNotVerified) || !errors.As(err, &ce) || ce.Reason != reason {
			t.Fatalf("%s: %v", name, err)
		}
	}
	stripped := rc
	stripped.Cert = nil
	want("stripped chain", stripped.VerifyCertificate(f.ci, a), agent.ReasonNoCert)
	// Not this person's certificate.
	mallory := rc
	mallory.Approver = "mallory"
	if err := mallory.VerifyCertificate(f.ci, a); err == nil || !errors.Is(err, audit.ErrNotVerified) {
		t.Fatalf("an approver with no identity: %v", err)
	}
	// A chain to an anchor the gate does not trust.
	otherRoot := testca.NewRoot(t, "other", testca.Opts{})
	oi := otherRoot.NewIntermediate(t, "inter", testca.Opts{})
	foreign := rc
	foreign.Cert = oi.NewLeaf(t, testca.Opts{KeyWith: f.leaf.Key}).Chain()
	want("foreign chain", foreign.VerifyCertificate(f.ci, a), agent.ReasonCertChain)
	// The time it became provable is outside the certificate's validity.
	want("too late", rc.VerifyCertificate(f.ci, testDecidedAt.Add(3*time.Hour)), agent.ReasonCertWindow)
	// The receipt's scheme must be the certificate key's.
	confused := rc
	confused.Alg = audit.AlgECDSAP256
	if err := confused.VerifyCertificate(f.ci, a); !errors.Is(err, audit.ErrNotVerified) {
		t.Fatalf("algorithm confusion: %v", err)
	}
	// A signature that is not the leaf key's.
	forged := rc
	forged.Signature = append([]byte{}, rc.Signature...)
	forged.Signature[0] ^= 0xff
	if err := forged.VerifyCertificate(f.ci, a); !errors.Is(err, audit.ErrNotVerified) {
		t.Fatalf("forged signature: %v", err)
	}
	// No policy: a configuration error, not a verdict.
	if err := rc.VerifyCertificate(nil, a); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("nil policy: %v", err)
	}
}

// The anchoring proof is the smallest covering head, and its time is what the certificate check uses.
func TestCertReceipt_AnchorTime(t *testing.T) {
	f := newCertReceiptFx(t)
	rc := f.receipt(t)
	if _, err := rc.AnchorTime(audit.Ed25519Verifier{Pub: f.g.logPub}); err == nil {
		t.Fatal("a receipt with no proof has an anchor time")
	}
	h1 := headAt(t, f.g, f.g.logPriv, receiptRecordedAt.Add(time.Minute))
	anchored, err := audit.AnchorReceipt(rc, journalOf(t, f.g), []audit.SignedTreeHead{h1}, audit.Ed25519Verifier{Pub: f.g.logPub})
	if err != nil {
		t.Fatal(err)
	}
	at, err := anchored.AnchorTime(audit.Ed25519Verifier{Pub: f.g.logPub})
	if err != nil || !at.Equal(receiptRecordedAt.Add(time.Minute)) {
		t.Fatalf("anchor time %v, %v", at, err)
	}
	if len(anchored.Cert) != 2 {
		t.Fatal("anchoring dropped the chain")
	}
	// A different key does not vouch for the proof.
	if _, err := anchored.AnchorTime(audit.Ed25519Verifier{Pub: f.leaf.Cert.PublicKey.(ed25519.PublicKey)}); err == nil {
		t.Fatal("anchor time under the wrong log key")
	}
}

// A receipt is of one record: the same decision journaled under another chain (or none) is not it.
func TestCertReceipt_RefusesARecordWithAnotherChain(t *testing.T) {
	f := newCertReceiptFx(t)
	other := testca.NewRoot(t, "root2", testca.Opts{}).NewLeaf(t, testca.Opts{KeyWith: f.leaf.Key}).Chain()
	for name, chain := range map[string][][]byte{
		"another leaf":  other,
		"no chain":      nil,
		"a longer one":  append(append([][]byte{}, f.chain...), f.chain[0]),
		"a shorter one": f.chain[:1],
	} {
		d := f.d
		d.Cert = chain
		if _, err := audit.NewDecisionReceipt(f.g.subject(t), d, f.rec, f.idx); !errors.Is(err, agent.ErrConfig) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
