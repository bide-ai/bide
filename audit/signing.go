package audit

import (
	"bytes"
	"crypto/ed25519"
	"crypto/mldsa"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/bide-ai/bide/agent"
)

// Signature-scheme agility. Everything this package signs (tree heads, evidence seals, grants, the
// linear head) and everything it verifies goes through a Signer or a Verifier, so a deployment can
// choose classical (ed25519, small and fast, FIPS-approved), post-quantum (ML-DSA-65, FIPS 204), or
// a hybrid of both (secure if either holds, the direction transparency logs are moving). All three
// are stdlib as of Go 1.27, so post-quantum anchoring adds no dependency.
//
// Every signed artifact names its scheme (Alg) and every verifier checks that the scheme is its
// own, so a signature is never checked under a scheme it was not made with. A tree head also signs
// its scheme (see TreeHead), and a hybrid signature separates its two components by domain (see
// HybridSigner), so the ed25519 half of a hybrid signature verifies neither as a hybrid signature
// nor as a plain ed25519 one.
//
// Note: crypto/mldsa is unavailable under the FIPS 140-3 module v1.0.0, so ML-DSA and hybrid
// signing error at runtime there while ed25519 keeps working. Pick the scheme per buyer: FIPS
// today, or post-quantum forward protection for long-lived anchors.

// Alg names a signature scheme. It is agent.Alg, so the scheme an approver decision is journaled
// under (agent.Record.ApproverAlg) and the scheme of an audit Verifier are one type.
type Alg = agent.Alg

// The signature schemes.
const (
	AlgEd25519 Alg = "ed25519"           // classical Ed25519 (small, fast, FIPS-approved)
	AlgMLDSA65 Alg = "ml-dsa-65"         // post-quantum ML-DSA-65 (FIPS 204)
	AlgHybrid  Alg = "ed25519+ml-dsa-65" // both schemes; valid only if both verify
)

// mldsaContext domain-separates ML-DSA signatures produced by this package from ML-DSA signatures
// the same key makes for anything else. Each message this package signs carries its own tag as
// well (a tree head, a seal, a grant), so the messages are told apart by their bytes.
const mldsaContext = "bide.audit.mldsa65.v1"

// Hybrid component labels. Each component of a hybrid signature signs its label followed by the
// message, after the IETF composite-signature construction, so neither component is a signature
// over the message alone: the ed25519 half of a hybrid signature does not verify as a plain
// ed25519 signature over the same message, and the two halves cannot be swapped or reused apart.
const (
	hybridEd25519Label = "bide.hybrid.ed25519.v1\x00"
	hybridMLDSA65Label = "bide.hybrid.mldsa65.v1\x00"
)

// Signer produces signatures under one scheme with one key.
type Signer interface {
	// Alg names the scheme.
	Alg() Alg
	// PublicKey returns the encoded public key of the signing key (see NewVerifier for the
	// encoding of each scheme), or nil if the signer holds no usable key.
	PublicKey() []byte
	// Sign returns a signature over message, or an error if the key is unusable.
	Sign(message []byte) ([]byte, error)
}

// Verifier checks signatures under one scheme with one public key.
type Verifier interface {
	// Alg names the scheme.
	Alg() Alg
	// PublicKey returns the encoded public key (see NewVerifier), or nil if the verifier holds none.
	PublicKey() []byte
	// Verify reports whether signature is valid over message. A verifier without a usable key
	// verifies nothing; it never panics.
	Verify(message, signature []byte) bool
	// KeyIDs identifies the signing keys behind Verify (see agent.ApproverVerifier.KeyIDs and
	// KeyID), so every Verifier is an agent.ApproverVerifier. A verifier without a usable key
	// reports none.
	KeyIDs() []string
}

