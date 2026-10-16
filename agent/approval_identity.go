package agent

import (
	"fmt"
	"strings"
)

// The journal records of a gate with a certificate-bound approver (docs/design/approver-identity.md,
// "Journal and format changes"). They are written beside the gate's terminal tally and read by viewers
// and the auditor; the gate never reads them to decide. All are insert-once, decoded leniently (an
// unknown member is ignored), and written with encoding/json's field order and sorted map keys.
//
// ApprovalIdentity is a claim by the live gate: its tier, status time and basis are what the gate
// computed at counting, with the clock set. An audit recomputes every decision from the decision
// records, heads, chains and status bytes with no clock, and compares; it never takes these as input.
//
// A gate has generations: generation 0 is its first count, and each exec hold (ExecHold) makes the next
// count a new generation. The identity record and the hold are named by generation, so a re-asked
// gate's second count has its own record.

// ApprovalIdentityStep is the journal name of the ApprovalIdentity record of toolUseID's gate at
// generation gen.
func ApprovalIdentityStep(toolUseID string, gen int) string {
	return fmt.Sprintf("approval-identity:%s:%d", encodeID(toolUseID), gen)
}

// ApprovalIdentityDigestStep is the name of the identity record of generation gen when the plain name
// already holds a record of a different tally (a crash between the record and the tally, then a
// different count): the record whose TallyDigest is the one the gate then writes is found under this
// name, digest being the first 16 hex digits of that digest.
func ApprovalIdentityDigestStep(toolUseID string, gen int, digest string) string {
	return ApprovalIdentityStep(toolUseID, gen) + ":" + digest[:min(16, len(digest))]
}

// ApprovalStatusStep is the journal name of the ApprovalStatusBytes record whose DER hashes (SHA-256,
// lowercase hex) to hash. One record per run and hash, shared by every call of the run that relied on
// those bytes.
func ApprovalStatusStep(hash string) string { return "approval-status:" + hash }

// ApprovalExecHoldStep is the journal name of the ExecHold record of generation gen of toolUseID's gate.
func ApprovalExecHoldStep(toolUseID string, gen int) string {
	return fmt.Sprintf("approval-exec-hold:%s:%d", encodeID(toolUseID), gen)
}

// ApprovalIdentTallyStep is the journal name of the terminal tally of an identity-bound gate at
// generation gen: ApprovalTallyStep plus ":ident" for generation 0 and ":ident-reask:<gen>" for a
// gate re-asked after an exec hold. The tally is the commit point of a count: the status records and
// the identity record are written before it.
func ApprovalIdentTallyStep(toolUseID string, gen int) string {
	if gen <= 0 {
		return ApprovalTallyStep(toolUseID) + ":ident"
	}
	return fmt.Sprintf("%s:ident-reask:%d", ApprovalTallyStep(toolUseID), gen)
}

// identGen returns the generation of toolUseID's gate in recs: the number of exec holds the call has.
func identGen(recs []Record, toolUseID string) int {
	prefix := "approval-exec-hold:" + encodeID(toolUseID) + ":"
	n := 0
	for _, r := range recs {
		if r.Kind == StepValue && strings.HasPrefix(r.Name, prefix) {
			n++
		}
	}
	return n
}

// Formats of the identity records.
const (
	ApprovalIdentityFormat   = "bide.approval-identity.v2"
	ApprovalStatusFormat     = "bide.approval-status.v2"
	ApprovalExecHoldFormat   = "bide.approval-exec-hold.v2"
	ApprovalIdentityRulesVer = "v1"
	// EvaluatedLive is ApprovalIdentity.Evaluated of a record the live gate wrote.
	EvaluatedLive = "live"
	// MaxStatusInline is the largest status record (a CRL's DER) stored in the journal; above it the
	// record holds the metadata and the hash, and the bytes go to the policy's status archive.
	MaxStatusInline = 256 << 10
)

// Pause phases (ApprovalWait.Pause): why a gate with a certificate-bound approver is waiting. The
// phase is a hint; a viewer decides from the authoritative records (result, hold, tally).
// A counted decision whose status is only provisional is not a pause: it executes, and its tier is in
// the identity record.
const (
	PauseAwaitingDecisions = "awaiting-decisions" // fewer valid decisions than needed (also the empty value)
	PauseAwaitingAnchor    = "awaiting-anchor"    // a decision that verifies has no covering signed head yet
	PauseAwaitingStatus    = "awaiting-status"    // revocation status is missing, stale or out of window
	PauseHeldUntilFinal    = "held-until-final"   // counted, only provisional, and the policy requires final to run
	PauseExecHold          = "exec-hold"          // the pre-execution status check holds the call
)

// Identity tiers (IdentityDecision.Tier). A decision that is not counted does not appear.
const (
	TierIndependentFinal = "independent-final" // status final, time from an independent source
	TierLogclockFinal    = "logclock-final"    // status final, time from the log signer's clock only
	TierProvisional      = "provisional"       // a later status could change it, the window is short, or the series has a gap
)

