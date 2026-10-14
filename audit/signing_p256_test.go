package audit

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"math/big"
	"slices"
	"testing"
)

func newP256(t *testing.T) ECDSAP256Signer {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("p256 keygen: %v", err)
	}
	return ECDSAP256Signer{Priv: k}
}

func p256N() *big.Int { return elliptic.P256().Params().N }

// splitRS returns r and s of a 64-byte signature.
func splitRS(sig []byte) (r, s *big.Int) {
	return new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
}

func joinRS(r, s *big.Int) []byte {
	out := make([]byte, 64)
	r.FillBytes(out[:32])
	s.FillBytes(out[32:])
	return out
}

func TestP256_RoundTripAndShape(t *testing.T) {
	s := newP256(t)
	if s.Alg() != AlgECDSAP256 || AlgECDSAP256 != "ecdsa-p256" {
		t.Fatalf("alg = %q", s.Alg())
	}
	pub := s.PublicKey()
	if len(pub) != 65 || pub[0] != 4 {
		t.Fatalf("public key is %d bytes starting %x; want 65 bytes of SEC1 uncompressed", len(pub), pub[:1])
	}
	v, err := NewVerifier(AlgECDSAP256, pub)
	if err != nil {
		t.Fatal(err)
	}
	if v.Alg() != AlgECDSAP256 || !slices.Equal(v.PublicKey(), pub) {
		t.Fatal("verifier does not report the signer's scheme and key")
	}
	if got := v.KeyIDs(); len(got) != 1 || got[0] != KeyID("ecdsa-p256", pub) {
		t.Fatalf("KeyIDs = %v", got)
	}
	for i := 0; i < 50; i++ {
		msg := []byte{byte(i), 1, 2, 3}
		sig, err := s.Sign(msg)
		if err != nil {
			t.Fatal(err)
		}
		if len(sig) != 64 {
			t.Fatalf("signature is %d bytes, want 64 (r||s)", len(sig))
		}
		_, sv := splitRS(sig)
		if sv.Cmp(new(big.Int).Rsh(p256N(), 1)) > 0 {
			t.Fatalf("signer emitted a high-S signature")
		}
		if !v.Verify(msg, sig) {
			t.Fatalf("round trip %d failed", i)
		}
		if v.Verify(append([]byte{9}, msg...), sig) {
			t.Fatal("verified a different message")
		}
	}
}

// The signature is over SHA-256 of the exact bytes: an independent ecdsa verification agrees.
func TestP256_HashesWithSHA256(t *testing.T) {
	s := newP256(t)
	msg := []byte("exact bytes")
	sig, _ := s.Sign(msg)
	h := sha256.Sum256(msg)
	r, sv := splitRS(sig)
	if !ecdsa.Verify(&s.Priv.PublicKey, h[:], r, sv) {
		t.Fatal("signature is not ECDSA over SHA-256(message)")
	}
	// A signature over a different hash of the message is refused.
	h2 := sha256.Sum256(append([]byte("x"), msg...))
	r2, s2, _ := ecdsa.Sign(rand.Reader, s.Priv, h2[:])
	v, _ := NewVerifier(AlgECDSAP256, s.PublicKey())
	if v.Verify(msg, joinRS(r2, lowS(s2))) {
		t.Fatal("accepted a signature over other bytes")
	}
}

func lowS(s *big.Int) *big.Int {
	if s.Cmp(new(big.Int).Rsh(p256N(), 1)) > 0 {
		return new(big.Int).Sub(p256N(), s)
	}
	return s
}

func TestP256_MalleabilityAndNonCanonicalRejected(t *testing.T) {
	s := newP256(t)
	v, _ := NewVerifier(AlgECDSAP256, s.PublicKey())
	msg := []byte("m")
	sig, _ := s.Sign(msg)
	r, sv := splitRS(sig)
	n := p256N()

	// (r, n-s) is a mathematically valid signature; the high-S twin is refused.
	if v.Verify(msg, joinRS(r, new(big.Int).Sub(n, sv))) {
		t.Fatal("accepted the high-S twin of a valid signature")
	}
	// r or s of 0, or >= n (encoded mod 2^256) are refused.
	for name, bad := range map[string][]byte{
		"zero r": joinRS(big.NewInt(0), sv),
		"zero s": joinRS(r, big.NewInt(0)),
		"r = n":  joinRS(n, sv),
		"s = n":  joinRS(r, n),
		"all ff": slices.Repeat([]byte{0xff}, 64),
		"empty":  nil,
		"short":  sig[:63],
		"long":   append(slices.Clone(sig), 0),
	} {
		if v.Verify(msg, bad) {
			t.Errorf("%s: accepted", name)
		}
	}
	// A DER-encoded signature is not the wire form.
	der, err := asn1.Marshal(struct{ R, S *big.Int }{r, sv})
	if err != nil {
		t.Fatal(err)
	}
	if v.Verify(msg, der) {
		t.Fatal("accepted a DER signature as r||s")
	}
}