// NewVerifier returns the verifier of scheme alg for the encoded public key pub:
//
//	ed25519:           the 32-byte Ed25519 public key
//	ml-dsa-65:         the 1952-byte ML-DSA-65 public key encoding (mldsa.PublicKey.Bytes)
//	ed25519+ml-dsa-65: a 4-byte big-endian length of the Ed25519 key, the Ed25519 key, then the
//	                   ML-DSA-65 key (HybridSigner.PublicKey)
//
// A key that is not of its scheme's form is an error wrapping ErrMalformed; a weak Ed25519 key (see
// CheckEd25519PublicKey) wraps ErrWeakKey as well.
func NewVerifier(alg Alg, pub []byte) (Verifier, error) {
	switch alg {
	case AlgEd25519:
		if len(pub) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("audit: ed25519 public key is %d bytes, want %d: %w", len(pub), ed25519.PublicKeySize, ErrMalformed)
		}
		// A weak key (small order, mixed order, or not canonically encoded) verifies forged
		// signatures or gives one secret a second identity, so it is not a verification key.
		if err := CheckEd25519PublicKey(pub); err != nil {
			return nil, fmt.Errorf("%w (%w)", err, ErrMalformed)
		}
		return Ed25519Verifier{Pub: bytes.Clone(pub)}, nil
	case AlgMLDSA65:
		pk, err := mldsa.NewPublicKey(mldsa.MLDSA65(), pub)
		if err != nil {
			return nil, fmt.Errorf("audit: ml-dsa-65 public key: %v: %w", err, ErrMalformed)
		}
		return MLDSAVerifier{Pub: pk}, nil
	case AlgHybrid:
		ed, ml, ok := splitHybrid(pub)
		if !ok {
			return nil, fmt.Errorf("audit: hybrid public key has no valid length prefix: %w", ErrMalformed)
		}
		edV, err := NewVerifier(AlgEd25519, ed)
		if err != nil {
			return nil, err
		}
		mlV, err := NewVerifier(AlgMLDSA65, ml)
		if err != nil {
			return nil, err
		}
		return HybridVerifier{Ed: edV.(Ed25519Verifier), ML: mlV.(MLDSAVerifier)}, nil
	default:
		return nil, fmt.Errorf("audit: unknown signature scheme %q: %w", alg, ErrMalformed)
	}
}

// VerifierOf returns the verifier for s's public key: NewVerifier(s.Alg(), s.PublicKey()).
func VerifierOf(s Signer) (Verifier, error) {
	if s == nil {
		return nil, fmt.Errorf("audit: no signer: %w", agent.ErrConfig)
	}
	return NewVerifier(s.Alg(), s.PublicKey())
}

// FormatPublicKey returns the text form of a public key that ParsePublicKey reads: the scheme, a
// colon, and the encoded key in lowercase hex ("ed25519:1a2b...").
func FormatPublicKey(alg Alg, pub []byte) string { return string(alg) + ":" + hex.EncodeToString(pub) }

// ParsePublicKey reads the text form of a public key (FormatPublicKey) and returns its verifier. A
// bare hex string of 32 bytes is read as an Ed25519 key. Surrounding white space is ignored. Any
// other text is an error wrapping ErrMalformed.
func ParsePublicKey(s string) (Verifier, error) {
	s = strings.TrimSpace(s)
	alg, keyHex, ok := strings.Cut(s, ":")
	if !ok {
		alg, keyHex = string(AlgEd25519), s
	}
	pub, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, fmt.Errorf("audit: public key must be <scheme>:<hex> or ed25519 hex: %v: %w", err, ErrMalformed)
	}
	return NewVerifier(Alg(alg), pub)
}

// checkSigner refuses a nil signer and one without a usable key, so a producer fails before it
// signs rather than emitting an artifact nothing verifies.
func checkSigner(s Signer) error {
	if s == nil {
		return fmt.Errorf("audit: no signer: %w", agent.ErrConfig)
	}
	if len(s.PublicKey()) == 0 {
		var why error
		switch k := s.(type) {
		case Ed25519Signer:
			why = checkPrivateKey(k.Priv)
		case MLDSASigner:
			why = k.check()
		case HybridSigner:
			if why = checkPrivateKey(k.Ed.Priv); why == nil {
				why = k.ML.check()
			}
		}
		if why != nil {
			return fmt.Errorf("audit: the %s signer holds no usable key: %w", s.Alg(), why)
		}
		return fmt.Errorf("audit: the %s signer holds no usable key: %w", s.Alg(), agent.ErrConfig)
	}
	return nil
}

// checkVerifier refuses a nil verifier.
func checkVerifier(v Verifier) error {
	if v == nil {
		return fmt.Errorf("audit: no verifier: %w", agent.ErrConfig)
	}
	return nil
}

// ---- ed25519 ----

// Ed25519Signer signs with an Ed25519 private key.
type Ed25519Signer struct {
	Priv ed25519.PrivateKey // the Ed25519 private key that signs
}

