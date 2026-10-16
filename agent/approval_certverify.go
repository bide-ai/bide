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
	// Window is W', the compromise-reporting window the status rule left the decision (a duration string).
	Window string
	// Warnings are findings that do not change the outcome (a series gap against the declared period).
	Warnings []string
	// Chain is the SHA-256 (lowercase hex) of each certificate of the accepted chain, leaf first,
	// Anchor that of the trust anchor's SubjectPublicKeyInfo the chain ends at, and HeadSize the size
	// of the signed head that first covers the decision: what the gate records in ApprovalIdentity.
	Chain    []string
	Anchor   string
	HeadSize int
	// HeadRoot is the Merkle root of that head (hex) and HeadHash the SHA-256 of its signed canonical
	// encoding (hex), so an independent verifier can tell which head the time rests on.
	HeadRoot, HeadHash string
	// TimeEvidence is the SHA-256 (hex) of the RFC 3161 token or anchor receipt behind A(d); empty when
	// the time is the log signer's clock.
	TimeEvidence string
	// BasisKind is the status basis as a value a viewer can branch on: BasisAtOrAfterDecision or
	// BasisLiveBeforeDecision.
	BasisKind string
	// Evidence are the revocation status records (CRLs) the finding rests on, each once: the gate
	// stores them by hash (ApprovalStatusStep) so what it knew survives the loss of an archive.
	Evidence []ApprovalStatusBytes
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
	// IdentityMappingConfigured reports whether the bank supplied the mapping from requesters to
	// identifiers. Without it the roster's default mapping (a principal equal to an approver id) only
	// repeats the exclusion by approver id, so a requester it cannot map is not worth a warning; with
	// it, a requester it cannot map is.
	IdentityMappingConfigured() bool
	// RosterDigest is the SHA-256 over the whole identity map (see CertApprover.RosterDigest). The
	// certificate-bound approvers of one gate must report one value: a gate has one identity map.
	RosterDigest() string
}

// IdentityDescriber is implemented by the verifier of a certificate-bound approver that can describe
// the certificate policy in force, for the identity record a gate journals with its terminal count:
// DescribeIdentity is the policy and status values, PolicyDigest the SHA-256 (lowercase hex) of the
// JSON of that description (the value the pause note and the record both carry), and RosterDigest the
// SHA-256 over the whole identity map in sorted approver order, each entry len32(approver) approver
// len32(identifier) identifier.
type IdentityDescriber interface {
	DescribeIdentity() IdentityPolicyRecord
	PolicyDigest() string
	RosterDigest() string
	// VerifierInfo is the revision the verifier was built from (vcs.revision) and the x509 GODEBUG
	// settings it runs under, for the record: the settings are explicit configuration, never read from
	// the process environment by the engine.
	VerifierInfo() (revision, goDebug string)
}
