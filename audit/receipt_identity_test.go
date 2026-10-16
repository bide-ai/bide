package audit_test

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/policyfile"
	"github.com/bide-ai/bide/internal/testca"
)

// anchoredCertReceipt is the fixture's receipt with its anchoring proof under a head stamped a minute
// after the decision was signed, and the log key that vouches for it.
func anchoredCertReceipt(t *testing.T, f *certReceiptFx) (audit.DecisionReceipt, audit.Verifier) {
	t.Helper()
	logKey := audit.Ed25519Verifier{Pub: f.g.logPub}
	h := headAt(t, f.g, f.g.logPriv, testDecidedAt.Add(time.Minute))
	rc, err := audit.AnchorReceipt(f.receipt(t), journalOf(t, f.g), []audit.SignedTreeHead{h}, logKey)
	if err != nil {
		t.Fatal(err)
	}
	return rc, logKey
}

func TestReceiptVerifyIdentity(t *testing.T) {
	f := newCertReceiptFx(t)
	rc, logKey := anchoredCertReceipt(t, f)
	got, err := rc.VerifyIdentity(f.ci, logKey)
	if err != nil {
		t.Fatalf("VerifyIdentity: %v", err)
	}
	if got.Identifier != testca.Person(1) || got.Approver != "alice" {
		t.Fatalf("identity %+v", got)
	}
	if !got.AnchoredAt.Equal(testDecidedAt.Add(time.Minute)) || got.TimeSource != "log-signer clock" {
		t.Fatalf("anchor %v from %q", got.AnchoredAt, got.TimeSource)
	}
	lh := agent.CertLeafHash(f.leaf.DER)
	if got.LeafHash != hex.EncodeToString(lh[:]) || len(got.AnchorKey) != 64 || len(got.Chain) != 3 {
		t.Fatalf("hashes %+v", got)
	}
	// The status was not looked at: the result says so rather than leaving it to the caller.
	if got.Revocation != "not checked" {
		t.Fatalf("revocation %q", got.Revocation)
	}
}

func TestReceiptVerifyIdentityRefusals(t *testing.T) {
	f := newCertReceiptFx(t)
	rc, logKey := anchoredCertReceipt(t, f)
	noProof := f.receipt(t)
	if _, err := noProof.VerifyIdentity(f.ci, logKey); !errors.Is(err, audit.ErrNotVerified) || !strings.Contains(err.Error(), "anchoring proof") {
		t.Fatalf("no proof: %v", err)
	}
	if _, err := rc.VerifyIdentity(f.ci, audit.Ed25519Verifier{Pub: f.leaf.Cert.PublicKey.(ed25519.PublicKey)}); err == nil {
		t.Fatal("wrong log key")
	}
}

// writePolicy writes the fixture's trust anchors and a policy file for it into a directory.
func writePolicy(t *testing.T, root *testca.CA, inter *testca.CA, edit func(m map[string]any)) string {
	t.Helper()
	dir := t.TempDir()
	pemPath := filepath.Join(dir, "root.pem")
	if err := os.WriteFile(pemPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Cert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	spki := audit.SPKIHashOf(inter.Cert)
	m := map[string]any{
		"anchors":       []string{"root.pem"},
		"intermediates": []string{hex.EncodeToString(spki[:])},
		"identity":      map[string]string{"alice": testca.Person(1)},
		"id_kind":       "person-uri",
		"eku":           testca.EKU.String(),
		"policy_oid":    testca.PolicyOID.String(),
		"canonical":     map[string]string{"host": "idp.bank.example"},
	}
	if edit != nil {
		edit(m)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadIdentityPolicy(t *testing.T) {
	root := testca.NewRoot(t, "root", testca.Opts{})
	inter := root.NewIntermediate(t, "inter", testca.Opts{})
	path := writePolicy(t, root, inter, nil)
	ci, err := policyfile.Read(path)
	if err != nil {
		t.Fatalf("ReadIdentityPolicy: %v", err)
	}
	if id, ok := ci.Identity("alice"); !ok || id != testca.Person(1) {
		t.Fatalf("identity %q %v", id, ok)
	}
	// The file's policy accepts what the same policy built in Go accepts.
	at := testDecidedAt
	leaf := inter.NewLeaf(t, testca.Opts{Key: testca.Ed25519, Mutate: func(c *x509.Certificate) {
		c.NotBefore, c.NotAfter = at.Add(-time.Hour), at.Add(time.Hour)
	}})
	if _, err := ci.VerifyChain("alice", leaf.Chain(), at, at); err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
}

func TestReadIdentityPolicyRefusals(t *testing.T) {
	root := testca.NewRoot(t, "root", testca.Opts{})
	inter := root.NewIntermediate(t, "inter", testca.Opts{})
	cases := map[string]func(m map[string]any){
		"unknown field":  func(m map[string]any) { m["trust_system_roots"] = true },
		"no anchors":     func(m map[string]any) { m["anchors"] = []string{} },
		"missing anchor": func(m map[string]any) { m["anchors"] = []string{"nope.pem"} },
		"bad id kind":    func(m map[string]any) { m["id_kind"] = "phone" },
		"bad eku":        func(m map[string]any) { m["eku"] = "not-an-oid" },
		"bad pin":        func(m map[string]any) { m["intermediates"] = []string{"abcd"} },
		"bad duration":   func(m map[string]any) { m["skew"] = "soon" },
		"distrust of nobody": func(m map[string]any) {
			m["distrust"] = map[string]string{strings.Repeat("ab", 32): "2026-01-01T00:00:00Z"}
		},
		"no identity map":    func(m map[string]any) { m["identity"] = map[string]string{} },
		"anchor path escape": func(m map[string]any) { m["anchors"] = []string{"../root.pem"} },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := policyfile.Read(writePolicy(t, root, inter, edit))
			if err == nil || !errors.Is(err, agent.ErrConfig) {
				t.Fatalf("want ErrConfig, got %v", err)
			}
		})
	}
	if _, err := policyfile.Read(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("a missing policy file loaded")
	}
}