// Alg reports the signature scheme (AlgEd25519).
func (Ed25519Signer) Alg() Alg { return AlgEd25519 }

// PublicKey returns the 32-byte Ed25519 public key, or nil if the private key has the wrong length.
func (s Ed25519Signer) PublicKey() []byte {
	if len(s.Priv) != ed25519.PrivateKeySize {
		return nil
	}
	return bytes.Clone(s.Priv.Public().(ed25519.PublicKey))
}

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
func (Ed25519Verifier) Alg() Alg { return AlgEd25519 }

// PublicKey returns the public key, or nil if it has the wrong length.
func (v Ed25519Verifier) PublicKey() []byte {
	if len(v.Pub) != ed25519.PublicKeySize {
		return nil
	}
	return bytes.Clone(v.Pub)
}

// Verify reports whether sig is a valid Ed25519 signature over m. A weak public key (see
// CheckEd25519PublicKey: the wrong length, not canonically encoded, or not in the prime-order
// subgroup) verifies nothing, since a small-order key accepts forged signatures; it never panics.
func (v Ed25519Verifier) Verify(m, sig []byte) bool {
	return CheckEd25519PublicKey(v.Pub) == nil && ed25519.Verify(v.Pub, m, sig)
}

// KeyIDs identifies the public key, for agent.ApproverVerifier: one entry, KeyID(AlgEd25519,
// Pub). A weak public key (see CheckEd25519PublicKey) verifies nothing and reports no entry, so an
// approval gate refuses it as a configuration error rather than seating it. This also keeps one
// secret to one identity: a mixed-order key A + T, which A's secret can sign for, and a
// non-canonical spelling of A are both refused.
func (v Ed25519Verifier) KeyIDs() []string {
	if CheckEd25519PublicKey(v.Pub) != nil {
		return nil
	}
	return []string{KeyID(string(AlgEd25519), v.Pub)}
}

// ---- ML-DSA-65 (FIPS 204) ----

// MLDSASigner signs with an ML-DSA-65 private key. Signing is deterministic, so replaying the
// same tree head reproduces the same signature.
type MLDSASigner struct {
	Priv *mldsa.PrivateKey // the ML-DSA-65 private key that signs
}

// Alg reports the signature scheme (AlgMLDSA65).
func (MLDSASigner) Alg() Alg { return AlgMLDSA65 }

// PublicKey returns the encoded ML-DSA-65 public key, or nil if there is no key or it is of
// another ML-DSA parameter set.
func (s MLDSASigner) PublicKey() []byte {
	if s.check() != nil {
		return nil
	}
	return s.Priv.PublicKey().Bytes()
}

func (s MLDSASigner) check() error {
	if s.Priv == nil {
		return fmt.Errorf("audit: no ml-dsa-65 private key: %w", agent.ErrConfig)
	}
	if p := s.Priv.PublicKey().Parameters(); p != mldsa.MLDSA65() {
		return fmt.Errorf("audit: ml-dsa private key is %s, want ML-DSA-65: %w", p, agent.ErrConfig)
	}
	return nil
}

// Sign returns the deterministic ML-DSA-65 signature over m, or an error if there is no key or it
// is of another parameter set.
func (s MLDSASigner) Sign(m []byte) ([]byte, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	return s.Priv.SignDeterministic(m, &mldsa.Options{Context: mldsaContext})
}

// MLDSAVerifier verifies with an ML-DSA-65 public key.
type MLDSAVerifier struct {
	Pub *mldsa.PublicKey // the ML-DSA-65 public key that verifies
}

// Alg reports the signature scheme (AlgMLDSA65).
func (MLDSAVerifier) Alg() Alg { return AlgMLDSA65 }

// PublicKey returns the encoded public key, or nil if there is none or it is not ML-DSA-65.
func (v MLDSAVerifier) PublicKey() []byte {
	if v.Pub == nil || v.Pub.Parameters() != mldsa.MLDSA65() {
		return nil
	}
	return v.Pub.Bytes()
}

// Verify reports whether sig is a valid ML-DSA-65 signature over m. A verifier with no key, or a
// key of another parameter set, verifies nothing.
func (v MLDSAVerifier) Verify(m, sig []byte) bool {
	if v.Pub == nil || v.Pub.Parameters() != mldsa.MLDSA65() {
		return false
	}
	return mldsa.Verify(v.Pub, m, sig, &mldsa.Options{Context: mldsaContext}) == nil
}