func TestP256_PublicKeyEncodingStrict(t *testing.T) {
	s := newP256(t)
	pub := s.PublicKey()
	compressed := elliptic.MarshalCompressed(elliptic.P256(), s.Priv.PublicKey.X, s.Priv.PublicKey.Y)
	hybrid := slices.Clone(pub)
	hybrid[0] = 6 + byte(s.Priv.PublicKey.Y.Bit(0))
	offCurve := slices.Clone(pub)
	offCurve[64] ^= 1
	for name, bad := range map[string][]byte{
		"compressed":    compressed,
		"hybrid prefix": hybrid,
		"off curve":     offCurve,
		"infinity":      {0},
		"no prefix":     pub[1:],
		"empty":         nil,
		"trailing byte": append(slices.Clone(pub), 0),
		"x >= p":        append([]byte{4}, slices.Repeat([]byte{0xff}, 64)...),
		"zero point":    append([]byte{4}, make([]byte, 64)...),
		"ed25519-sized": make([]byte, 32),
	} {
		_, err := NewVerifier(AlgECDSAP256, bad)
		if !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: err = %v, want ErrMalformed", name, err)
		}
	}
	// Another curve's point is refused.
	k384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	b, _ := k384.PublicKey.Bytes()
	if _, err := NewVerifier(AlgECDSAP256, b); !errors.Is(err, ErrMalformed) {
		t.Errorf("P-384 key: %v", err)
	}
}

func TestP256_TextForm(t *testing.T) {
	s := newP256(t)
	txt := FormatPublicKey(AlgECDSAP256, s.PublicKey())
	if len(txt) != len("ecdsa-p256:")+130 {
		t.Fatalf("text form %q", txt)
	}
	v, err := ParsePublicKey("  " + txt + "\n")
	if err != nil || v.Alg() != AlgECDSAP256 {
		t.Fatalf("ParsePublicKey: %v %v", v, err)
	}
	// Upper-case hex parses (hex.DecodeString) but the verifier is the same key.
	if _, err := ParsePublicKey("ecdsa-p256:zz"); !errors.Is(err, ErrMalformed) {
		t.Fatalf("bad hex: %v", err)
	}
	// A bare 65-byte hex string is not read as P-256: bare hex means Ed25519 only.
	if _, err := ParsePublicKey(txt[len("ecdsa-p256:"):]); !errors.Is(err, ErrMalformed) {
		t.Fatalf("bare hex of a P-256 key: %v", err)
	}
}

// A P-256 signature never verifies under another scheme, and vice versa, even if the key bytes are
// presented under the other scheme's name.
func TestP256_CrossSchemeConfusion(t *testing.T) {
	p := newP256(t)
	edPub, edPriv, _ := ed25519.GenerateKey(rand.Reader)
	ed := Ed25519Signer{Priv: edPriv}
	msg := []byte("confusable")
	ps, _ := p.Sign(msg)
	es, _ := ed.Sign(msg)

	pv, _ := NewVerifier(AlgECDSAP256, p.PublicKey())
	ev, _ := NewVerifier(AlgEd25519, edPub)
	if pv.Verify(msg, es) {
		t.Error("P-256 verifier accepted an Ed25519 signature")
	}
	if ev.Verify(msg, ps) {
		t.Error("Ed25519 verifier accepted a P-256 signature")
	}
	// P-256 key bytes under the Ed25519 / hybrid names, and Ed25519 key bytes under P-256's.
	if _, err := NewVerifier(AlgEd25519, p.PublicKey()); !errors.Is(err, ErrMalformed) {
		t.Errorf("P-256 key as ed25519: %v", err)
	}
	if _, err := NewVerifier(AlgECDSAP256, edPub); !errors.Is(err, ErrMalformed) {
		t.Errorf("ed25519 key as P-256: %v", err)
	}
	if _, err := NewVerifier(AlgHybrid, p.PublicKey()); !errors.Is(err, ErrMalformed) {
		t.Errorf("P-256 key as hybrid: %v", err)
	}
	if _, err := NewVerifier("ecdsa-p256+ml-dsa-65", p.PublicKey()); !errors.Is(err, ErrMalformed) {
		t.Errorf("P-256 hybrid is not a scheme yet: %v", err)
	}

	// A tree head signed by P-256 does not verify as a head claiming another scheme.
	sth, err := SignTreeHead(testTreeHead(), p)
	if err != nil {
		t.Fatal(err)
	}
	if sth.Alg != AlgECDSAP256 {
		t.Fatalf("sth alg %q", sth.Alg)
	}
	if err := sth.Verify(pv); err != nil {
		t.Fatal(err)
	}
	if err := sth.Verify(ev); err == nil {
		t.Fatal("P-256 head verified under an Ed25519 verifier")
	}
	relabelled := sth
	relabelled.Alg = AlgEd25519
	if relabelled.Verify(pv) == nil || relabelled.Verify(ev) == nil {
		t.Fatal("a relabelled head verified")
	}
	tampered := sth
	tampered.Size++
	if tampered.Verify(pv) == nil {
		t.Fatal("tampered head verified")
	}
}

