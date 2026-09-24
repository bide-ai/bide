package audit

import (
	"crypto/ed25519"
	"crypto/mldsa"
	"encoding/binary"
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
	AlgEd25519 = "ed25519"
	AlgMLDSA65 = "ml-dsa-65"
	AlgHybrid  = "ed25519+ml-dsa-65"
)

// mldsaContext domain-separates ML-DSA signatures produced by this package.
const mldsaContext = "go-agents.audit.sth.v1"

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
type Ed25519Signer struct{ Priv ed25519.PrivateKey }

func (Ed25519Signer) Alg() string                     { return AlgEd25519 }
func (s Ed25519Signer) Sign(m []byte) ([]byte, error) { return ed25519.Sign(s.Priv, m), nil }

// Ed25519Verifier verifies with an Ed25519 public key.
type Ed25519Verifier struct{ Pub ed25519.PublicKey }

func (Ed25519Verifier) Alg() string                 { return AlgEd25519 }
func (v Ed25519Verifier) Verify(m, sig []byte) bool { return ed25519.Verify(v.Pub, m, sig) }

// ---- ML-DSA-65 (FIPS 204) ----

// MLDSASigner signs with an ML-DSA-65 private key. Signing is deterministic, so replaying the
// same tree head reproduces the same signature.
type MLDSASigner struct{ Priv *mldsa.PrivateKey }

func (MLDSASigner) Alg() string { return AlgMLDSA65 }
func (s MLDSASigner) Sign(m []byte) ([]byte, error) {
	return s.Priv.SignDeterministic(m, &mldsa.Options{Context: mldsaContext})
}

// MLDSAVerifier verifies with an ML-DSA-65 public key.
type MLDSAVerifier struct{ Pub *mldsa.PublicKey }

func (MLDSAVerifier) Alg() string { return AlgMLDSA65 }
func (v MLDSAVerifier) Verify(m, sig []byte) bool {
	return mldsa.Verify(v.Pub, m, sig, &mldsa.Options{Context: mldsaContext}) == nil
}

// ---- hybrid ed25519 + ML-DSA-65 ----

// HybridSigner signs with both schemes; the signature is only accepted if BOTH verify, so the
// anchor is safe as long as either scheme remains unbroken.
type HybridSigner struct {
	Ed Ed25519Signer
	ML MLDSASigner
}

func (HybridSigner) Alg() string { return AlgHybrid }
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
	Ed Ed25519Verifier
	ML MLDSAVerifier
}

func (HybridVerifier) Alg() string { return AlgHybrid }
func (v HybridVerifier) Verify(m, sig []byte) bool {
	e, d, ok := decodeHybrid(sig)
	if !ok {
		return false
	}
	return v.Ed.Verify(m, e) && v.ML.Verify(m, d)
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
	if int(n) > len(sig)-4 {
		return nil, nil, false
	}
	return sig[4 : 4+n], sig[4+n:], true
}

// VerifyWith is the scheme-agnostic form of ProofBundle.Verify: it authenticates the STH under
// any Verifier (ed25519, ML-DSA, or hybrid), then binds and checks the inclusion proof.
func (b ProofBundle) VerifyWith(v Verifier) (bool, error) {
	if !b.STH.VerifyWith(v) {
		return false, nil
	}
	if b.Inclusion.Size != b.STH.Size {
		return false, nil
	}
	return VerifyInclusion(b.STH.Root, b.Record, b.Inclusion)
}

// VerifyWith is the scheme-agnostic form of AbsenceBundle.Verify.
func (b AbsenceBundle) VerifyWith(v Verifier) (bool, error) {
	if !b.STH.VerifyWith(v) {
		return false, nil
	}
	if b.Absence.Size != b.STH.Size {
		return false, nil
	}
	return VerifyAbsence(b.STH.Root, b.Absence)
}
