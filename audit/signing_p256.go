package audit

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"fmt"
	"math/big"

	"github.com/bide-ai/bide/agent"
)

// ECDSA P-256 (FIPS 186-5), the scheme many HSMs and cloud key services sign with where they cannot
// sign Ed25519 (AWS KMS ECC_NIST_P256, Azure Key Vault P-256, PKCS#11 CKM_ECDSA). The wire formats
// are fixed here so a signer and a verifier written apart agree on every byte:
//
//   - Hash: SHA-256 over the exact message bytes (the same bytes Ed25519 would sign). A signer that
//     holds only a digest interface (a KMS "DIGEST" signing mode) signs SHA-256(message).
//   - Signature: 64 bytes, r || s, each a 32-byte big-endian integer (IEEE P1363, not ASN.1 DER),
//     with 1 <= r < n and 1 <= s <= (n-1)/2. A verifier refuses a high-S signature, so a signature
//     has one encoding and a third party cannot turn a valid one into a second valid one. A signer
//     that gets a DER signature (AWS KMS) or a high-S one converts it with ECDSAP256SignatureFromDER
//     or ECDSAP256SignatureFromRaw.
//   - Public key: 65 bytes, the SEC1 uncompressed point 0x04 || X || Y, on the curve. Compressed,
//     hybrid and infinity encodings, and points off the curve, are refused. The text form is
//     "ecdsa-p256:" followed by the 130 lowercase hex digits. ECDSAP256PublicKeyFromSPKI reads the
//     point out of the SubjectPublicKeyInfo a KMS returns.
//   - Signing is randomized (crypto/ecdsa mixes the key and message into the nonce), unlike Ed25519
//     and ML-DSA, so signing one message twice gives two different valid signatures.
//
// P-256 is not a hybrid component: the hybrid scheme stays ed25519+ml-dsa-65, whose point is
// post-quantum protection that no HSM or KMS offers yet.

// AlgECDSAP256 is the ECDSA P-256 scheme.
const AlgECDSAP256 Alg = "ecdsa-p256"

const (
	p256ScalarSize = 32
	p256SigSize    = 2 * p256ScalarSize
	p256PubSize    = 1 + 2*p256ScalarSize
)

// ECDSAP256Signer signs with an ECDSA P-256 private key.
type ECDSAP256Signer struct {
	Priv *ecdsa.PrivateKey // the P-256 private key that signs
}

func (s ECDSAP256Signer) check() error {
	if s.Priv == nil {
		return fmt.Errorf("audit: no ecdsa-p256 private key: %w", agent.ErrConfig)
	}
	if s.Priv.Curve != elliptic.P256() {
		return fmt.Errorf("audit: ecdsa private key is on %s, want P-256: %w", s.Priv.Curve.Params().Name, agent.ErrConfig)
	}
	// The public half must be D*G: a key whose halves disagree signs without error, and what it
	// signs verifies under no key.
	e, err := s.Priv.ECDH()
	if err != nil {
		return fmt.Errorf("audit: ecdsa-p256 private key is not valid: %v: %w", err, agent.ErrConfig)
	}
	pub, err := s.Priv.PublicKey.Bytes()
	if err != nil || !bytes.Equal(e.PublicKey().Bytes(), pub) {
		return fmt.Errorf("audit: ecdsa-p256 private key's public half is not its private scalar's: %w", agent.ErrConfig)
	}
	return nil
}

// Alg reports the signature scheme (AlgECDSAP256).
func (ECDSAP256Signer) Alg() Alg { return AlgECDSAP256 }

// PublicKey returns the 65-byte uncompressed SEC1 public key, or nil if there is no key or it is
// not on P-256.
func (s ECDSAP256Signer) PublicKey() []byte {
	if s.check() != nil {
		return nil
	}
	b, err := s.Priv.PublicKey.Bytes()
	if err != nil {
		return nil
	}
	return b
}

// Sign returns the 64-byte low-S r||s signature over SHA-256(m), or an error if the key is unusable.
func (s ECDSAP256Signer) Sign(m []byte) ([]byte, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	h := sha256.Sum256(m)
	r, sv, err := ecdsa.Sign(rand.Reader, s.Priv, h[:])
	if err != nil {
		return nil, fmt.Errorf("audit: ecdsa-p256 sign: %w", err)
	}
	return p256Pack(r, sv), nil
}

// ECDSAP256Verifier verifies with an ECDSA P-256 public key.
type ECDSAP256Verifier struct {
	Pub *ecdsa.PublicKey // the P-256 public key that verifies
}

func (v ECDSAP256Verifier) usable() bool {
	return v.Pub != nil && v.Pub.Curve == elliptic.P256() && v.Pub.X != nil && v.Pub.Y != nil && v.Pub.IsOnCurve(v.Pub.X, v.Pub.Y)
}

// Alg reports the signature scheme (AlgECDSAP256).
func (ECDSAP256Verifier) Alg() Alg { return AlgECDSAP256 }

// PublicKey returns the 65-byte uncompressed SEC1 public key, or nil if there is none or it is not a
// P-256 point.
func (v ECDSAP256Verifier) PublicKey() []byte {
	if !v.usable() {
		return nil
	}
	b, err := v.Pub.Bytes()
	if err != nil {
		return nil
	}
	return b
}

// Verify reports whether sig is a canonical (64-byte, 1 <= r < n, 1 <= s <= (n-1)/2) ECDSA P-256
// signature over SHA-256(m). Any other encoding, including a valid signature with a high S, is
// refused. A verifier without a usable key verifies nothing.
func (v ECDSAP256Verifier) Verify(m, sig []byte) bool {
	if !v.usable() {
		return false
	}
	r, s, ok := p256Unpack(sig)
	if !ok {
		return false
	}
	h := sha256.Sum256(m)
	return ecdsa.Verify(v.Pub, h[:], r, s)
}

