package audit_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/x509"
	"errors"
	"testing"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/testca"
)

// CertDecisionKey says which decision scheme a leaf's key has and gives the key in the encoding a signer
// reports, so a command can compare them before it signs.
func TestCertDecisionKey(t *testing.T) {
	root := testca.NewRoot(t, "root", testca.Opts{})
	for _, tc := range []struct {
		name string
		kind testca.KeyKind
		alg  audit.Alg
		ok   bool
	}{
		{"ed25519", testca.Ed25519, audit.AlgEd25519, true},
		{"ecdsa-p256", testca.P256, audit.AlgECDSAP256, true},
		{"ml-dsa-65", testca.MLDSA65, audit.AlgMLDSA65, true},
		{"ecdsa-p384", testca.P384, "", false},
		{"rsa", testca.RSA2048, "", false},
	} {
		leaf := root.NewLeaf(t, testca.Opts{Key: tc.kind})
		alg, key, err := audit.CertDecisionKey(leaf.Cert)
		if (err == nil) != tc.ok || alg != tc.alg {
			t.Errorf("%s: %q %v", tc.name, alg, err)
			continue
		}
		if !tc.ok {
			var ce *audit.CertError
			if !errors.As(err, &ce) {
				t.Errorf("%s: %T", tc.name, err)
			}
			continue
		}
		// The bytes are what NewVerifier reads, and what the key signs for: a signature by the leaf's own key
		// verifies under the verifier made from them.
		if _, err := audit.NewVerifier(alg, key); err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		switch k := leaf.Cert.PublicKey.(type) {
		case ed25519.PublicKey:
			if !bytes.Equal(key, k) {
				t.Errorf("%s: key bytes", tc.name)
			}
		case *ecdsa.PublicKey:
			if b, _ := k.Bytes(); !bytes.Equal(key, b) {
				t.Errorf("%s: key bytes", tc.name)
			}
		}
	}
	if _, _, err := audit.CertDecisionKey(nil); err == nil {
		t.Error("a nil certificate")
	}
	_ = x509.Certificate{}
}
