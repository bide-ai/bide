package audit

import (
	"bytes"
	"crypto/ed25519"
	"crypto/mldsa"
	"errors"
	"strings"
	"testing"
)

// A head names the scheme it is signed under, and Verify checks that scheme is the verifier's
// before it checks the signature: an ed25519 key's signature over the encoding of a head labelled
// ml-dsa-65 is not an ml-dsa-65 head, and an ed25519 verifier does not accept it as one either.
func TestSTH_VerifyRequiresTheVerifiersScheme(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	th := TreeHead{Kind: TreeJournal, RunID: "r", Size: 0, Root: make([]byte, 32), TimestampNanos: 1}
	mislabelled := SignedTreeHead{Format: STHFormat, TreeHead: th, Alg: AlgMLDSA65, Signature: ed25519.Sign(priv, th.canonical(AlgMLDSA65))}
	if err := mislabelled.Verify(Ed25519Verifier{Pub: pub}); !errors.Is(err, ErrNotVerified) {
		t.Fatalf("an ed25519 signature over an ml-dsa-65 head: err = %v, want ErrNotVerified", err)
	}
	// The scheme is in the signed bytes: the encodings of one head under two schemes differ.
	if bytes.Equal(th.canonical(AlgEd25519), th.canonical(AlgHybrid)) {
		t.Fatal("the signed encoding of a head does not depend on its scheme")
	}
	if !strings.HasPrefix(string(th.canonical(AlgEd25519)), "bide.audit.sth.v5\x00") {
		t.Fatal("the signed encoding of a head is not tagged bide.audit.sth.v5")
	}
}

// An ML-DSA signer holds an ML-DSA-65 key: a key of another parameter set signs nothing and names
// no public key, so no artifact claims ml-dsa-65 over an ML-DSA-44 or -87 signature.
func TestMLDSASigner_RequiresMLDSA65(t *testing.T) {
	for _, p := range []mldsa.Parameters{mldsa.MLDSA44(), mldsa.MLDSA87()} {
		k, err := mldsa.GenerateKey(p)
		if err != nil {
			t.Fatal(err)
		}
		s := MLDSASigner{Priv: k}
		if _, err := s.Sign([]byte("m")); err == nil {
			t.Fatalf("%s: an ml-dsa-65 signer signed with a %s key", p, p)
		}
		if s.PublicKey() != nil {
			t.Fatalf("%s: an ml-dsa-65 signer named a %s public key", p, p)
		}
		if (MLDSAVerifier{Pub: k.PublicKey()}).PublicKey() != nil {
			t.Fatalf("%s: an ml-dsa-65 verifier named a %s public key", p, p)
		}
	}
}

// A grant's canonical bytes are tagged bide.audit.grant.v2 and are what the issuer signs and the
// digest covers, so a signature over the grant's bare JSON (a v1 grant) does not verify.
func TestGrant_BytesAreTaggedV2(t *testing.T) {
	g := Grant{ID: "g", Issuer: "i", Subject: "s", NotAfterUnix: 9}
	if !bytes.HasPrefix(g.Bytes(), []byte("bide.audit.grant.v2\n{")) || !bytes.Contains(g.Bytes(), []byte(`"not_after_unix":9`)) {
		t.Fatalf("Bytes = %s", g.Bytes())
	}
	pub, priv, _ := ed25519.GenerateKey(nil)
	bare := bytes.TrimPrefix(g.Bytes(), []byte("bide.audit.grant.v2\n"))
	old := SignedGrant{Grant: g, Alg: AlgEd25519, Sig: ed25519.Sign(priv, bare)}
	if err := old.Verify(Ed25519Verifier{Pub: pub}); !errors.Is(err, ErrNotVerified) {
		t.Fatalf("a signature over the untagged grant: err = %v, want ErrNotVerified", err)
	}
}

// A linear head's signature is over "bide.audit.head.v1\x00" || head, never the bare head, so it
// cannot stand in for a signature over any other 32 bytes the key signs.
func TestSign_HeadIsDomainSeparated(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	head := bytes.Repeat([]byte{1}, 32)
	sig, err := Sign(head, Ed25519Signer{Priv: priv})
	if err != nil {
		t.Fatal(err)
	}
	if ed25519.Verify(pub, head, sig) {
		t.Fatal("the head signature is a signature over the bare head")
	}
	if err := VerifySignature(head, ed25519.Sign(priv, head), Ed25519Verifier{Pub: pub}); !errors.Is(err, ErrNotVerified) {
		t.Fatalf("a signature over the bare head: err = %v, want ErrNotVerified", err)
	}
	if err := VerifySignature(head, sig, Ed25519Verifier{Pub: pub}); err != nil {
		t.Fatal(err)
	}
}

// An anchor entry carries its format, and VerifyAnchorInclusion refuses one without it.
func TestAnchorEntry_FormatIsChecked(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	sth, err := SignTreeHead(TreeHead{Kind: TreeJournal, RunID: "r", Root: make([]byte, 32), TimestampNanos: 1}, Ed25519Signer{Priv: priv})
	if err != nil {
		t.Fatal(err)
	}
	l := NewMemAnchorLog()
	if err := l.Publish(t.Context(), "r", sth); err != nil {
		t.Fatal(err)
	}
	root, _ := l.Root()
	proof, _ := l.Prove(0)
	e := l.Entries()[0]
	if e.Format != AnchorEntryFormat {
		t.Fatalf("Publish wrote format %q", e.Format)
	}
	if err := VerifyAnchorInclusion(root, e, proof); err != nil {
		t.Fatal(err)
	}
	e.Format = ""
	if err := VerifyAnchorInclusion(root, e, proof); !errors.Is(err, ErrFormat) {
		t.Fatalf("an entry without a format: err = %v, want ErrFormat", err)
	}
}
