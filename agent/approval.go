package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
)

// This file holds the m-of-n approval gate's data model: the policy check, what an approver
// signs, how decisions are journaled, and the counting rule. The counting rule
// (TallyApprovals) is exported and pure so the gate at run time and an auditor offline run
// the same function; see audit.VerifyApprovals.

// Validate reports whether the policy is well formed: at least one approver, every approver id
// non-empty printable ASCII, no two ids the same approver, and 1 <= Need <= len(Approvers). A
// policy that fails is a configuration error; the gate refuses it with ErrConfig rather than
// guessing.
//
// An approver id is printable ASCII, bytes 0x21 to 0x7E: letters, digits and punctuation, so an
// email address or a URI is a valid id, and a space, a control character or anything outside
// ASCII is not. An id outside that set could look like another (a Cyrillic "о" for a Latin "o", a
// fullwidth or accented spelling) and let one approver fill two seats. Ids are compared as exact
// bytes everywhere else (eligibility, the signed decision bytes, the verifier lookup), but a
// policy may not list two ids that differ only by ASCII case ("alice" and "Alice"): a person
// reading the policy, or a key lookup that folds case, takes them for one approver.
//
// ExcludePrincipal is valid on an m-of-n policy and ErrConfig on SingleApproval. Whether Need stays
// reachable without the requester depends on the run (who requested it), so the gate checks that
// when it evaluates a call, not here: a policy whose Need equals the number of approvers can
// exclude the requester only for runs whose requester is not one of them.
func (p ApprovalPolicy) Validate() error {
	if p.one { // SingleApproval, unless its fields were changed since
		if p.Need != 1 || len(p.Approvers) != 0 {
			return fmt.Errorf("a SingleApproval policy was changed to Need %d and %d approvers; build an m-of-n policy instead: %w", p.Need, len(p.Approvers), ErrConfig)
		}
		if p.ExcludePrincipal {
			return fmt.Errorf("a SingleApproval policy cannot exclude the principal: its decision (Approve) names no approver; build an m-of-n policy instead: %w", ErrConfig)
		}
		return nil
	}
	if len(p.Approvers) == 0 {
		return fmt.Errorf("approval policy has no approvers: %w", ErrConfig)
	}
	seen := make(map[string]string, len(p.Approvers)) // fold key -> the first id with it
	for _, id := range p.Approvers {
		if id == "" {
			return fmt.Errorf("approval policy has an empty approver id: %w", ErrConfig)
		}
		if !printableASCII(id) {
			return fmt.Errorf("approval policy approver id %q is not printable ASCII (bytes 0x21 to 0x7E, no space): %w", id, ErrConfig)
		}
		key := strings.ToLower(id)
		if first, dup := seen[key]; dup {
			if first == id {
				return fmt.Errorf("approval policy lists approver %q twice: %w", id, ErrConfig)
			}
			return fmt.Errorf("approval policy lists approvers %q and %q, which differ only by case: %w", first, id, ErrConfig)
		}
		seen[key] = id
	}
	if p.Need < 1 || p.Need > len(p.Approvers) {
		return fmt.Errorf("approval policy Need = %d, want 1 <= Need <= %d approvers: %w", p.Need, len(p.Approvers), ErrConfig)
	}
	return nil
}

// printableASCII reports whether every byte of id is printable ASCII other than space (0x21 to
// 0x7E). For such an id, strings.ToLower is ASCII case folding.
func printableASCII(id string) bool {
	for i := 0; i < len(id); i++ {
		if id[i] < 0x21 || id[i] > 0x7e {
			return false
		}
	}
	return true
}

// ApprovalSubject is exactly what an approver decides on: one tool call, with its name and
// arguments, in one run. The decision bytes bind all of it, so a signature approves this call
// and no other: not another run, not another call, and not the same call id with different
// arguments.
type ApprovalSubject struct {
	RunID     string
	ToolUseID string
	ToolName  string
	Args      json.RawMessage
}

// Subject returns the approval subject of the paused call, which is what approvers sign:
//
//	sig, err := signer.Sign(agent.ApprovalDecisionBytes(pend.Subject(), "finance", true, decidedAt))
//
// For a gate inside a sub-agent, RunID is the sub-run's id.
func (e *ApprovalPending) Subject() ApprovalSubject {
	return ApprovalSubject{RunID: e.RunID, ToolUseID: e.ToolUseID, ToolName: e.ToolName, Args: e.Args}
}

// approvalDomain tags the bytes an approver signs so a signature over a decision can never
// be replayed as a signature over any other message the same key signs. It is also the
// encoding's version, covered by the signature: v3 binds the approver's decision time; v2 bound
// the call's tool name and arguments; v1 bound only the call id. A decision signed under an
// earlier version does not verify under this one.
const approvalDomain = DecisionFormat + "\n"

// DecisionFormat names the signed decision format this version writes and accepts: the domain tag
// of ApprovalDecisionBytes without its newline. A gate's journaled tally records it.
const DecisionFormat = "bide.approval.v3"

// ApprovalDecisionBytes returns the canonical bytes an approver signs and every verifier
// checks. decidedAt is when the approver decided, by their own clock; it is signed in
// FormatTimestamp's form, so pass the same time as Decision.DecidedAt. The layout, stable and
// reproducible outside Go:
//
//	"bide.approval.v3\n"
//	len32(RunID) RunID
//	len32(ToolUseID) ToolUseID
//	len32(ToolName) ToolName
//	SHA-256(canonical args), 32 bytes
//	len32(approverID) approverID
//	1 byte: 1 approved, 0 denied
//	len32(decidedAt) decidedAt
//
// len32 is a 4-byte big-endian length, so no two distinct decisions encode to the same bytes.
// The canonical args are the arguments parsed as JSON and re-serialized with object keys
// sorted, no insignificant whitespace, no HTML escaping, and number literals kept verbatim;
// empty arguments are "{}", and bytes that are not a single JSON value are used as-is. The
// canonical form makes the signature independent of how the arguments were formatted in
// transit (a live model stream, the journal, an evidence file). decidedAt is RFC 3339 in UTC to
// the millisecond, 24 bytes, e.g. "2026-10-13T12:14:03.123Z" (FormatTimestamp); a verifier
// refuses a decision whose recorded time is in any other form (ParseTimestamp), so an instant
// has one signed spelling. A time outside the years 0001 to 9999 has no such form, and a
// decision signed with one never counts. docs/design/design-mofn-approval.md works one decision
// through to its exact bytes, for a verifier written in another language.
func ApprovalDecisionBytes(s ApprovalSubject, approverID string, approved bool, decidedAt time.Time) []byte {
	return decisionBytes(s, approverID, approved, FormatTimestamp(decidedAt))
}