func TestP256_SignerRefusesBadKeys(t *testing.T) {
	if _, err := (ECDSAP256Signer{}).Sign([]byte("x")); err == nil {
		t.Error("nil key signed")
	}
	if (ECDSAP256Signer{}).PublicKey() != nil {
		t.Error("nil key has a public key")
	}
	k384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	bad := ECDSAP256Signer{Priv: k384}
	if bad.PublicKey() != nil {
		t.Error("P-384 key reported a public key")
	}
	if _, err := bad.Sign([]byte("x")); err == nil {
		t.Error("P-384 key signed")
	}
	if err := checkSigner(bad); err == nil {
		t.Error("checkSigner accepted a P-384 key")
	}
	if err := checkSigner(ECDSAP256Signer{}); err == nil {
		t.Error("checkSigner accepted a nil key")
	}
	if (ECDSAP256Verifier{}).Verify([]byte("m"), make([]byte, 64)) {
		t.Error("zero verifier verified")
	}
	if (ECDSAP256Verifier{}).KeyIDs() != nil || (ECDSAP256Verifier{}).PublicKey() != nil {
		t.Error("zero verifier reports a key")
	}
}

func TestP256_SignatureAdapters(t *testing.T) {
	s := newP256(t)
	msg := []byte("kms")
	h := sha256.Sum256(msg)
	v, _ := NewVerifier(AlgECDSAP256, s.PublicKey())
	n := p256N()
	for i := 0; i < 30; i++ {
		der, err := ecdsa.SignASN1(rand.Reader, s.Priv, h[:])
		if err != nil {
			t.Fatal(err)
		}
		sig, err := ECDSAP256SignatureFromDER(der)
		if err != nil {
			t.Fatal(err)
		}
		if !v.Verify(msg, sig) {
			t.Fatal("converted DER signature does not verify")
		}
		// Raw r||s with the high-S form normalises to the same low-S signature.
		r, sv := splitRS(sig)
		hi := joinRS(r, new(big.Int).Sub(n, sv))
		got, err := ECDSAP256SignatureFromRaw(hi)
		if err != nil || !slices.Equal(got, sig) {
			t.Fatalf("raw normalisation: %v", err)
		}
	}
	// Strict DER: BER padding, trailing bytes, negative integers are refused.
	r, sv := splitRS(func() []byte { sg, _ := s.Sign(msg); return sg }())
	der, _ := asn1.Marshal(struct{ R, S *big.Int }{r, sv})
	if _, err := ECDSAP256SignatureFromDER(append(slices.Clone(der), 0)); err == nil {
		t.Error("trailing byte accepted")
	}
	neg, _ := asn1.Marshal(struct{ R, S *big.Int }{new(big.Int).Neg(r), sv})
	if _, err := ECDSAP256SignatureFromDER(neg); err == nil {
		t.Error("negative r accepted")
	}
	zero, _ := asn1.Marshal(struct{ R, S *big.Int }{big.NewInt(0), sv})
	if _, err := ECDSAP256SignatureFromDER(zero); err == nil {
		t.Error("zero r accepted")
	}
	big_, _ := asn1.Marshal(struct{ R, S *big.Int }{new(big.Int).Add(n, big.NewInt(1)), sv})
	if _, err := ECDSAP256SignatureFromDER(big_); err == nil {
		t.Error("r >= n accepted")
	}
	// A non-minimal length form (BER) is refused.
	ber := append([]byte{0x30, 0x81, byte(len(der) - 2)}, der[2:]...)
	if _, err := ECDSAP256SignatureFromDER(ber); err == nil {
		t.Error("BER long-form length accepted")
	}
	if _, err := ECDSAP256SignatureFromRaw(make([]byte, 63)); err == nil {
		t.Error("short raw accepted")
	}

	// SPKI to the SEC1 point.
	spki, err := x509.MarshalPKIXPublicKey(&s.Priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ECDSAP256PublicKeyFromSPKI(spki)
	if err != nil || !slices.Equal(pub, s.PublicKey()) {
		t.Fatalf("SPKI: %v", err)
	}
	k384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	spki384, _ := x509.MarshalPKIXPublicKey(&k384.PublicKey)
	if _, err := ECDSAP256PublicKeyFromSPKI(spki384); err == nil {
		t.Error("P-384 SPKI accepted")
	}
	edPub, _, _ := ed25519.GenerateKey(rand.Reader)
	spkiEd, _ := x509.MarshalPKIXPublicKey(edPub)
	if _, err := ECDSAP256PublicKeyFromSPKI(spkiEd); err == nil {
		t.Error("Ed25519 SPKI accepted")
	}
}

// A HybridSigner stays ed25519+ml-dsa-65: P-256 is not a hybrid half.
func TestP256_NotAHybridHalf(t *testing.T) {
	if AlgHybrid != "ed25519+ml-dsa-65" {
		t.Fatal("hybrid scheme changed")
	}
}
