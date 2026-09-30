package audit

import (
	"crypto/ed25519"
	"crypto/mldsa"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
)

// Signature-scheme agility for signed tree heads. The audit trail signs an STH's canonical
// encoding; which scheme does the signing is pluggable so a deployment can choose classical
// (ed25519, small and fast, FIPS-approved), post-quantum (ML-DSA, FIPS 204), or a hybrid of
// both (secure if either holds, the direction transparency logs are moving). All three are
// stdlib as of Go 1.27, so post-quantum anchoring adds no dependency.
//
// Note: crypto/mldsa is unavailable under the FIPS 140-3 module, so ML-DSA and hybrid signing
// error at runtime in FIPS mode while ed25519 keeps working. Pick the scheme per buyer: FIPS
// today, or post-quantum forward protection for long-lived anchors.

// Algorithm identifiers stored in SignedTreeHead.Alg.
const (
	AlgEd25519 = "ed25519"           // classical Ed25519 (small, fast, FIPS-approved)
	AlgMLDSA65 = "ml-dsa-65"         // post-quantum ML-DSA-65 (FIPS 204)
	AlgHybrid  = "ed25519+ml-dsa-65" // both schemes; valid only if both verify
)

// mldsaContext domain-separates ML-DSA signatures produced by this package.
const mldsaContext = "bide.audit.sth.v1"

// Signer produces a signature over a message under a named algorithm.
type Signer interface {
	Alg() string
	Sign(message []byte) ([]byte, error)
}

// Verifier checks a signature over a message under a named algorithm.
type Verifier interface {
	Alg() string
	Verify(message, signature []byte) bool
}

// ---- ed25519 ----

// Ed25519Signer signs with an Ed25519 private key.
type Ed25519Signer struct {
	Priv ed25519.PrivateKey // the Ed25519 private key that signs
}

// Alg reports the signature scheme (AlgEd25519).
func (Ed25519Signer) Alg() string { return AlgEd25519 }

// Sign returns the Ed25519 signature over m, or an error if the private key has the wrong length.
func (s Ed25519Signer) Sign(m []byte) ([]byte, error) {
	if err := checkPrivateKey(s.Priv); err != nil {
		return nil, err
	}
	return ed25519.Sign(s.Priv, m), nil
}

// Ed25519Verifier verifies with an Ed25519 public key.
type Ed25519Verifier struct {
	Pub ed25519.PublicKey // the Ed25519 public key that verifies
}

// Alg reports the signature scheme (AlgEd25519).
func (Ed25519Verifier) Alg() string { return AlgEd25519 }

// Verify reports whether sig is a valid Ed25519 signature over m. A public key of the wrong length
// verifies nothing (it never panics).
func (v Ed25519Verifier) Verify(m, sig []byte) bool {
	return len(v.Pub) == ed25519.PublicKeySize && ed25519.Verify(v.Pub, m, sig)
}

// KeyIDs identifies the public key, for agent.ApproverVerifier: one entry, KeyID(AlgEd25519,
// Pub). A public key of the wrong length verifies nothing and reports no entry, so an approval
// gate refuses it as a configuration error rather than counting a seat no one can fill.
func (v Ed25519Verifier) KeyIDs() []string {
	if len(v.Pub) != ed25519.PublicKeySize {
		return nil
	}
	return []string{KeyID(AlgEd25519, v.Pub)}
}

// ---- ML-DSA-65 (FIPS 204) ----

// MLDSASigner signs with an ML-DSA-65 private key. Signing is deterministic, so replaying the
// same tree head reproduces the same signature.
type MLDSASigner struct {
	Priv *mldsa.PrivateKey // the ML-DSA-65 private key that signs
}

// Alg reports the signature scheme (AlgMLDSA65).
func (MLDSASigner) Alg() string { return AlgMLDSA65 }

// Sign returns the deterministic ML-DSA-65 signature over m.
func (s MLDSASigner) Sign(m []byte) ([]byte, error) {
	return s.Priv.SignDeterministic(m, &mldsa.Options{Context: mldsaContext})
}

// MLDSAVerifier verifies with an ML-DSA-65 public key.
type MLDSAVerifier struct {
	Pub *mldsa.PublicKey // the ML-DSA-65 public key that verifies
}

// Alg reports the signature scheme (AlgMLDSA65).
func (MLDSAVerifier) Alg() string { return AlgMLDSA65 }

// Verify reports whether sig is a valid ML-DSA-65 signature over m.
func (v MLDSAVerifier) Verify(m, sig []byte) bool {
	return mldsa.Verify(v.Pub, m, sig, &mldsa.Options{Context: mldsaContext}) == nil
}