// decisionBytes is ApprovalDecisionBytes over decidedAt as text: a verifier rebuilds the signed
// bytes from the recorded text itself, after checking its form.
func decisionBytes(s ApprovalSubject, approverID string, approved bool, decidedAt string) []byte {
	args := sha256.Sum256(canonicalArgs(s.Args))
	b := make([]byte, 0, len(approvalDomain)+5*4+len(s.RunID)+len(s.ToolUseID)+len(s.ToolName)+len(args)+len(approverID)+1+len(decidedAt))
	b = append(b, approvalDomain...)
	for _, str := range []string{s.RunID, s.ToolUseID, s.ToolName} {
		b = binary.BigEndian.AppendUint32(b, uint32(len(str)))
		b = append(b, str...)
	}
	b = append(b, args[:]...)
	b = binary.BigEndian.AppendUint32(b, uint32(len(approverID)))
	b = append(b, approverID...)
	if approved {
		b = append(b, 1)
	} else {
		b = append(b, 0)
	}
	b = binary.BigEndian.AppendUint32(b, uint32(len(decidedAt)))
	return append(b, decidedAt...)
}

// canonicalArgs is the canonical form ApprovalDecisionBytes hashes (see its comment).
func canonicalArgs(raw json.RawMessage) []byte {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return trimmed
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return trimmed
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

// Alg names a signature scheme: "ed25519", "ecdsa-p256", "ml-dsa-65", or "ed25519+ml-dsa-65". The
// audit package defines the schemes (audit.AlgEd25519, audit.AlgECDSAP256, audit.AlgMLDSA65,
// audit.AlgHybrid) and their signers and
// verifiers; the agent package only journals the name, with each approver decision, so a decision is
// checked under the scheme it was signed with.
type Alg string

// ApproverVerifier checks an approver's signature over the decision bytes under one scheme and
// names the signing keys behind it. audit.Ed25519Verifier, audit.ECDSAP256Verifier,
// audit.MLDSAVerifier and audit.HybridVerifier implement it, so a deployment reuses the existing verifiers with no new
// cryptography and no agent->audit import.
type ApproverVerifier interface {
	// Alg names the verifier's scheme. A decision counts only if it was journaled under this scheme.
	Alg() Alg
	// Verify reports whether sig is a valid signature over message.
	Verify(message, sig []byte) bool
	// KeyIDs identifies the signing keys behind Verify: one entry per public key whose private
	// key takes part in producing a signature Verify accepts. A single-key verifier reports one
	// entry; a verifier that accepts a signature by any of several keys (a rotation window)
	// reports one per key; a verifier that requires several signatures (audit.HybridVerifier)
	// reports one per component key.
	//
	// An entry identifies a key, not an approver: it is derived from the public key's bytes,
	// never from a configured name, so two verifiers over one key report the same entry however
	// they were built. The audit verifiers report the scheme and the hex SHA-256 of the public
	// key's encoding ("ed25519:3b6a27bc..."). Entries are compared as exact strings.
	//
	// The gate counts one seat per key: it refuses a policy two of whose approvers report a
	// common entry, or whose approver's verifier reports no entry or an empty one, with
	// ErrConfig (see ApprovalPolicy.ValidateKeys). Otherwise the holder of a shared key could
	// sign as each approver that key serves and meet a quorum alone. Enrolling one approver's
	// public key under a second approver makes the gate refuse the whole policy: it fails closed,
	// and no call under it runs until the keys are corrected.
	//
	// Trust boundary: a verifier is trusted code, like the resolver that returns it. The gate
	// takes its KeyIDs on faith; it cannot check that they name the keys Verify really accepts.
	// A verifier that under-reports its keys, or reports an identity that is not derived from a
	// key, defeats the check. Use the audit verifiers, or derive identities the same way.
	KeyIDs() []string
}

// ApproverVerifierFor resolves the verifier for an approver id (the caller's PKI, mirroring
// audit's issuer lookup). It reports ok=false for an unknown approver; the counting rule
// treats that approver's decisions as unverifiable.
type ApproverVerifierFor func(approverID string) (ApproverVerifier, bool)

// ValidateKeys reports whether the policy is well formed (Validate) and holds one seat per
// signing key under verifierFor: it resolves every approver once and refuses, with ErrConfig, a
// nil verifierFor, an approver whose verifier reports no key identity (an empty KeyIDs, or an
// empty entry), and two approvers whose verifiers report a common key identity. An approver the
// resolver does not know is not refused: its decisions cannot verify, so it fills no seat.
//
// The gate runs this check on every evaluation, before it reads a recorded tally, so a resolver
// that changes between runs is checked again; TallyApprovals also never counts an approver whose
// key is shared, whatever resolver it is given. The check guards the tallies this version counts
// and records. A terminal tally already in the journal stays authoritative and is not recounted,
// so one recorded before this check existed is reused as recorded, even if it counted two
// approvers on one key. A caller that builds policies and resolvers ahead of a run can call
// ValidateKeys to refuse a bad pairing before any call pauses.
func (p ApprovalPolicy) ValidateKeys(verifierFor ApproverVerifierFor) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if verifierFor == nil {
		return fmt.Errorf("approval policy has no approver verifier resolver: %w", ErrConfig)
	}
	_, _, err := approverSeats(p, verifierFor)
	return err
}

// isNilVerifier reports whether v holds a typed nil (a nil pointer, map, slice, func, channel or
// interface inside a non-nil ApproverVerifier). Calling its methods would likely panic, and it
// names no key, so the gate refuses it rather than calling it.
func isNilVerifier(v ApproverVerifier) bool {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
		return rv.IsNil()
	}
	return false
}