// Status bases (IdentityDecision.BasisKind).
const (
	BasisAtOrAfterDecision  = "at-or-after-decision" // status issued at or after the decision
	BasisLiveBeforeDecision = "live-before-decision" // a live count on status issued before the decision (provisional only)
)

// Time sources (IdentityDecision.TimeSource).
const (
	TimeSourceLogSignerClock = "log-signer-clock"
	TimeSourceRFC3161        = "rfc3161"
	TimeSourceAnchorReceipt  = "anchor-receipt"
)

// ApprovalIdentity is what the record named ApprovalIdentityStep holds: the identity environment of a
// terminal count. It holds the policy in force, a digest of the identity roster, and for each counted
// certificate-bound decision the accepted chain, when and from what it became provable, its tier and
// basis, and the hashes of the status bytes relied on (ApprovalStatusBytes). The certificates are in the
// decision records. Decisions covers certificate-bound decisions only: the one-seat and requester
// checks that also involve bare-key decisions are covered by RosterDigest and the auditor's expected
// policy of record.
type ApprovalIdentity struct {
	Format      string               `json:"format"`
	ToolUseID   string               `json:"tool_use_id"`
	Generation  int                  `json:"generation"`
	Tally       string               `json:"tally"`        // the journal name of the tally this record belongs to
	TallyDigest string               `json:"tally_digest"` // SHA-256 of the exact JSON of that tally
	Evaluated   string               `json:"evaluated"`    // EvaluatedLive
	EvaluatedAt string               `json:"evaluated_at"` // the gate's clock, RFC 3339 with nanoseconds
	Policy      IdentityPolicyRecord `json:"policy"`
	// PolicyDigest is the SHA-256 of Policy's JSON, written without HTML escaping, with fields in
	// declaration order, map keys sorted and durations as time.Duration.String; it equals the pause
	// note's IdentityPolicyDigest.
	PolicyDigest string `json:"policy_digest"`
	// RosterDigest is the SHA-256 over the whole identity map in sorted approver order, each entry
	// len32(approver) approver len32(identifier) identifier.
	RosterDigest string             `json:"roster_digest"`
	Decisions    []IdentityDecision `json:"decisions"` // the counted certificate-bound decisions, in policy order
	Rules        string             `json:"rules"`
	Verifier     string             `json:"verifier"`               // runtime.Version() of the verifier
	VerifierRev  string             `json:"verifier_rev,omitempty"` // the build's vcs.revision, when known
	GoDebug      string             `json:"godebug,omitempty"`      // the x509 GODEBUG settings in force
	RecordedAt   string             `json:"recorded_at,omitempty"`
}

// IdentityPolicyRecord is the certificate policy and status values in force when a gate counted, every
// one that reaches a verdict. Durations are time.Duration.String; times are RFC 3339 with nanoseconds;
// anchors and intermediates are named by the SHA-256 of their SubjectPublicKeyInfo. EffectiveFrom is
// the operator's change-record date (CertApprover.PolicyEffectiveFrom), informational: the auditor's
// expected policy of record dates the policy, not this field.
type IdentityPolicyRecord struct {
	EffectiveFrom           string            `json:"effective_from"`
	Anchors                 []string          `json:"anchors"`
	Intermediates           []string          `json:"intermediates"`
	Distrust                map[string]string `json:"distrust,omitempty"`
	EKU                     string            `json:"eku"`
	PolicyOID               string            `json:"policy_oid,omitempty"`
	IDKind                  string            `json:"id_kind"`
	Canonical               string            `json:"canonical"` // the canonicalizer's name and version
	AllowEmailProtectionEKU bool              `json:"allow_email_protection_eku,omitempty"`
	RequireNameConstraints  bool              `json:"require_name_constraints,omitempty"`
	RequirePolicyOnCAs      bool              `json:"require_policy_on_cas,omitempty"`
	RSA2048Until            string            `json:"rsa2048_until,omitempty"`
	MaxChain                int               `json:"max_chain"`
	MaxLeafLifetime         string            `json:"max_leaf_lifetime"`
	Skew                    string            `json:"skew"`
	AnchorGrace             string            `json:"anchor_grace"`
	ClockTolerance          string            `json:"clock_tolerance"`
	W                       string            `json:"w"`
	MinW                    string            `json:"min_w"`
	RoutineGrace            string            `json:"routine_grace,omitempty"`
	MaxStatusAge            string            `json:"max_status_age"`
	ExecStatusAge           string            `json:"exec_status_age"`
	ExecStatusAgeFor        map[string]string `json:"exec_status_age_for,omitempty"` // per issuing CA (issuer SPKI hash)
	MaxStatusAgeFor         map[string]string `json:"max_status_age_for,omitempty"`  // per issuing CA
	CAPeriod                string            `json:"ca_period"`
	CAPeriodFor             map[string]string `json:"ca_period_for,omitempty"`
	CoverageTolerance       string            `json:"coverage_tolerance"`
	ThisUpdateSkew          string            `json:"this_update_skew"`
	ThisUpdateSkewFor       map[string]string `json:"this_update_skew_for,omitempty"`
	AllowHolds              bool              `json:"allow_holds,omitempty"`
	ConsecutiveNumbers      bool              `json:"consecutive_numbers,omitempty"`
	CountLive               bool              `json:"count_live,omitempty"`
	Retained                bool              `json:"retained,omitempty"`
	IndependentTime         bool              `json:"independent_time,omitempty"`
}

