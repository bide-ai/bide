package audit_test

// Tests from the adversarial review of #109 (one seat per signing key) that need no curve
// arithmetic: the ML-DSA key identity's label, and a hybrid sharing a component with a
// standalone approver.

import (
	"crypto/ed25519"
	"crypto/mldsa"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

func resolverOf(m map[string]agent.ApproverVerifier) agent.ApproverVerifierFor {
	return func(id string) (agent.ApproverVerifier, bool) { v, ok := m[id]; return v, ok }
}

// An ML-DSA key identity names the parameter set it is for. MLDSAVerifier verifies ML-DSA-65 only
// (the scheme AlgMLDSA65 names), so a key of another parameter set verifies nothing and reports no
// identity, which an approval gate refuses, and an ML-DSA-65 key reports one identity labelled
// "ml-dsa-65". (#109 labelled each parameter set because its verifier accepted all three; under
// P11's scheme agility a verifier checks only its own scheme.)
func TestMLDSAKeyIDLabelsParameterSet(t *testing.T) {
	for _, c := range []struct {
		params mldsa.Parameters
		label  string
		usable bool
	}{{mldsa.MLDSA44(), "ml-dsa-44", false}, {mldsa.MLDSA65(), string(audit.AlgMLDSA65), true}, {mldsa.MLDSA87(), "ml-dsa-87", false}} {
		sk, err := mldsa.NewPrivateKey(c.params, make([]byte, 32))
		if err != nil {
			t.Fatal(err)
		}
		v := audit.MLDSAVerifier{Pub: sk.PublicKey()}
		ids := v.KeyIDs()
		if !c.usable {
			if ids != nil {
				t.Fatalf("an %s key, which the verifier does not verify under, reports %v, want none", c.label, ids)
			}
			continue
		}
		msg := []byte("m")
		sig, err := audit.MLDSASigner{Priv: sk}.Sign(msg)
		if err != nil || !v.Verify(msg, sig) {
			t.Fatalf("%s: setup: signature does not verify (%v)", c.label, err)
		}
		if len(ids) != 1 || !strings.HasPrefix(ids[0], c.label+":") || ids[0] != audit.KeyID(c.label, sk.PublicKey().Bytes()) {
			t.Fatalf("an %s key reports %v, want one identity labelled %q", c.label, ids, c.label)
		}
	}
}

// A hybrid whose Ed25519 component is also a standalone approver's key is refused.
func TestHybridComponentSharedWithStandaloneRefused(t *testing.T) {
	edPriv := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	edPub := edPriv.Public().(ed25519.PublicKey)
	sk, err := mldsa.NewPrivateKey(mldsa.MLDSA65(), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	hy := audit.HybridVerifier{Ed: audit.Ed25519Verifier{Pub: edPub}, ML: audit.MLDSAVerifier{Pub: sk.PublicKey()}}
	pol := agent.ApprovalPolicy{Need: 2, Approvers: []string{"a1", "a2"}}
	vf := resolverOf(map[string]agent.ApproverVerifier{"a1": hy, "a2": audit.Ed25519Verifier{Pub: edPub}})
	if err := pol.ValidateKeys(vf); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("ValidateKeys = %v, want ErrConfig", err)
	}
	// The private-key and seed forms are not a second identity for the same key: they verify
	// nothing and report none, so the gate refuses them.
	if ids := (audit.Ed25519Verifier{Pub: ed25519.PublicKey(edPriv)}).KeyIDs(); ids != nil {
		t.Fatalf("the 64-byte private form reports %v", ids)
	}
	seedV := audit.Ed25519Verifier{Pub: ed25519.PublicKey(edPriv.Seed())}
	if seedV.Verify([]byte("m"), ed25519.Sign(edPriv, []byte("m"))) {
		t.Fatal("the seed used as a public key verifies the key's signature")
	}
}
