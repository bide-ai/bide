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
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// This file holds the m-of-n approval gate's data model: the policy check, what an approver
// signs, how decisions are journaled, and the counting rule. The counting rule
// (TallyApprovals) is exported and pure so the gate at run time and an auditor offline run
// the same function; see audit.VerifyApprovals.

// Validate reports whether the policy is well formed: at least one approver, every approver id
// non-empty valid UTF-8, no two ids the same approver, and 1 <= Need <= len(Approvers). A policy
// that fails is a configuration error; the gate refuses it with ErrConfig rather than guessing.
//
// Approver ids are compared as exact bytes everywhere else (eligibility, the signed decision
// bytes, the verifier lookup). A policy may still not list two ids that differ only by case or by
// Unicode normalization ("alice" and "Alice", an NFC and an NFD "café", a fullwidth and an ASCII
// spelling): a person reading the policy, or a key lookup that folds case, takes them for one
// approver, who could then fill two seats. Two ids are the same approver when their
// approverFoldKey is equal, and such a policy is refused as ambiguous.
func (p ApprovalPolicy) Validate() error {
	if p.one { // SingleApproval, unless its fields were changed since
		if p.Need != 1 || len(p.Approvers) != 0 {
			return fmt.Errorf("a SingleApproval policy was changed to Need %d and %d approvers; build an m-of-n policy instead: %w", p.Need, len(p.Approvers), ErrConfig)
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
		if !utf8.ValidString(id) {
			return fmt.Errorf("approval policy approver id %q is not valid UTF-8: %w", id, ErrConfig)
		}
		key := approverFoldKey(id)
		if first, dup := seen[key]; dup {
			if first == id {
				return fmt.Errorf("approval policy lists approver %q twice: %w", id, ErrConfig)
			}
			return fmt.Errorf("approval policy lists approvers %q and %q, which differ only by case or Unicode normalization: %w", first, id, ErrConfig)
		}
		seen[key] = id
	}
	if p.Need < 1 || p.Need > len(p.Approvers) {
		return fmt.Errorf("approval policy Need = %d, want 1 <= Need <= %d approvers: %w", p.Need, len(p.Approvers), ErrConfig)
	}
	return nil
}

// approverFoldKey is id under NFKC case folding (Unicode's toNFKC_Casefold, without its removal
// of default-ignorable code points): decomposed, case folded with full folding, then NFKC
// normalized. Two ids with the same key differ only by case, by canonical or compatibility
// normalization, or both.
func approverFoldKey(id string) string {
	return norm.NFKC.String(cases.Fold().String(norm.NFD.String(id)))
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
//	sig := signer.Sign(agent.ApprovalDecisionBytes(pend.Subject(), "finance", true))
//
// For a gate inside a sub-agent, RunID is the sub-run's id.
func (e *ApprovalPending) Subject() ApprovalSubject {
	return ApprovalSubject{RunID: e.RunID, ToolUseID: e.ToolUseID, ToolName: e.ToolName, Args: e.Args}
}

// approvalDomain tags the bytes an approver signs so a signature over a decision can never
// be replayed as a signature over any other message the same key signs. v2 binds the call's
// tool name and arguments; v1 bound only the call id.
const approvalDomain = "bide.approval.v2\n"

// ApprovalDecisionBytes returns the canonical bytes an approver signs and every verifier
// checks. The layout, stable and reproducible outside Go:
//
//	"bide.approval.v2\n"
//	len32(RunID) RunID
//	len32(ToolUseID) ToolUseID
//	len32(ToolName) ToolName
//	SHA-256(canonical args), 32 bytes
//	len32(approverID) approverID
//	1 byte: 1 approved, 0 denied
//
// len32 is a 4-byte big-endian length, so no two distinct decisions encode to the same bytes.
// The canonical args are the arguments parsed as JSON and re-serialized with object keys
// sorted, no insignificant whitespace, no HTML escaping, and number literals kept verbatim;
// empty arguments are "{}", and bytes that are not a single JSON value are used as-is. The
// canonical form makes the signature independent of how the arguments were formatted in
// transit (a live model stream, the journal, an evidence file).
func ApprovalDecisionBytes(s ApprovalSubject, approverID string, approved bool) []byte {
	args := sha256.Sum256(canonicalArgs(s.Args))
	b := make([]byte, 0, len(approvalDomain)+4*4+len(s.RunID)+len(s.ToolUseID)+len(s.ToolName)+len(args)+len(approverID)+1)
	b = append(b, approvalDomain...)
	for _, str := range []string{s.RunID, s.ToolUseID, s.ToolName} {
		b = binary.BigEndian.AppendUint32(b, uint32(len(str)))
		b = append(b, str...)
	}
	b = append(b, args[:]...)
	b = binary.BigEndian.AppendUint32(b, uint32(len(approverID)))
	b = append(b, approverID...)
	if approved {
		return append(b, 1)
	}
	return append(b, 0)
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

// Alg names a signature scheme: "ed25519", "ml-dsa-65", or "ed25519+ml-dsa-65". The audit package
// defines the schemes (audit.AlgEd25519, audit.AlgMLDSA65, audit.AlgHybrid) and their signers and
// verifiers; the agent package only journals the name, with each approver decision, so a decision is
// checked under the scheme it was signed with.
type Alg string

// ApproverVerifier checks an approver's signature over the decision bytes under one scheme and
// names the signing keys behind it. audit.Ed25519Verifier, audit.MLDSAVerifier and
// audit.HybridVerifier implement it, so a deployment reuses the existing verifiers with no new
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
	// (ReasonSharedKey, ReasonNoKeyID). They never count, so Unreachable leaves them out. The gate
	// refuses such a policy before counting, so a tally it journals has none.
	Excluded []string `json:"excluded,omitempty"`
	// Records names every decision record on this call the gate read, valid or not, in
	// journal order. Evidence must disclose all of them, so an omitted decision is detectable.
	Records []string `json:"records,omitempty"`
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
)

// protocol:claims begin QCount

// TallyApprovals is the m-of-n counting rule, shared by the gate and offline verification so
// the two cannot drift. Over recs, in journal order, it considers every decision record on
// s.ToolUseID (IsApprovalDecision). An approver's decision is their FIRST record that is
// valid: the approver is in p.Approvers, verifierFor resolves their key, and the signature
// verifies over ApprovalDecisionBytes(s, approver, approved) under the scheme the record names
// (ApproverAlg), which must be the scheme of the approver's key. Records that are not valid never
// occupy an approver's place, so a forged or mistaken decision cannot block the approver's
// real one; later valid records from an approver who already decided are superseded.
//
// A seat is a key, not an id. verifierFor is called once per approver, in policy order, and
// every record is checked with that one resolution. An approver whose verifier reports a key
// identity (ApproverVerifier.KeyIDs) that another approver's verifier also reports, or that
// reports none, never counts (ReasonSharedKey, ReasonNoKeyID), whichever of them signed first.
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
		case r.ApproverAlg() == "" || r.ApproverAlg() != v.Alg():
			c.Reason = ReasonAlg
		case !v.Verify(ApprovalDecisionBytes(s, r.Approver(), r.Approved), r.Signature()):
			c.Reason = ReasonBadSig
		default:
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

// protocol:claims end

// IsApprovalDecision reports whether r is an m-of-n approver decision (written by SubmitDecision)
// on toolUseID. The single-approver Approve record has no Approver and is not one.
func IsApprovalDecision(r Record, toolUseID string) bool {
	return r.Kind == StepApproval && r.ToolUseID == toolUseID && r.Approver() != ""
}

// ApprovalTallyStep is the journal name of the gate's terminal tally for toolUseID.
func ApprovalTallyStep(toolUseID string) string { return "approval-tally:" + encodeID(toolUseID) }

// FindToolCall returns the journal index of the model turn that requested toolUseID, and the
// call itself (its name and arguments), or ok=false if no recorded turn requested it.
func FindToolCall(recs []Record, toolUseID string) (idx int, call ToolUse, ok bool) {
	for i, r := range recs {
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
// approved flag, the scheme and the signature are hashed into it), so an approver's records never
// collide: a bad record cannot take the name a later valid one needs, and an identical
// resubmission maps to the same name and stays a no-op. Deterministic signatures (ed25519, and
// audit's ML-DSA signer) resubmit to the same name; randomized ones add a record the counting rule
// supersedes.
func approvalDecisionStep(toolUseID, approverID string, approved bool, alg Alg, sig []byte) string {
	h := sha256.New()
	if approved {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	h.Write(binary.BigEndian.AppendUint32(nil, uint32(len(alg))))
	h.Write([]byte(alg))
	h.Write(sig)
	return approvalStep(toolUseID) + ":" + approverID + ":" + hex.EncodeToString(h.Sum(nil))
}

// ApproveOption configures SubmitDecision.
type ApproveOption func(*approveOptions)

type approveOptions struct {
	verifierFor ApproverVerifierFor
}

// WithDecisionCheck makes SubmitDecision check the decision before recording it, so an approver
// learns at submission time that it will not count: it reads the run, finds the call, and
// verifies the signature against that exact call with verifierFor. It returns
// ErrInvalidApproval (no such call, unknown approver, or a signature that does not verify)
// or ErrAlreadyDecided (the approver's earlier valid decision already counts), and records
// nothing. The check is for feedback; the gate re-verifies every record itself, so
// correctness never depends on it. Eligibility is the policy's, which SubmitDecision does not see.
func WithDecisionCheck(verifierFor ApproverVerifierFor) ApproveOption {
	return func(o *approveOptions) { o.verifierFor = verifierFor }
}

// Decision is one named approver's signed decision on a tool call gated by an m-of-n Approval
// policy. Signature is the approver's signature over
// ApprovalDecisionBytes(pend.Subject(), ApproverID, Approved), where pend is the run's
// *ApprovalPending, under the scheme Alg names (the signer's Alg). Alg is journaled with the
// decision as Record.ApproverAlg, and the gate counts the decision only under a verifier of that
// scheme.
type Decision struct {
	RunID      string // the run whose journal holds the call: the pause's RunID
	ToolUseID  string
	ApproverID string
	Approved   bool
	Alg        Alg // the signature scheme, e.g. audit.AlgEd25519
	Signature  []byte
}

// protocol:claims begin Submit

// SubmitDecision records d, one approver's signed decision on an m-of-n gated call. After
// enough decisions land, re-run the pause's RootRunID. (A single-approver gate uses Approve.)
//
// Each distinct decision is its own journal record; resubmitting the identical decision is a
// no-op. The gate counts each approver's first valid decision and ignores the rest, so an
// invalid record never blocks a valid one. Without WithDecisionCheck, SubmitDecision does not
// verify: a decision that will not count is recorded and reported by the gate as ignored.
func SubmitDecision(ctx context.Context, store *Journal, d Decision, opts ...ApproveOption) error {
	return submitDecision(ctx, store, "SubmitDecision", d, opts)
}

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
	if d.Alg == "" {
		return fmt.Errorf("%s: empty signature scheme (Decision.Alg): %w", op, ErrConfig)
	}
	if len(d.Signature) == 0 {
		return fmt.Errorf("%s: empty signature: %w", op, ErrConfig)
	}
	var o approveOptions
	for _, opt := range opts {
		opt(&o)
	}
	name := approvalDecisionStep(d.ToolUseID, d.ApproverID, d.Approved, d.Alg, d.Signature)
	if o.verifierFor != nil {
		if err := checkDecision(ctx, store, op, d, name, o.verifierFor); err != nil {
			return err
		}
	}
	_, err := store.do(ctx, d.RunID, name, func(context.Context) (Record, error) {
		return Record{Kind: StepApproval, ToolUseID: d.ToolUseID, Approved: d.Approved, ApproverSignature: &ApproverSignature{Approver: d.ApproverID, ApproverAlg: d.Alg, Signature: d.Signature}}, nil
	})
	return err
}

// checkDecision is WithDecisionCheck's pre-flight: the decision verifies for the recorded
// call, and the approver has no other valid decision on it (an identical one is fine).
func checkDecision(ctx context.Context, store *Journal, op string, d Decision, name string, verifierFor ApproverVerifierFor) error {
	recs, err := store.History(ctx, d.RunID)
	if err != nil {
		return fmt.Errorf("%s: load history %s: %w (%w)", op, d.RunID, err, ErrStorage)
	}
	_, call, ok := FindToolCall(recs, d.ToolUseID)
	if !ok {
		return fmt.Errorf("%s: no tool call %q in run %s: %w", op, d.ToolUseID, d.RunID, ErrInvalidApproval)
	}
	v, ok := verifierFor(d.ApproverID)
	if !ok || v == nil {
		return fmt.Errorf("%s: no key for approver %q: %w", op, d.ApproverID, ErrInvalidApproval)
	}
	if isNilVerifier(v) {
		return fmt.Errorf("%s: the resolver returned a nil %T verifier for approver %q: %w", op, v, d.ApproverID, ErrConfig)
	}
	if d.Alg != v.Alg() {
		return fmt.Errorf("%s: approver %q: decision signed under %q, key is %q: %w", op, d.ApproverID, d.Alg, v.Alg(), ErrInvalidApproval)
	}
	s := ApprovalSubject{RunID: d.RunID, ToolUseID: d.ToolUseID, ToolName: call.Name, Args: call.Args}
	if !v.Verify(ApprovalDecisionBytes(s, d.ApproverID, d.Approved), d.Signature) {
		return fmt.Errorf("%s: approver %q: %s: %w", op, d.ApproverID, ReasonBadSig, ErrInvalidApproval)
	}
	for _, r := range recs {
		if !IsApprovalDecision(r, d.ToolUseID) || r.Approver() != d.ApproverID || r.Name == name {
			continue
		}
		if r.ApproverAlg() == v.Alg() && v.Verify(ApprovalDecisionBytes(s, d.ApproverID, r.Approved), r.Signature()) {
			return fmt.Errorf("%s: approver %q already decided on call %q: %w", op, d.ApproverID, d.ToolUseID, ErrAlreadyDecided)
		}
	}
	return nil
}
