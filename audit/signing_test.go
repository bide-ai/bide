package audit

import (
	"crypto/ed25519"
	"crypto/mldsa"
	"crypto/rand"
	"testing"
)

func testTreeHead() TreeHead {
	return TreeHead{Kind: TreeJournal, RunID: "run", Size: 3, Root: []byte("0123456789abcdef0123456789abcdef"), Timestamp: 42}
}

// TestSigningSchemes exercises ed25519, ML-DSA-65, and hybrid over a tree head: each signer's
// STH verifies under its matching verifier and is rejected under a mismatched scheme or a
// tampered tree head.
func TestSigningSchemes(t *testing.T) {
	edPub, edPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519 keygen: %v", err)
	}
	mlPriv, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatalf("mldsa keygen: %v", err)
	}
	mlPub := mlPriv.PublicKey()

	cases := []struct {
		name   string
		alg    string
		signer Signer
		verify Verifier
	}{
		{"ed25519", AlgEd25519, Ed25519Signer{edPriv}, Ed25519Verifier{edPub}},
		{"mldsa", AlgMLDSA65, MLDSASigner{mlPriv}, MLDSAVerifier{mlPub}},
		{"hybrid", AlgHybrid,
			HybridSigner{Ed25519Signer{edPriv}, MLDSASigner{mlPriv}},
			HybridVerifier{Ed25519Verifier{edPub}, MLDSAVerifier{mlPub}}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sth, err := SignTreeHeadWith(testTreeHead(), c.signer)
			if err != nil {
				t.Fatalf("sign: %v", err)
			}
			if sth.Alg != c.alg {
				t.Fatalf("alg = %q, want %q", sth.Alg, c.alg)
			}
			if !sth.VerifyWith(c.verify) {
				t.Fatalf("%s STH did not verify under its own verifier", c.name)
			}
			// A tampered tree head must not verify.
			bad := sth
			bad.Timestamp = 43
			if bad.VerifyWith(c.verify) {
				t.Fatalf("%s: tampered STH verified", c.name)
			}
			// A different scheme's verifier must be rejected on algorithm mismatch.
			other := Ed25519Verifier{edPub}
			if c.alg != AlgEd25519 && sth.VerifyWith(other) {
				t.Fatalf("%s STH verified under an ed25519 verifier", c.name)
			}
		})
	}
}

// TestSigning_BackwardCompatible confirms the ed25519-only SignTreeHead / Verify path is
// unchanged: it leaves Alg empty, and an empty-Alg STH still verifies via VerifyWith(ed25519).
func TestSigning_BackwardCompatible(t *testing.T) {
	edPub, edPriv, _ := ed25519.GenerateKey(rand.Reader)
	sth := SignTreeHead(testTreeHead(), edPriv)
	if sth.Alg != "" {
		t.Fatalf("legacy SignTreeHead should leave Alg empty, got %q", sth.Alg)
	}
	if !sth.Verify(edPub) {
		t.Fatalf("legacy Verify failed")
	}
	if !sth.VerifyWith(Ed25519Verifier{edPub}) {
		t.Fatalf("empty-Alg STH should verify as ed25519 via VerifyWith")
	}
}