// approverSeats resolves every approver in p.Approvers once, in policy order. It returns the
// verifier to check each resolved approver's decisions with (an approver the resolver does not
// know, or resolves to an untyped nil, is absent), the approvers that fill no seat with the
// reason, and the first such problem as an ErrConfig error. An approver fills no seat when the
// resolver returns a typed nil verifier, when its verifier reports no key identity, or when it
// reports a key identity another approver's verifier also reports;
// every approver sharing that key is excluded, not only the later ones, so the outcome does not
// depend on policy order or on who signed first.
func approverSeats(p ApprovalPolicy, verifierFor ApproverVerifierFor) (map[string]ApproverVerifier, map[string]string, error) {
	vs := make(map[string]ApproverVerifier, len(p.Approvers))
	bad := make(map[string]string)
	owner := make(map[string]string) // key identity -> the first approver reporting it
	var first error
	for _, id := range p.Approvers {
		if _, done := vs[id]; done || verifierFor == nil {
			continue
		}
		v, ok := verifierFor(id)
		if !ok || v == nil {
			continue
		}
		if isNilVerifier(v) {
			bad[id] = ReasonNoKeyID
			if first == nil {
				first = fmt.Errorf("approval policy approver %q: the resolver returned a nil %T verifier: %w", id, v, ErrConfig)
			}
			continue
		}
		vs[id] = v
		keys := v.KeyIDs()
		if len(keys) == 0 || slices.Contains(keys, "") {
			bad[id] = ReasonNoKeyID
			if first == nil {
				first = fmt.Errorf("approval policy approver %q: its verifier reports no key identity: %w", id, ErrConfig)
			}
			continue
		}
		for _, k := range keys {
			o, seen := owner[k]
			if !seen {
				owner[k] = id
				continue
			}
			if o == id {
				continue
			}
			bad[o], bad[id] = ReasonSharedKey, ReasonSharedKey
			if first == nil {
				first = fmt.Errorf("approval policy approvers %q and %q resolve to one signing key (%s), so one person would fill two seats: %w", o, id, k, ErrConfig)
			}
		}
	}
	return vs, bad, first
}

// ApprovalTally is the m-of-n gate's count for one call. At a pause it is the running tally
// (ApprovalPending.Quorum, ApprovalRequired.Quorum). At a terminal outcome the gate journals
// it under ApprovalTallyStep, before the tool runs, as the record of what it enforced and
// what it read.
type ApprovalTally struct {
	Need       int      `json:"need"`                  // the policy's k
	Approvers  []string `json:"approvers"`             // the eligible set the gate enforced, in policy order
	Approved   int      `json:"approved"`              // approvers whose counted decision is an approval
	Denied     int      `json:"denied"`                // approvers whose counted decision is a denial
	ApprovedBy []string `json:"approved_by,omitempty"` // those approvers, in journal order
	DeniedBy   []string `json:"denied_by,omitempty"`   // those approvers, in journal order
	Pending    []string `json:"pending,omitempty"`     // eligible approvers with no valid decision yet who can still make one, in policy order
	// Excluded names the eligible approvers who fill no seat, in policy order: their verifier
	// shares a key identity with another approver's, reports none, or is a typed nil
	// (ReasonSharedKey, ReasonNoKeyID), or the policy excludes the requester and the approver is
	// the requester (ReasonRequester) or the run records no usable requester (ReasonNoRequester).
	// They never count, so Unreachable leaves them out. The gate refuses a policy with a key
	// problem, or with no usable requester, before counting, so a tally it journals names at most
	// the requester here.
	Excluded []string `json:"excluded,omitempty"`
	// ExcludePrincipal records that the policy excluded the requester (ApprovalPolicy.ExcludePrincipal),
	// and Requesters the requesters the count excluded: the principals the run's run:principal
	// names, those of the runs above it first and the run's own last (RunPrincipal.Requesters);
	// empty when it records none.
	ExcludePrincipal bool     `json:"exclude_principal,omitempty"`
	Requesters       []string `json:"requesters,omitempty"`
	// Records names every decision record on this call the gate read, valid or not, in
	// journal order. Evidence must disclose all of them, so an omitted decision is detectable.
	Records []string `json:"records,omitempty"`
	// DecisionFormat is the signed decision format the gate counted under (DecisionFormat), set on
	// the terminal tally it journals. A tally recorded by an earlier version has none; a gate
	// refuses to run a call on such a tally that passed (ErrConfig), since its approvals do not
	// verify under this format.
	DecisionFormat string `json:"decision_format,omitempty"`
	// Supersedes names the tally this count replaces: set when the gate re-asks for approval
	// because the tally recorded under ApprovalTallyStep passed on decisions that predate
	// DecisionFormat (the re-count is recorded under ApprovalRetallyStep once terminal), or because
	// the tally it would go by passed without excluding the requester and its policy now excludes it
	// (recorded under ApprovalSoDRetallyStep).
	Supersedes string `json:"supersedes,omitempty"`
	// reapproval is the ApprovalPending.Reason of a running count that re-asks (PendingReapproval,
	// PendingSoDReapproval); it is not journaled.
	reapproval string
}

// Passed reports whether Need approvals are in.
func (t ApprovalTally) Passed() bool { return t.Approved >= t.Need }

// Unreachable reports whether Need can no longer be reached: fewer approvers remain who have
// not denied, and are not Excluded, than approvals are required. Only valid denials count toward
// this, so an invalid record cannot force a denial.
func (t ApprovalTally) Unreachable() bool {
	return len(t.Approvers)-len(t.Excluded)-t.Denied < t.Need
}

// DecisionCheck is how the counting rule classified one decision record.
type DecisionCheck struct {
	Step     string // the record's journal name
	Approver string
	Approved bool
	Counted  bool   // this is the approver's decision: their first valid one
	Reason   string // why it did not count; empty when Counted
}

// Reasons a decision record does not count.
const (
	ReasonNotEligible = "not an eligible approver"
	ReasonSuperseded  = "superseded by the approver's earlier valid decision"
	ReasonNoKey       = "no key for this approver"
	ReasonBadSig      = "signature does not verify for this call"
	ReasonSharedKey   = "the approver's signing key also serves another approver"
	ReasonNoKeyID     = "the approver's verifier reports no key identity"
	ReasonAlg         = "signed under another scheme than the approver's key"
	ReasonNoDecidedAt = "no signed decision time: signed under an earlier decision format than bide.approval.v3, which this version does not accept"
	ReasonDecidedAt   = "the signed decision time is not in canonical form (2006-01-02T15:04:05.000Z)"
	ReasonRequester   = "the approver is a requester of the run (its principal, or that of a run above it), whom the policy excludes (separation of duties)"
	ReasonNoRequester = "the policy excludes the requester, and the run, or a run above it, records no requester principal in printable ASCII"
)