// KeyIDs identifies the public key, for agent.ApproverVerifier: one entry, KeyID(AlgMLDSA65,
// Pub.Bytes()). A verifier with no key, or a key of another ML-DSA parameter set, verifies nothing
// (see Verify) and reports no entry, so an approval gate refuses it rather than seating it.
func (v MLDSAVerifier) KeyIDs() []string {
	if v.Pub == nil || v.Pub.Parameters() != mldsa.MLDSA65() {
		return nil
	}
	return []string{KeyID(string(AlgMLDSA65), v.Pub.Bytes())}
}

// ---- hybrid ed25519 + ML-DSA-65 ----

// HybridSigner signs with both schemes; the signature is only accepted if BOTH verify, so the
// anchor is safe as long as either scheme remains unbroken. Each component signs its own label
// followed by the message ("bide.hybrid.ed25519.v1\x00" || m and "bide.hybrid.mldsa65.v1\x00" || m),
// so a component stripped out of a hybrid signature is not a signature over m under its scheme.
type HybridSigner struct {
	Ed Ed25519Signer // the Ed25519 component signer
	ML MLDSASigner   // the ML-DSA-65 component signer
}

// Alg reports the signature scheme (AlgHybrid).
func (HybridSigner) Alg() Alg { return AlgHybrid }

// PublicKey returns the hybrid public key: a 4-byte big-endian length of the Ed25519 key, the
// Ed25519 key, then the ML-DSA-65 key. It is nil if either component has no usable key.
func (s HybridSigner) PublicKey() []byte {
	ed, ml := s.Ed.PublicKey(), s.ML.PublicKey()
	if ed == nil || ml == nil {
		return nil
	}
	return joinHybrid(ed, ml)
}

// Sign returns the packed pair of domain-separated Ed25519 and ML-DSA-65 signatures over m.
func (s HybridSigner) Sign(m []byte) ([]byte, error) {
	e, err := s.Ed.Sign(hybridMessage(hybridEd25519Label, m))
	if err != nil {
		return nil, err
	}
	d, err := s.ML.Sign(hybridMessage(hybridMLDSA65Label, m))
	if err != nil {
		return nil, err
	}
	return joinHybrid(e, d), nil
}

// HybridVerifier requires both component signatures to verify, each over its own label and the
// message.
type HybridVerifier struct {
	Ed Ed25519Verifier // the Ed25519 component verifier
	ML MLDSAVerifier   // the ML-DSA-65 component verifier
}

// Alg reports the signature scheme (AlgHybrid).
func (HybridVerifier) Alg() Alg { return AlgHybrid }

// PublicKey returns the hybrid public key (see HybridSigner.PublicKey), or nil if either component
// has no usable key.
func (v HybridVerifier) PublicKey() []byte {
	ed, ml := v.Ed.PublicKey(), v.ML.PublicKey()
	if ed == nil || ml == nil {
		return nil
	}
	return joinHybrid(ed, ml)
}

// Verify reports whether both component signatures over m are valid.
func (v HybridVerifier) Verify(m, sig []byte) bool {
	e, d, ok := splitHybrid(sig)
	if !ok {
		return false
	}
	return v.Ed.Verify(hybridMessage(hybridEd25519Label, m), e) && v.ML.Verify(hybridMessage(hybridMLDSA65Label, m), d)
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

// hybridMessage is what one hybrid component signs: its label, then the message.
func hybridMessage(label string, m []byte) []byte {
	return append([]byte(label), m...)
}

// joinHybrid packs two values as len32(a) || a || b, so the split is unambiguous. It packs both the
// signature pair and the public key pair.
func joinHybrid(a, b []byte) []byte {
	out := make([]byte, 0, 4+len(a)+len(b))
	out = binary.BigEndian.AppendUint32(out, uint32(len(a)))
	out = append(out, a...)
	return append(out, b...)
}

// splitHybrid undoes joinHybrid.
func splitHybrid(sig []byte) (a, b []byte, ok bool) {
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
		return fmt.Errorf("audit: ed25519 private key is %d bytes, want %d: %w", len(priv), ed25519.PrivateKeySize, agent.ErrConfig)
	}
	return nil
}
