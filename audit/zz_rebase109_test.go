package audit_test

import (
	"errors"
	"testing"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/audit/verify"
)

// Rebasing #105 onto #109 put #109's weak-key rule on #105's Verifier API: a weak Ed25519 key is
// not a verification key, so neither constructor returns a verifier for one, alone or as a hybrid
// component, and the text form is refused the same way (ErrWeakKey and ErrMalformed).
func TestRebase109_ConstructorsRefuseWeakEd25519Keys(t *testing.T) {
	signers := p11Signers(t)
	ml := signers["ml-dsa-65"].PublicKey()
	for i, k := range identityEncodings() {
		if _, err := audit.NewVerifier(audit.AlgEd25519, k); !errors.Is(err, audit.ErrWeakKey) || !errors.Is(err, audit.ErrMalformed) {
			t.Errorf("identity encoding %d: NewVerifier(ed25519) err %v, want ErrWeakKey and ErrMalformed", i, err)
		}
		if _, err := audit.ParsePublicKey(audit.FormatPublicKey(audit.AlgEd25519, k)); !errors.Is(err, audit.ErrWeakKey) {
			t.Errorf("identity encoding %d: ParsePublicKey err %v, want ErrWeakKey", i, err)
		}
		hybrid := append(append([]byte{0, 0, 0, byte(len(k))}, k...), ml...)
		if _, err := audit.NewVerifier(audit.AlgHybrid, hybrid); !errors.Is(err, audit.ErrWeakKey) {
			t.Errorf("identity encoding %d: NewVerifier(hybrid) err %v, want ErrWeakKey", i, err)
		}
		if _, err := verify.NewVerifier("ed25519", k); err == nil {
			t.Errorf("identity encoding %d: verify.NewVerifier accepted the key", i)
		}
		if _, err := verify.NewVerifier("ed25519+ml-dsa-65", hybrid); err == nil {
			t.Errorf("identity encoding %d: verify.NewVerifier accepted a hybrid with the key", i)
		}
	}
	// A usable key still makes a verifier under both constructors.
	ed := signers["ed25519"].PublicKey()
	if _, err := audit.NewVerifier(audit.AlgEd25519, ed); err != nil {
		t.Fatal(err)
	}
	if _, err := verify.NewVerifier("ed25519", ed); err != nil {
		t.Fatal(err)
	}
}