// IdentityDecision is one counted certificate-bound decision in an ApprovalIdentity.
type IdentityDecision struct {
	Approver   string   `json:"approver"`
	Step       string   `json:"step"`       // the decision record's journal name
	Identifier string   `json:"identifier"` // the verified canonical identifier; the seat is "ident:" plus it
	LeafHash   string   `json:"leaf_hash"`  // SHA-256 of the leaf certificate's DER
	Chain      []string `json:"chain"`      // SHA-256 of each certificate of the accepted chain, leaf first
	Anchor     string   `json:"anchor"`     // SHA-256 of the trust anchor's SubjectPublicKeyInfo the chain ends at
	// AnchoredAt is A(d); compare it to a recomputed time as an instant, not as a string.
	AnchoredAt   string   `json:"anchored_at"`
	TimeSource   string   `json:"time_source"`             // a TimeSource constant
	TimeEvidence string   `json:"time_evidence,omitempty"` // SHA-256 of the RFC 3161 token or anchor receipt; empty for the log clock
	HeadSize     int      `json:"head_size"`               // the covering head: size,
	HeadRoot     string   `json:"head_root"`               // its Merkle root,
	HeadHash     string   `json:"head_hash"`               // and the SHA-256 of its signed canonical encoding (audit.SignedTreeHead.Hash)
	Tier         string   `json:"tier"`                    // a Tier constant
	BasisKind    string   `json:"basis_kind"`              // a Basis constant; a viewer branches on this, never on Basis
	Basis        string   `json:"basis"`                   // display text
	StatusTime   string   `json:"status_time,omitempty"`   // S
	Window       string   `json:"window,omitempty"`        // W'
	Status       []string `json:"status,omitempty"`        // SHA-256 of the status bytes relied on (ApprovalStatusStep names)
	Warnings     []string `json:"warnings,omitempty"`
}

// ApprovalStatusBytes is what an ApprovalStatusStep record holds: one CRL (or OCSP response) a gate
// relied on. The metadata is informational (a reader parses DER and ignores the metadata where they
// differ); the writer checks that SHA-256 of DER is the record's name. A CRL larger than
// MaxStatusInline is recorded without DER, and its bytes go to the policy's status archive.
type ApprovalStatusBytes struct {
	Format     string `json:"format"`
	Kind       string `json:"kind"` // "crl" or "ocsp"
	Issuer     string `json:"issuer"`
	Number     string `json:"number,omitempty"` // the CRLNumber, decimal
	ThisUpdate string `json:"this_update"`
	NextUpdate string `json:"next_update,omitempty"`
	Size       int    `json:"size"`          // len of the original DER
	DER        []byte `json:"der,omitempty"` // base64 in JSON; omitted above MaxStatusInline
}

// ExecHold is what an ApprovalExecHoldStep record holds: the pre-execution status check found that the
// call may not run, with the evidence an operator decides on. Detail is our own words, never text
// taken from a certificate. Approvers lists every counted decision that no longer holds.
type ExecHold struct {
	Format       string   `json:"format"`
	ToolUseID    string   `json:"tool_use_id"`
	Generation   int      `json:"generation"`
	Reason       string   `json:"reason"` // ReasonRevoked, ReasonStatusStale, ReasonStatusUnknown, ReasonNoCoveringHead or ReasonSharedKey
	Detail       string   `json:"detail"`
	Approvers    []string `json:"approvers,omitempty"`
	CheckedAt    string   `json:"checked_at"`
	StatusTime   string   `json:"status_time,omitempty"`
	Serial       string   `json:"serial,omitempty"`      // decimal serial of the revoked certificate
	E            string   `json:"e,omitempty"`           // its effective revocation time
	ReasonCode   int      `json:"reason_code"`           // the CRL reason code, -1 if none
	CRLHash      string   `json:"crl_hash,omitempty"`    // SHA-256 of the CRL that lists it (an approval-status name)
	AnchoredAt   string   `json:"anchored_at,omitempty"` // A(d) of the decision
	PolicyDigest string   `json:"policy_digest,omitempty"`
}