// protocol:claims begin QCount

// TallyApprovals is the m-of-n counting rule, shared by the gate and offline verification so
// the two cannot drift. Over recs, in journal order, it considers every decision record on
// s.ToolUseID (IsApprovalDecision). An approver's decision is their FIRST record that is
// valid: the approver is in p.Approvers, verifierFor resolves their key, the record carries a
// decision time in canonical form (ParseTimestamp), and the signature verifies over
// ApprovalDecisionBytes(s, approver, approved, decidedAt) under the scheme the record names
// (ApproverAlg), which must be the scheme of the approver's key. Records that are not valid never
// occupy an approver's place, so a forged or mistaken decision cannot block the approver's
// real one; later valid records from an approver who already decided are superseded.
//
// A seat is a key, not an id. verifierFor is called once per approver, in policy order, and
// every record is checked with that one resolution. An approver whose verifier reports a key
// identity (ApproverVerifier.KeyIDs) that another approver's verifier also reports, or that
// reports none, never counts (ReasonSharedKey, ReasonNoKeyID), whichever of them signed first.
//
// The decision time is checked only as signed bytes: its value never affects what counts, in
// what order, or the outcome, and the time a decision was journaled (RecordedAt) is not read.
// Journal order alone orders decisions.
//
// Separation of duties: with p.ExcludePrincipal, the requesters are the principals the run's
// run:principal record among recs names (RecordedPrincipal, RunPrincipal.Requesters): the run's
// own and those of every run above it. An approver whose id equals any of them, ignoring ASCII
// case, never counts (ReasonRequester). When recs hold no run:principal, or one naming an empty or
// non-ASCII requester, no approver counts (ReasonNoRequester): the rule fails closed rather than
// count a decision it cannot tell apart from a requester's. A caller that passes only some of a
// run's records (offline verification, or a gate counting after a request) includes the
// run:principal.
//
// It returns the tally and a classification of every record it considered. It does not
// validate p (see ApprovalPolicy.ValidateKeys, which the gate runs first and which refuses a
// policy this rule would count short); a nil verifierFor counts nothing.
func TallyApprovals(recs []Record, s ApprovalSubject, p ApprovalPolicy, verifierFor ApproverVerifierFor) (ApprovalTally, []DecisionCheck) {
	t := ApprovalTally{Need: p.Need, Approvers: append([]string(nil), p.Approvers...)}
	eligible := make(map[string]bool, len(p.Approvers))
	for _, id := range p.Approvers {
		eligible[id] = true
	}
	vs, bad, _ := approverSeats(p, verifierFor)
	if p.ExcludePrincipal {
		t.ExcludePrincipal = true
		rp, ok := RecordedPrincipal(recs)
		if ok {
			t.Requesters = rp.Requesters()
		}
		usable := ok && requesterProblem(rp) == ""
		for _, id := range p.Approvers {
			if _, done := bad[id]; done {
				continue
			}
			switch {
			case !usable:
				bad[id] = ReasonNoRequester
			case slices.ContainsFunc(t.Requesters, func(r string) bool { return SameApprover(id, r) }):
				bad[id] = ReasonRequester
			}
		}
	}
	decided := make(map[string]bool, len(p.Approvers))
	var checks []DecisionCheck
	for _, r := range recs {
		if !IsApprovalDecision(r, s.ToolUseID) {
			continue
		}
		t.Records = append(t.Records, r.Name)
		c := DecisionCheck{Step: r.Name, Approver: r.Approver(), Approved: r.Approved}
		v, ok := vs[r.Approver()]
		reason, excluded := bad[r.Approver()]
		switch {
		case !eligible[r.Approver()]:
			c.Reason = ReasonNotEligible
		case decided[r.Approver()]:
			c.Reason = ReasonSuperseded
		case excluded:
			c.Reason = reason
		case !ok:
			c.Reason = ReasonNoKey
		default:
			if c.Reason = decisionReason(r, s, v); c.Reason != "" {
				break
			}
			c.Counted = true
			decided[r.Approver()] = true
			if r.Approved {
				t.Approved++
				t.ApprovedBy = append(t.ApprovedBy, r.Approver())
			} else {
				t.Denied++
				t.DeniedBy = append(t.DeniedBy, r.Approver())
			}
		}
		checks = append(checks, c)
	}
	for _, id := range p.Approvers {
		switch _, excluded := bad[id]; {
		case excluded:
			if !slices.Contains(t.Excluded, id) {
				t.Excluded = append(t.Excluded, id)
			}
		case !decided[id]:
			t.Pending = append(t.Pending, id)
		}
	}
	return t, checks
}

// decisionReason returns why r, a decision record whose approver's verifier is v, does not verify
// for s, or "" when it does: it is signed under v's scheme, carries a decision time in canonical
// form, and its signature verifies over the decision bytes rebuilt from the record, its decision
// time included as recorded.
func decisionReason(r Record, s ApprovalSubject, v ApproverVerifier) string {
	switch {
	case r.ApproverAlg() == "" || r.ApproverAlg() != v.Alg():
		return ReasonAlg
	case r.DecidedAt() == "":
		return ReasonNoDecidedAt
	case !canonicalTimestamp(r.DecidedAt()):
		return ReasonDecidedAt
	case !v.Verify(decisionBytes(s, r.Approver(), r.Approved, r.DecidedAt()), r.Signature()):
		return ReasonBadSig
	}
	return ""
}

// canonicalTimestamp reports whether s is in exactly FormatTimestamp's form.
func canonicalTimestamp(s string) bool {
	_, err := ParseTimestamp(s)
	return err == nil
}

// protocol:claims end

// protocol:claims begin QTally QCount

