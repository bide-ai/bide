package agent

import (
	"time"
)

// A certificate-bound approver (docs/design/approver-identity.md) is an approver whose verifier
// implements CertDecisionVerifier. The counting rule decides how to check a decision by the kind of
// the approver's verifier, never by what the record carries: a bare-key verifier checks the v3 bytes
// and never accepts a v4 signature, a certificate-bound verifier checks a v4 decision with its chain
// and never accepts a v3 one.

// CertDecision is one decision record presented to a certificate-bound approver's verifier.
type CertDecision struct {
	// Subject is the call the gate counts for.
	Subject ApprovalSubject
	// Record is the decision record: approver, verdict, scheme, decided_at, signature and chain.
	Record Record
	// Journal is the run's journal from its first record, as Journal.History returns it, and Index the
	// position of Record in it: what a verifier needs to find the signed tree head that first covers
	// the record. Journal is nil when the caller does not hold the run's journal; the verifier then
	// cannot establish when the decision became provable and refuses it.
	Journal []Record
	Index   int
}

// CertCheck is a certificate-bound verifier's finding about one decision. Reason is empty when the
// decision verifies and counts; otherwise it is one of the Reason constants (a certificate or status
// reason, ReasonBadSig, ReasonAlg and so on) and Detail says precisely what failed. The other fields
// describe a decision that verified, for the verdict and the identity record.
type CertCheck struct {
	Reason string
	Detail string
	// Identity is the verified canonical identifier of the leaf's one subject alternative name.
	Identity string
	// KeyHash is the SHA-256 of the leaf's public key info: two counted decisions by different
	// approver ids with one key hash do not both count (ReasonSharedKey).
	KeyHash [32]byte
	// Anchored is when the decision became provable (A(d)), by the source Time names.
	Anchored time.Time
	Time     string
	// Tier ("final", "provisional", "no information"), Basis and StatusTime (S, RFC 3339) say what the
	// revocation status behind the decision is and rests on; only a "final" tier is a verified one.
	Tier       string
	Basis      string
	StatusTime string
	// Warnings are findings that do not change the outcome (a series gap against the declared period).
	Warnings []string
}

// CertDecisionVerifier is implemented by the verifier of a certificate-bound approver. It extends
// ApproverVerifier: Alg names the family ("x509"), Verify is false (a bare signature over v3 bytes is
// never a certificate-bound decision), and KeyIDs is the seat, "ident:" plus the approver's canonical
// identity, so the one-seat-per-key check (ValidateKeys) applies to a person, not to a key.
type CertDecisionVerifier interface {
	ApproverVerifier
	// VerifyCertDecision checks one decision record: that it carries a chain, that the chain is
	// accepted for this approver at the time the decision became provable, that the signature is over
	// the v4 bytes of this exact call under the leaf's key, and that revocation status allows it.
	VerifyCertDecision(cd CertDecision) CertCheck
}

// AlgCertificate is the Alg a certificate-bound verifier reports.
const AlgCertificate Alg = "x509"

// IdentityRoster is implemented by the verifier of a certificate-bound approver that knows the policy's
// identity map. A gate with a certificate-bound approver requires the map to name every approver of the
// policy, bare-key ones included: an approver outside it could be the same person as another, and one
// person would fill two seats.
type IdentityRoster interface {
	// HasIdentity reports whether the identity map names the approver id.
	HasIdentity(approverID string) bool
	// RequesterIdentifier maps a requester principal (as the run records it) to the canonical identifier
	// of the person it names, so separation of duties can exclude the requester by identifier. ok is
	// false when the principal cannot be mapped.
	RequesterIdentifier(principal string) (identifier string, ok bool)
}