// KeyIDs identifies the public key, for agent.ApproverVerifier: one entry, KeyID(AlgECDSAP256, the
// 65-byte point). A verifier without a usable key reports none.
func (v ECDSAP256Verifier) KeyIDs() []string {
	pub := v.PublicKey()
	if pub == nil {
		return nil
	}
	return []string{KeyID(string(AlgECDSAP256), pub)}
}

// newECDSAP256Verifier parses the 65-byte SEC1 uncompressed point.
func newECDSAP256Verifier(pub []byte) (Verifier, error) {
	if len(pub) != p256PubSize {
		return nil, fmt.Errorf("audit: ecdsa-p256 public key is %d bytes, want %d (SEC1 uncompressed): %w", len(pub), p256PubSize, ErrMalformed)
	}
	k, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), pub)
	if err != nil {
		return nil, fmt.Errorf("audit: ecdsa-p256 public key: %v: %w", err, ErrMalformed)
	}
	return ECDSAP256Verifier{Pub: k}, nil
}

// p256HalfOrder is (n-1)/2, the largest permitted s.
var p256HalfOrder = new(big.Int).Rsh(elliptic.P256().Params().N, 1)

// p256Pack encodes r and s as low-S r||s.
func p256Pack(r, s *big.Int) []byte {
	if s.Cmp(p256HalfOrder) > 0 {
		s = new(big.Int).Sub(elliptic.P256().Params().N, s)
	}
	out := make([]byte, p256SigSize)
	r.FillBytes(out[:p256ScalarSize])
	s.FillBytes(out[p256ScalarSize:])
	return out
}

// p256Unpack decodes a canonical r||s: 64 bytes, 1 <= r < n, 1 <= s <= (n-1)/2.
func p256Unpack(sig []byte) (r, s *big.Int, ok bool) {
	if len(sig) != p256SigSize {
		return nil, nil, false
	}
	n := elliptic.P256().Params().N
	r = new(big.Int).SetBytes(sig[:p256ScalarSize])
	s = new(big.Int).SetBytes(sig[p256ScalarSize:])
	if r.Sign() == 0 || r.Cmp(n) >= 0 || s.Sign() == 0 || s.Cmp(p256HalfOrder) > 0 {
		return nil, nil, false
	}
	return r, s, true
}

// ECDSAP256SignatureFromRaw turns a 64-byte r||s signature (what Azure Key Vault and PKCS#11
// CKM_ECDSA return) into the canonical form this package verifies: the same r and the low s. It
// refuses a signature of the wrong length or with r or s outside [1, n-1].
func ECDSAP256SignatureFromRaw(rs []byte) ([]byte, error) {
	if len(rs) != p256SigSize {
		return nil, fmt.Errorf("audit: ecdsa-p256 raw signature is %d bytes, want %d: %w", len(rs), p256SigSize, ErrMalformed)
	}
	n := elliptic.P256().Params().N
	r := new(big.Int).SetBytes(rs[:p256ScalarSize])
	s := new(big.Int).SetBytes(rs[p256ScalarSize:])
	if r.Sign() == 0 || r.Cmp(n) >= 0 || s.Sign() == 0 || s.Cmp(n) >= 0 {
		return nil, fmt.Errorf("audit: ecdsa-p256 signature scalar out of range: %w", ErrMalformed)
	}
	return p256Pack(r, s), nil
}

// ECDSAP256SignatureFromDER turns an ASN.1 DER ECDSA-Sig-Value (SEQUENCE { r INTEGER, s INTEGER },
// what AWS KMS returns) into the canonical 64-byte low-S r||s. The DER must be strictly
// well-formed (minimal encoding, no trailing bytes, no negative or out-of-range integer).
func ECDSAP256SignatureFromDER(der []byte) ([]byte, error) {
	var sig struct{ R, S *big.Int }
	rest, err := asn1.Unmarshal(der, &sig)
	if err != nil || len(rest) != 0 {
		return nil, fmt.Errorf("audit: ecdsa-p256 DER signature does not parse: %w", ErrMalformed)
	}
	if again, err := asn1.Marshal(sig); err != nil || !bytes.Equal(again, der) {
		return nil, fmt.Errorf("audit: ecdsa-p256 DER signature is not minimally encoded: %w", ErrMalformed)
	}
	n := elliptic.P256().Params().N
	if sig.R.Sign() <= 0 || sig.R.Cmp(n) >= 0 || sig.S.Sign() <= 0 || sig.S.Cmp(n) >= 0 {
		return nil, fmt.Errorf("audit: ecdsa-p256 signature scalar out of range: %w", ErrMalformed)
	}
	return p256Pack(sig.R, sig.S), nil
}

// ECDSAP256PublicKeyFromSPKI returns the 65-byte uncompressed SEC1 point (the encoding NewVerifier
// reads) from a DER SubjectPublicKeyInfo, which is what KMS GetPublicKey and Key Vault return. It
// refuses a key that is not ECDSA on P-256.
func ECDSAP256PublicKeyFromSPKI(spki []byte) ([]byte, error) {
	k, err := x509.ParsePKIXPublicKey(spki)
	if err != nil {
		return nil, fmt.Errorf("audit: ecdsa-p256 SPKI: %v: %w", err, ErrMalformed)
	}
	pk, ok := k.(*ecdsa.PublicKey)
	if !ok || pk.Curve != elliptic.P256() {
		return nil, fmt.Errorf("audit: SPKI is %T, want an ECDSA P-256 key: %w", k, ErrMalformed)
	}
	return pk.Bytes()
}