// SameApprover reports whether two ids name one approver: equal ignoring ASCII case, the rule
// Validate holds a policy's ids to, and the rule a gate that excludes the requester matches a
// requester to an approver id by. Approver ids are printable ASCII, so for them strings.ToLower is
// ASCII case folding; an id outside printable ASCII is equal only to itself.
func SameApprover(a, b string) bool {
	if !printableASCII(a) || !printableASCII(b) {
		return a == b
	}
	return strings.ToLower(a) == strings.ToLower(b)
}

// requesterProblem is why a run's recorded principal p names no usable requester for a policy
// that excludes the requester, or "" when every requester it names (Requesters) is a non-empty
// printable ASCII id. An empty entry is a run, the run itself or one above it, that records no
// principal: who requested is not known, so no one can be excluded for it.
func requesterProblem(p RunPrincipal) string {
	reqs := p.Requesters()
	for i, r := range reqs {
		who := "the run"
		if i < len(reqs)-1 {
			who = fmt.Sprintf("the run %d above it", len(reqs)-1-i)
		}
		switch {
		case r == "":
			return who + " records no principal (OnBehalfOf)"
		case !printableASCII(r):
			return fmt.Sprintf("%s records requester %q, which is not printable ASCII: requester ids, like approver ids, must be printable ASCII (bytes 0x21 to 0x7E, no space) to be compared with them", who, r)
		}
	}
	return ""
}

// checkRequester is the gate's check of p's requester exclusion against the run's records recs,
// run on every evaluation, before a recorded tally is read, beside the key check. A policy that
// excludes the requester is refused (ErrConfig) when:
//
//   - the run records no run:principal (RecordedPrincipal), or one that names no usable requester:
//     the run, or a run above it, records no principal, or one that is not printable ASCII;
//   - recs hold the request of the call toolUseID before the run:principal: a run an earlier
//     version started records it only when a drive under this version first loads the run, so it
//     names the resuming drive's principal, not the requester's;
//   - the run:start in recs names another principal or other ancestors than the run:principal;
//   - Need is out of reach once every requester's seat is taken away.
func checkRequester(p ApprovalPolicy, recs []Record, runID, toolUseID string) error {
	if !p.ExcludePrincipal {
		return nil
	}
	rp, ok := RecordedPrincipal(recs)
	if !ok {
		return fmt.Errorf("the approval policy excludes the requester, but run %s records no requester (no run:principal, or one that does not read strictly): start the run with an identity whose OnBehalfOf names the requester (WithIdentity, for an agent run or a plan flow run): %w", runID, ErrConfig)
	}
	at := slices.IndexFunc(recs, IsRunPrincipal)
	if callIdx, _, found := FindToolCall(recs, toolUseID); found && callIdx < at {
		return fmt.Errorf("the approval policy excludes the requester, but run %s was started before this version: it recorded its principal (run:principal) only after call %q was requested, so the record names whoever resumed the run, not who requested the call; nothing backfills it, so the run cannot pass this gate (cancel it, and start the work again as a new run): %w", runID, toolUseID, ErrConfig)
	}
	if i := slices.IndexFunc(recs, func(r Record) bool { return r.Kind == StepValue && r.Name == runStartStep }); i >= 0 {
		var st RunStart
		if err := json.Unmarshal(recs[i].Result, &st); err != nil || !rp.Matches(st) {
			return fmt.Errorf("the approval policy excludes the requester, but run %s's run:principal and run:start name different principals: %w", runID, ErrConfig)
		}
	}
	if why := requesterProblem(rp); why != "" {
		return fmt.Errorf("the approval policy excludes the requester, but in run %s %s: %w", runID, why, ErrConfig)
	}
	seats := 0
	for _, id := range p.Approvers {
		if !slices.ContainsFunc(rp.Requesters(), func(r string) bool { return SameApprover(id, r) }) {
			seats++
		}
	}
	if seats < p.Need {
		return fmt.Errorf("the approval policy needs %d of %d approvers and excludes the requesters %q, so only %d can approve: the gate could never pass: %w", p.Need, len(p.Approvers), rp.Requesters(), seats, ErrConfig)
	}
	return nil
}

// protocol:claims end

// requestersFrom is the requesters of the run ctx drives, as its drive bound them: the journaled
// principals of the run and the runs above it (see openPlan), or nil when it has none. Only the
// pause note reads it; the gate reads the journal (RecordedPrincipal).
func requestersFrom(ctx context.Context) []string {
	chain := chainFrom(ctx)
	if len(chain) == 0 {
		return nil
	}
	return RunPrincipal{Principal: chain[len(chain)-1], Ancestors: chain[:len(chain)-1]}.Requesters()
}

// IsApprovalDecision reports whether r is an m-of-n approver decision (written by SubmitDecision)
// on toolUseID. The single-approver Approve record has no Approver and is not one.
func IsApprovalDecision(r Record, toolUseID string) bool {
	return r.Kind == StepApproval && r.ToolUseID == toolUseID && r.Approver() != ""
}

// ApprovalTallyStep is the journal name of the gate's terminal tally for toolUseID.
func ApprovalTallyStep(toolUseID string) string { return "approval-tally:" + encodeID(toolUseID) }

// ApprovalRetallyStep is the journal name of the terminal tally a gate records when it re-asks for
// approval because the tally under ApprovalTallyStep passed on decisions that predate
// DecisionFormat (see ApprovalTally.Supersedes). When both are recorded, this one is the gate's
// outcome. The encoded ID has no ':', so the name never collides with another call's tally.
func ApprovalRetallyStep(toolUseID string) string { return ApprovalTallyStep(toolUseID) + ":v3" }

// ApprovalSoDRetallyStep is the journal name of the terminal tally a gate records when it re-asks
// for approval because the tally it would otherwise go by passed without excluding the requester,
// and the gate's policy now excludes it (ApprovalPolicy.ExcludePrincipal: a redeploy added the
// exclusion, or a drive still running the old policy recorded first). Its Supersedes names that
// tally. When it is recorded over that tally, it is the gate's outcome.
func ApprovalSoDRetallyStep(toolUseID string) string { return ApprovalTallyStep(toolUseID) + ":sod" }