// KeyIDs identifies the public key, for agent.ApproverVerifier: one entry, KeyID(alg,
// Pub.Bytes()), where alg names the key's parameter set ("ml-dsa-44", AlgMLDSA65 or "ml-dsa-87"):
// Verify accepts a key of any FIPS 204 parameter set, so the label says which one it is. A nil key
// reports no entry.
func (v MLDSAVerifier) KeyIDs() []string {
	if v.Pub == nil {
		return nil
	}
	return []string{KeyID(strings.ToLower(v.Pub.Parameters().String()), v.Pub.Bytes())}
}

// ---- hybrid ed25519 + ML-DSA-65 ----

// HybridSigner signs with both schemes; the signature is only accepted if BOTH verify, so the
// anchor is safe as long as either scheme remains unbroken.
type HybridSigner struct {
	Ed Ed25519Signer // the Ed25519 component signer
	ML MLDSASigner   // the ML-DSA-65 component signer
}

// Alg reports the signature scheme (AlgHybrid).
func (HybridSigner) Alg() string { return AlgHybrid }

// Sign returns the packed pair of Ed25519 and ML-DSA-65 signatures over m.
func (s HybridSigner) Sign(m []byte) ([]byte, error) {
	e, err := s.Ed.Sign(m)
	if err != nil {
		return nil, err
	}
	d, err := s.ML.Sign(m)
	if err != nil {
		return nil, err
	}
	return encodeHybrid(e, d), nil
}

// HybridVerifier requires both component signatures to verify.
type HybridVerifier struct {
	Ed Ed25519Verifier // the Ed25519 component verifier
	ML MLDSAVerifier   // the ML-DSA-65 component verifier
}

// Alg reports the signature scheme (AlgHybrid).
func (HybridVerifier) Alg() string { return AlgHybrid }

// Verify reports whether both component signatures over m are valid.
func (v HybridVerifier) Verify(m, sig []byte) bool {
	e, d, ok := decodeHybrid(sig)
	if !ok {
		return false
	}
	return v.Ed.Verify(m, e) && v.ML.Verify(m, d)
}

// KeyIDs identifies both component keys, for agent.ApproverVerifier: the Ed25519 entry, then the
// ML-DSA-65 entry. They are listed separately, not as one identity of the pair, because a hybrid
// signature is meant to hold while either scheme holds: if one scheme breaks, the other
// component's key alone signs, so two approvers sharing either component are one seat. If either
// component reports no entry the hybrid reports none, since it can verify nothing.
func (v HybridVerifier) KeyIDs() []string {
	e, m := v.Ed.KeyIDs(), v.ML.KeyIDs()
	if len(e) == 0 || len(m) == 0 {
		return nil
	}
	return append(e, m...)
}

// KeyID is the key identity the audit verifiers report from KeyIDs: alg, a colon, and the
// lowercase hex SHA-256 of the public key's encoding (for Ed25519 the 32-byte key, for ML-DSA its
// FIPS 204 encoding). It depends only on the key, so every verifier over one key reports it.
func KeyID(alg string, pub []byte) string {
	h := sha256.Sum256(pub)
	return alg + ":" + hex.EncodeToString(h[:])
}

// encodeHybrid packs two signatures as len(ed) || ed || mldsa, so the split is unambiguous.
func encodeHybrid(ed, mldsaSig []byte) []byte {
	out := make([]byte, 0, 4+len(ed)+len(mldsaSig))
	out = binary.BigEndian.AppendUint32(out, uint32(len(ed)))
	out = append(out, ed...)
	out = append(out, mldsaSig...)
	return out
}

func decodeHybrid(sig []byte) (ed, mldsaSig []byte, ok bool) {
	if len(sig) < 4 {
		return nil, nil, false
	}
	n := binary.BigEndian.Uint32(sig[:4])
	if !hybridLenFits(n, len(sig)-4) {
		return nil, nil, false
	}
	return sig[4 : 4+n], sig[4+n:], true
}

// hybridLenFits reports whether a hybrid signature's Ed25519 length prefix n fits in the rest
// bytes that follow it. The comparison is made in uint64, which holds every uint32 and every
// non-negative int, so it does not depend on the width of int: converting n to a 32-bit int
// would make a prefix of 2^31 or more negative, pass the check, and panic the split. It is
// generic over the integer type so a test can check it at the width int has on a 32-bit platform.
func hybridLenFits[I ~int | ~int32 | ~int64](n uint32, rest I) bool {
	return rest >= 0 && uint64(n) <= uint64(rest)
}

// checkPrivateKey refuses an ed25519 private key that is not ed25519.PrivateKeySize bytes, on which
// ed25519.Sign panics.
func checkPrivateKey(priv ed25519.PrivateKey) error {
	if len(priv) != ed25519.PrivateKeySize {
		return fmt.Errorf("audit: ed25519 private key is %d bytes, want %d", len(priv), ed25519.PrivateKeySize)
	}
	return nil
}
