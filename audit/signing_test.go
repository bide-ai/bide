package audit

import (
	"crypto/ed25519"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"slices"
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

// KeyIDs identify a key by its bytes: two verifiers over one key agree, distinct keys differ,
// a hybrid lists both components, and a key that verifies nothing reports no identity.
func TestVerifierKeyIDs(t *testing.T) {
	edPub, _, _ := ed25519.GenerateKey(rand.Reader)
	edPub2, _, _ := ed25519.GenerateKey(rand.Reader)
	mlPriv, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	mlPub := mlPriv.PublicKey()
	sum := sha256.Sum256(edPub)
	if got, want := (Ed25519Verifier{edPub}).KeyIDs(), []string{"ed25519:" + hex.EncodeToString(sum[:])}; !slices.Equal(got, want) {
		t.Fatalf("Ed25519Verifier.KeyIDs = %v, want %v", got, want)
	}
	if a, b := (Ed25519Verifier{edPub}).KeyIDs(), (Ed25519Verifier{slices.Clone(edPub)}).KeyIDs(); !slices.Equal(a, b) {
		t.Fatalf("one key, two verifiers: %v != %v", a, b)
	}
	if a, b := (Ed25519Verifier{edPub}).KeyIDs(), (Ed25519Verifier{edPub2}).KeyIDs(); slices.Equal(a, b) {
		t.Fatalf("distinct keys share an identity: %v", a)
	}
	parsed, err := mldsa.NewPublicKey(mldsa.MLDSA65(), mlPub.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	ml := MLDSAVerifier{mlPub}.KeyIDs()
	if !slices.Equal(ml, MLDSAVerifier{parsed}.KeyIDs()) || len(ml) != 1 || ml[0] != KeyID(AlgMLDSA65, mlPub.Bytes()) {
		t.Fatalf("MLDSAVerifier.KeyIDs = %v, want one identity from the key's encoding", ml)
	}
	hy := HybridVerifier{Ed25519Verifier{edPub}, MLDSAVerifier{mlPub}}.KeyIDs()
	if want := append((Ed25519Verifier{edPub}).KeyIDs(), ml...); !slices.Equal(hy, want) {
		t.Fatalf("HybridVerifier.KeyIDs = %v, want both components %v", hy, want)
	}
	for name, v := range map[string]interface{ KeyIDs() []string }{
		"short ed25519":       Ed25519Verifier{edPub[:31]},
		"nil ed25519":         Ed25519Verifier{},
		"nil ml-dsa":          MLDSAVerifier{},
		"hybrid, bad ed25519": HybridVerifier{Ed25519Verifier{}, MLDSAVerifier{mlPub}},
		"hybrid, nil ml-dsa":  HybridVerifier{Ed25519Verifier{edPub}, MLDSAVerifier{}},
	} {
		if got := v.KeyIDs(); got != nil {
			t.Fatalf("%s: KeyIDs = %v, want none", name, got)
		}
	}
}