// RecordedTally returns the record of the gate's outcome on toolUseID among records keyed by
// name. It starts from the tally (ApprovalTallyStep); over one that passed on decisions predating
// DecisionFormat, the re-approval tally (ApprovalRetallyStep) is the outcome when recorded; and
// over an outcome so far that passed without excluding the requester, the separation-of-duties
// re-approval tally (ApprovalSoDRetallyStep) is the outcome when it is recorded, excluded the
// requester, and supersedes exactly that outcome (its Supersedes names it). Those are the only
// cases a gate writes them, so a re-approval tally beside any other tally is not the gate's outcome
// and is ignored. ok is false when there is no tally.
func RecordedTally(byName map[string]Record, toolUseID string) (r Record, ok bool) {
	r, ok = byName[ApprovalTallyStep(toolUseID)]
	if !ok {
		return r, false
	}
	var t ApprovalTally
	if json.Unmarshal(r.Result, &t) != nil {
		return r, true
	}
	if predatesFormat(t) {
		if re, ok := byName[ApprovalRetallyStep(toolUseID)]; ok {
			r = re
			if json.Unmarshal(r.Result, &t) != nil {
				return r, true
			}
		}
	}
	if t.Passed() && !t.ExcludePrincipal {
		if sod, ok := byName[ApprovalSoDRetallyStep(toolUseID)]; ok {
			var st ApprovalTally
			if json.Unmarshal(sod.Result, &st) == nil && st.ExcludePrincipal && st.Supersedes == r.Name {
				return sod, true
			}
		}
	}
	return r, true
}

// SupersededTallies returns the names of the tallies that outcome, the gate's outcome (as RecordedTally
// returns it, among byName) supersedes, oldest first: the tally its Supersedes names, and that
// one's, and so on. Evidence for a re-approval tally carries them, since they decide that it is
// the outcome.
func SupersededTallies(byName map[string]Record, outcome string) []string {
	var out []string
	for cur := outcome; len(out) < 3; {
		var t ApprovalTally
		r, ok := byName[cur]
		if !ok || json.Unmarshal(r.Result, &t) != nil || t.Supersedes == "" || t.Supersedes == cur {
			break
		}
		if _, ok := byName[t.Supersedes]; !ok {
			break
		}
		out = append([]string{t.Supersedes}, out...)
		cur = t.Supersedes
	}
	return out
}

// predatesFormat reports whether t is a tally that passed on decisions signed before
// DecisionFormat: a terminal tally journaled by an earlier version, which carries no format. Its
// approvals do not verify under this format, so a gate does not run the call on it; it asks again
// (PendingReapproval). A denial is final whatever its format, since it runs nothing.
func predatesFormat(t ApprovalTally) bool { return t.Passed() && t.DecisionFormat != DecisionFormat }

// ApprovalPendingStep is the journal name of the record that a run paused at the approval gate of
// the call toolUseID (see ApprovalWait).
func ApprovalPendingStep(toolUseID string) string { return "approval-pending:" + encodeID(toolUseID) }

// ApprovalWait is what the record named ApprovalPendingStep holds: that a run paused at a call's
// approval gate, the gate it waits on, and when (RecordedAt), which is when approvers are asked.
// The run writes it, as a StepValue and only if it is absent, when it first pauses at the gate, in
// the drive whose model turn made the call; a resume that pauses again writes nothing, and a
// record lost to a crash is not written later.
//
// It is informational only, for a reader such as a run viewer, which can then show a gate before
// any decision on it is recorded. No part of the engine reads it to decide anything: not the gate
// (which counts decisions, see TallyApprovals), not a resume, not recovery. A failed write of it is
// not reported, since the pause is the same without it. Gate fields are the policy the run paused
// under; the call's result, or the gate's tally, records the policy the gate enforced. RecordedAt
// is when the record was written, by the drive's clock (WithClock, or the wall clock), in
// FormatTimestamp's form; empty on a record an earlier version wrote.
type ApprovalWait struct {
	ToolUseID  string   `json:"tool_use_id"`
	ToolName   string   `json:"tool_name"`
	Single     bool     `json:"single,omitempty"`      // a one-decision gate (SingleApproval): Need and Approvers are empty
	Need       int      `json:"need,omitempty"`        // the m-of-n gate's k
	Approvers  []string `json:"approvers,omitempty"`   // the m-of-n gate's eligible approvers, in policy order
	RecordedAt string   `json:"recorded_at,omitempty"` // when the run paused here, by the drive's clock
	// ExcludePrincipal: the m-of-n gate excludes the requester (ApprovalPolicy.ExcludePrincipal), and
	// Requesters are the requesters it excludes (RunPrincipal.Requesters).
	ExcludePrincipal bool     `json:"exclude_principal,omitempty"`
	Requesters       []string `json:"requesters,omitempty"`
}

// FindToolCall returns the journal index of the record that requested toolUseID, and the call
// itself (its name and arguments), or ok=false if no record requested it. The request of an
// agent's tool call is the model turn that made it; the request of a gated plan flow node (whose
// toolUseID is the node's key) is its ApprovalRequest, and the call's name and arguments are the
// node's name and input. The first request in journal order is returned.
func FindToolCall(recs []Record, toolUseID string) (idx int, call ToolUse, ok bool) {
	for i, r := range recs {
		if q, ok := decodeApprovalRequest(r, toolUseID); ok {
			return i, ToolUse{ID: q.ToolUseID, Name: q.ToolName, Args: q.Args}, true
		}
		if r.Kind != StepModel || r.Message == nil {
			continue
		}
		for _, tu := range r.Message.toolUses() {
			if tu.ID == toolUseID {
				return i, tu, true
			}
		}
	}
	return -1, ToolUse{}, false
}

