package pkcs11

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"math/big"
	"testing"

	"github.com/bide-ai/bide/audit"
)

// rawToken is a P-256 token whose sessions return a chosen raw signature form.
type rawToken struct {
	k    *ecdsa.PrivateKey
	pub  []byte
	sign func(k *ecdsa.PrivateKey, m []byte) []byte
}

func (r *rawToken) open(Config, string) (session, []byte, error) { return rawSession{r}, r.pub, nil }

type rawSession struct{ r *rawToken }

func (s rawSession) sign(m []byte) ([]byte, error) {
	// what the real session does with the token output
	return audit.ECDSAP256SignatureFromRaw(s.r.sign(s.r.k, m))
}
func (rawSession) close() error { return nil }

func newRawToken(t *testing.T, f func(*ecdsa.PrivateKey, []byte) []byte) *rawToken {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	pub, _ := k.PublicKey.Bytes()
	return &rawToken{k: k, pub: pub, sign: f}
}

func rs(k *ecdsa.PrivateKey, h []byte, high bool) []byte {
	r, s, _ := ecdsa.Sign(rand.Reader, k, h)
	n := elliptic.P256().Params().N
	if high != (s.Cmp(new(big.Int).Rsh(n, 1)) > 0) {
		s = new(big.Int).Sub(n, s)
	}
	out := make([]byte, 64)
	r.FillBytes(out[:32])
	s.FillBytes(out[32:])
	return out
}

func p256Cfg() Config {
	c := goodConfig()
	c.Scheme = audit.AlgECDSAP256
	return c
}

// A token that returns high-S signatures: Sign folds them and they verify.
func TestP256HighSFolded(t *testing.T) {
	tok := newRawToken(t, func(k *ecdsa.PrivateKey, m []byte) []byte { h := sha256.Sum256(m); return rs(k, h[:], true) })
	s, err := openWith(p256Cfg(), tok.open)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := audit.VerifierOf(s)
	for i := 0; i < 20; i++ {
		sig, err := s.Sign([]byte{byte(i)})
		if err != nil || !v.Verify([]byte{byte(i)}, sig) {
			t.Fatalf("%v", err)
		}
	}
}

// A token that signs the message without hashing it first (CKM_ECDSA fed the wrong input) is found
// at Open, not at the first anchor.
func TestP256WrongHashRefusedAtOpen(t *testing.T) {
	tok := newRawToken(t, func(k *ecdsa.PrivateKey, m []byte) []byte {
		h := sha256.Sum256(m)
		h = sha256.Sum256(h[:]) // double hash
		return rs(k, h[:], false)
	})
	if _, err := openWith(p256Cfg(), tok.open); err == nil {
		t.Fatal("opened a token that signs the wrong digest")
	}
}

// An off-curve 65-byte point is refused by Open (parsePoint only checks length and prefix).
func TestP256OffCurvePointRefused(t *testing.T) {
	tok := newRawToken(t, func(k *ecdsa.PrivateKey, m []byte) []byte { h := sha256.Sum256(m); return rs(k, h[:], false) })
	bad := append([]byte(nil), tok.pub...)
	bad[64] ^= 1
	tok.pub = bad
	if _, err := openWith(p256Cfg(), tok.open); err == nil {
		t.Fatal("opened with an off-curve public key")
	}
}

// Degenerate raw outputs: all zero, r or s = n, DER-looking 64 bytes.
func TestP256DegenerateRawRefused(t *testing.T) {
	n := elliptic.P256().Params().N
	for name, raw := range map[string][]byte{
		"zero": make([]byte, 64),
		"r=n":  append(n.FillBytes(make([]byte, 32)), make([]byte, 32)...),
		"s=n":  append(append([]byte{}, make([]byte, 31)...), append([]byte{1}, n.FillBytes(make([]byte, 32))...)...),
		"der":  {0x30, 0x3e, 0x02, 0x1d, 1},
	} {
		raw := raw
		tok := newRawToken(t, func(*ecdsa.PrivateKey, []byte) []byte { return append(raw, make([]byte, 64)...)[:64] })
		if _, err := openWith(p256Cfg(), tok.open); err == nil {
			t.Fatalf("%s: opened", name)
		}
	}
}