// approvalDecisionStep is the journal name of one decision. It is distinct per decision (the
// approved flag, the scheme, the signed decision time and the signature are hashed into it), so an
// approver's records never collide: a bad record cannot take the name a later valid one needs (a
// copy of a valid signature under another decision time, say), and an identical resubmission maps
// to the same name and stays a no-op. Deterministic signatures (ed25519, and audit's ML-DSA
// signer) resubmit to the same name; randomized ones add a record the counting rule supersedes.
func approvalDecisionStep(toolUseID, approverID string, approved bool, alg Alg, decidedAt string, sig []byte) string {
	h := sha256.New()
	if approved {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	h.Write(binary.BigEndian.AppendUint32(nil, uint32(len(alg))))
	h.Write([]byte(alg))
	h.Write(binary.BigEndian.AppendUint32(nil, uint32(len(decidedAt))))
	h.Write([]byte(decidedAt))
	h.Write(sig)
	return approvalStep(toolUseID) + ":" + approverID + ":" + hex.EncodeToString(h.Sum(nil))
}

// ApproveOption configures SubmitDecision: WithDecisionCheck, or WithClock for the clock that
// stamps the decision's RecordedAt.
type ApproveOption interface {
	applyApprove(*approveOptions) error
}

type approveOptions struct {
	verifierFor ApproverVerifierFor
	now         func() time.Time
}

// decisionCheck implements ApproveOption.
type decisionCheck ApproverVerifierFor

func (f decisionCheck) applyApprove(o *approveOptions) error {
	o.verifierFor = ApproverVerifierFor(f)
	return nil
}

func (f clockOption) applyApprove(o *approveOptions) error {
	if err := f.check(); err != nil {
		return err
	}
	o.now = f
	return nil
}

// WithDecisionCheck makes SubmitDecision check the decision before recording it, so an approver
// learns at submission time that it will not count: it reads the run, finds the call, and
// verifies the signature against that exact call with verifierFor. It returns
// ErrInvalidApproval (no such call, unknown approver, or a signature that does not verify)
// or ErrAlreadyDecided (the approver's earlier valid decision already counts), and records
// nothing. The check is for feedback; the gate re-verifies every record itself, so
// correctness never depends on it. Eligibility is the policy's, which SubmitDecision does not see.
func WithDecisionCheck(verifierFor ApproverVerifierFor) ApproveOption {
	return decisionCheck(verifierFor)
}

// Decision is one named approver's signed decision on a tool call gated by an m-of-n Approval
// policy. Signature is the approver's signature over
// ApprovalDecisionBytes(pend.Subject(), ApproverID, Approved, DecidedAt), where pend is the run's
// *ApprovalPending, under the scheme Alg names (the signer's Alg). Alg is journaled with the
// decision as Record.ApproverAlg, and the gate counts the decision only under a verifier of that
// scheme.
//
// DecidedAt is when the approver decided, by the approver's clock, and is required. It is
// journaled in FormatTimestamp's form (to the millisecond, in UTC) as the record's DecidedAt, and
// the signature covers it, so it cannot be changed after signing. It is a claim by the approver,
// for people and auditors: the gate never reads its value (see TallyApprovals). An approver who
// sends a decision over the wire sends the time as text; ParseTimestamp reads it back.
type Decision struct {
	RunID      string // the run whose journal holds the call: the pause's RunID
	ToolUseID  string
	ApproverID string // one of the policy's Approvers, exactly (printable ASCII; see ApprovalPolicy.Validate)
	Approved   bool
	DecidedAt  time.Time // when the approver decided, by their clock; signed
	Alg        Alg       // the signature scheme, e.g. audit.AlgEd25519
	Signature  []byte
}

// protocol:claims begin Submit
// protocol:flows begin ADecide

// SubmitDecision records d, one approver's signed decision on an m-of-n gated call. After
// enough decisions land, re-run the pause's RootRunID. (A single-approver gate uses Approve.)
//
// The record carries d.DecidedAt, signed, and the time it was recorded (Record.RecordedAt), read
// from WithClock's clock, else the clock ctx carries (a run's), else the wall clock. Neither time
// affects the gate. A zero DecidedAt, or one with no canonical form, is ErrConfig, and so is an
// ApproverID that is not printable ASCII (see ApprovalPolicy.Validate), which no policy can name.
//
// Each distinct decision is its own journal record; resubmitting the identical decision is a
// no-op: the same signature maps to the same record name, and with WithDecisionCheck the same
// approver, verdict, scheme and decided_at with a different signature (a randomized scheme such as
// ecdsa-p256 re-signs differently) is recognised as the same decision and written once. Without the
// check, a retry with a new signature adds a second record, which the gate ignores. The gate counts each approver's first valid decision and ignores the rest, so an
// invalid record never blocks a valid one. Without WithDecisionCheck, SubmitDecision does not
// verify: a decision that will not count is recorded and reported by the gate as ignored. For a
// plan flow node's gate (ToolUseID a node key, in a flow's run) the run must have recorded the
// gate's request: SubmitDecision refuses one with none (ErrConfig), and the gate counts only
// decisions after it. An identical decision recorded before the request (same name) is reported
// (ErrConfig): it cannot count; the approver re-signs at a new decided_at, which is a new decision
// that counts.
func SubmitDecision(ctx context.Context, store *Journal, d Decision, opts ...ApproveOption) error {
	return submitDecision(ctx, store, "SubmitDecision", d, opts)
}

// protocol:flows end
// protocol:claims end

func submitDecision(ctx context.Context, store *Journal, op string, d Decision, opts []ApproveOption) error {
	if d.RunID == "" {
		return fmt.Errorf("%s: empty runID: %w", op, ErrConfig)
	}
	if d.ToolUseID == "" {
		return fmt.Errorf("%s: empty toolUseID: %w", op, ErrConfig)
	}
	if d.ApproverID == "" {
		return fmt.Errorf("%s: empty approverID: %w", op, ErrConfig)
	}
	if !printableASCII(d.ApproverID) {
		// No policy can name such an id (ApprovalPolicy.Validate), so the decision could never
		// count; refusing it also keeps a line break or an escape out of every report that names
		// the decisions a gate read.
		return fmt.Errorf("%s: approverID %q is not printable ASCII (bytes 0x21 to 0x7E, no space), as every policy's approver ids are: %w", op, d.ApproverID, ErrConfig)
	}
	if d.Alg == "" {
		return fmt.Errorf("%s: empty signature scheme (Decision.Alg): %w", op, ErrConfig)
	}
	if len(d.Signature) == 0 {
		return fmt.Errorf("%s: empty signature: %w", op, ErrConfig)
	}
	if d.DecidedAt.IsZero() {
		return fmt.Errorf("%s: no decision time (Decision.DecidedAt): every signed decision carries the time the approver decided: %w", op, ErrConfig)
	}
	decidedAt := FormatTimestamp(d.DecidedAt)
	if !canonicalTimestamp(decidedAt) {
		return fmt.Errorf("%s: decision time %s has no canonical form (years 0001 to 9999): %w", op, d.DecidedAt, ErrConfig)
	}
	o := approveOptions{now: clockFrom(ctx)}
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		if err := opt.applyApprove(&o); err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
	}
	reqSeq, flow, err := flowDecision(ctx, store, op, d.RunID, d.ToolUseID)
	if err != nil {
		return err
	}
	name := approvalDecisionStep(d.ToolUseID, d.ApproverID, d.Approved, d.Alg, decidedAt, d.Signature)
	if o.verifierFor != nil {
		same, err := checkDecision(ctx, store, op, d, name, decidedAt, flow, o.verifierFor)
		if err != nil {
			return err
		}
		if same != "" {
			// The same decision is already journaled (a retry after a lost response). Writing
			// again would add a record whenever the signature differs, as a randomized scheme's does.
			name = same
			if flow {
				return staleDecision(ctx, store, op, d.RunID, d.ToolUseID, name, reqSeq, false)
			}
			return nil
		}
	}
	if _, err := store.do(ctx, d.RunID, name, func(context.Context) (Record, error) {
		// RecordedAt is read here, when the record is first written: an identical resubmission
		// keeps the first record, and its time. It is held to the one form decided_at is, so a
		// clock outside the years 0001 to 9999 records nothing rather than a time no reader accepts.
		recordedAt := FormatTimestamp(o.now())
		if !canonicalTimestamp(recordedAt) {
			return Record{}, fmt.Errorf("%s: the clock reads %s, which has no canonical form (years 0001 to 9999), so the decision's recorded_at cannot be written: %w", op, o.now(), ErrConfig)
		}
		return Record{Kind: StepApproval, ToolUseID: d.ToolUseID, Approved: d.Approved, ApproverSignature: &ApproverSignature{
			Approver: d.ApproverID, ApproverAlg: d.Alg, Signature: d.Signature, DecidedAt: decidedAt, RecordedAt: recordedAt,
		}}, nil
	}); err != nil {
		return err
	}
	if flow {
		return staleDecision(ctx, store, op, d.RunID, d.ToolUseID, name, reqSeq, false)
	}
	return nil
}

// checkDecision is WithDecisionCheck's pre-flight: the decision verifies for the recorded
// call, and the approver has no other valid decision on it. A valid record with the same approver,
// verdict, scheme and decided_at is the same decision, whatever its signature bytes (a randomized
// scheme signs it differently each time): its name is returned, and nothing is written again. For a plan
// flow node's gate (flow) it reads, as the gate does, only the decisions after the gate's request:
// one recorded before it never counts, so it does not make this one "already decided".
func checkDecision(ctx context.Context, store *Journal, op string, d Decision, name, decidedAt string, flow bool, verifierFor ApproverVerifierFor) (string, error) {
	recs, err := store.History(ctx, d.RunID)
	if err != nil {
		return "", fmt.Errorf("%s: load history %s: %w (%w)", op, d.RunID, err, ErrStorage)
	}
	_, call, ok := FindToolCall(recs, d.ToolUseID)
	if !ok {
		return "", fmt.Errorf("%s: no tool call %q in run %s: %w", op, d.ToolUseID, d.RunID, ErrInvalidApproval)
	}
	v, ok := verifierFor(d.ApproverID)
	if !ok || v == nil {
		return "", fmt.Errorf("%s: no key for approver %q: %w", op, d.ApproverID, ErrInvalidApproval)
	}
	if isNilVerifier(v) {
		return "", fmt.Errorf("%s: the resolver returned a nil %T verifier for approver %q: %w", op, v, d.ApproverID, ErrConfig)
	}
	if d.Alg != v.Alg() {
		return "", fmt.Errorf("%s: approver %q: decision signed under %q, key is %q: %w", op, d.ApproverID, d.Alg, v.Alg(), ErrInvalidApproval)
	}
	s := ApprovalSubject{RunID: d.RunID, ToolUseID: d.ToolUseID, ToolName: call.Name, Args: call.Args}
	if !v.Verify(ApprovalDecisionBytes(s, d.ApproverID, d.Approved, d.DecidedAt), d.Signature) {
		return "", fmt.Errorf("%s: approver %q: %s: %w", op, d.ApproverID, ReasonBadSig, ErrInvalidApproval)
	}
	if flow {
		i := slices.IndexFunc(recs, func(r Record) bool { return r.Name == ApprovalRequestStep(d.ToolUseID) })
		recs = recs[i+1:] // flowDecision found the request, so i >= 0
	}
	for _, r := range recs {
		if !IsApprovalDecision(r, d.ToolUseID) || r.Approver() != d.ApproverID || r.Name == name {
			continue
		}
		if decisionReason(r, s, v) == "" {
			if r.Approved == d.Approved && r.ApproverAlg() == d.Alg && r.DecidedAt() == decidedAt {
				return r.Name, nil
			}
			return "", fmt.Errorf("%s: approver %q already decided on call %q: %w", op, d.ApproverID, d.ToolUseID, ErrAlreadyDecided)
		}
	}
	return "", nil
}

// notePending records, unless it is recorded already, that the run paused at tu's gate pol (see
// ApprovalWait). The loop calls it only for a call of a turn this drive asked the model for, so a
// resume, which replays the turn, writes nothing: re-driving a paused run leaves its journal as it
// is. Nothing reads the record to decide, so a failed write is dropped.
func (a *Agent) notePending(ctx context.Context, runID string, tu ToolUse, pol *ApprovalPolicy) {
	w := ApprovalWait{ToolUseID: tu.ID, ToolName: tu.Name, Single: pol.single(), RecordedAt: noteStamp(clockFrom(ctx))}
	if !w.Single {
		w.Need, w.Approvers = pol.Need, slices.Clone(pol.Approvers)
		w.ExcludePrincipal = pol.ExcludePrincipal
		if pol.ExcludePrincipal {
			w.Requesters = requestersFrom(ctx)
		}
	}
	b, err := json.Marshal(w)
	if err != nil {
		return
	}
	_, _ = a.store.put(ctx, runID, ApprovalPendingStep(tu.ID), Record{Kind: StepValue, Result: b})
}
