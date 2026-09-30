package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

// ApproverVerifier checks an approver's signature over the decision bytes. Any audit.Verifier
// satisfies it structurally, so a deployment reuses the existing ed25519/ML-DSA/hybrid
// verifiers with no new cryptography and no agent->audit import.
type ApproverVerifier interface {
	Verify(message, sig []byte) bool
}

// ApproverVerifierFor resolves the verifier for an approver id (the caller's PKI, mirroring
// audit's issuer lookup). It reports ok=false for an unknown approver; the counting rule
// treats that approver's decisions as unverifiable.
type ApproverVerifierFor func(approverID string) (ApproverVerifier, bool)

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
	Pending    []string `json:"pending,omitempty"`     // eligible approvers with no valid decision yet, in policy order
	// Records names every decision record on this call the gate read, valid or not, in
	// journal order. Evidence must disclose all of them, so an omitted decision is detectable.
	Records []string `json:"records,omitempty"`
}

// Passed reports whether Need approvals are in.
func (t ApprovalTally) Passed() bool { return t.Approved >= t.Need }

// Unreachable reports whether Need can no longer be reached: fewer approvers remain who have
// not denied than approvals are required. Only valid denials count toward this, so an invalid
// record cannot force a denial.
func (t ApprovalTally) Unreachable() bool { return len(t.Approvers)-t.Denied < t.Need }

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
)

// TallyApprovals is the m-of-n counting rule, shared by the gate and offline verification so
// the two cannot drift. Over recs, in journal order, it considers every decision record on
// s.ToolUseID (IsApprovalDecision). An approver's decision is their FIRST record that is
// valid: the approver is in p.Approvers, verifierFor resolves their key, and the signature
// verifies over ApprovalDecisionBytes(s, approver, approved). Records that are not valid never
// occupy an approver's place, so a forged or mistaken decision cannot block the approver's
// real one; later valid records from an approver who already decided are superseded.
//
// It returns the tally and a classification of every record it considered. It does not
// validate p (see ApprovalPolicy.Validate); a nil verifierFor counts nothing.
func TallyApprovals(recs []Record, s ApprovalSubject, p ApprovalPolicy, verifierFor ApproverVerifierFor) (ApprovalTally, []DecisionCheck) {
	t := ApprovalTally{Need: p.Need, Approvers: append([]string(nil), p.Approvers...)}
	eligible := make(map[string]bool, len(p.Approvers))
	for _, id := range p.Approvers {
		eligible[id] = true
	}
	decided := make(map[string]bool, len(p.Approvers))
	var checks []DecisionCheck
	for _, r := range recs {
		if !IsApprovalDecision(r, s.ToolUseID) {
			continue
		}
		t.Records = append(t.Records, r.Name)
		c := DecisionCheck{Step: r.Name, Approver: r.Approver, Approved: r.Approved}
		switch {
		case !eligible[r.Approver]:
			c.Reason = ReasonNotEligible
		case decided[r.Approver]:
			c.Reason = ReasonSuperseded
		default:
			var v ApproverVerifier
			ok := false
			if verifierFor != nil {
				v, ok = verifierFor(r.Approver)
			}
			switch {
			case !ok || v == nil:
				c.Reason = ReasonNoKey
			case !v.Verify(ApprovalDecisionBytes(s, r.Approver, r.Approved), r.Signature):
				c.Reason = ReasonBadSig
			default:
				c.Counted = true
				decided[r.Approver] = true
				if r.Approved {
					t.Approved++
					t.ApprovedBy = append(t.ApprovedBy, r.Approver)
				} else {
					t.Denied++
					t.DeniedBy = append(t.DeniedBy, r.Approver)
				}
			}
		}
		checks = append(checks, c)
	}
	for _, id := range p.Approvers {
		if !decided[id] {
			t.Pending = append(t.Pending, id)
		}
	}
	return t, checks
}

// IsApprovalDecision reports whether r is an m-of-n approver decision (written by SubmitDecision)
// on toolUseID. The single-approver Approve record has no Approver and is not one.
func IsApprovalDecision(r Record, toolUseID string) bool {
	return r.Kind == StepApproval && r.ToolUseID == toolUseID && r.Approver != ""
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
// approved flag and signature are hashed into it), so an approver's records never collide: a
// bad record cannot take the name a later valid one needs, and an identical resubmission maps
// to the same name and stays a no-op. Deterministic signatures (ed25519) resubmit to the same
// name; randomized ones (ML-DSA) add a record the counting rule supersedes.
func approvalDecisionStep(toolUseID, approverID string, approved bool, sig []byte) string {
	h := sha256.New()
	if approved {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
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
// *ApprovalPending.
type Decision struct {
	RunID      string // the run whose journal holds the call: the pause's RunID
	ToolUseID  string
	ApproverID string
	Approved   bool
	Signature  []byte
}

// SubmitDecision records d, one approver's signed decision on an m-of-n gated call. After
// enough decisions land, re-run the pause's RootRunID. (A single-approver gate uses Approve.)
//
// Each distinct decision is its own journal record; resubmitting the identical decision is a
// no-op. The gate counts each approver's first valid decision and ignores the rest, so an
// invalid record never blocks a valid one. Without WithDecisionCheck, SubmitDecision does not
// verify: a decision that will not count is recorded and reported by the gate as ignored.
func SubmitDecision(ctx context.Context, store Durable, d Decision, opts ...ApproveOption) error {
	return submitDecision(ctx, store, "SubmitDecision", d, opts)
}

// ApproveAs records one named approver's signed decision on an m-of-n gated call.
//
// Deprecated: transitional; replaced by SubmitDecision, which takes the decision as a Decision.
func ApproveAs(ctx context.Context, d Durable, runID, toolUseID, approverID string, approved bool, sig []byte, opts ...ApproveOption) error {
	return submitDecision(ctx, d, "ApproveAs", Decision{RunID: runID, ToolUseID: toolUseID, ApproverID: approverID, Approved: approved, Signature: sig}, opts)
}

func submitDecision(ctx context.Context, store Durable, op string, d Decision, opts []ApproveOption) error {
	if d.RunID == "" {
		return fmt.Errorf("%s: empty runID: %w", op, ErrConfig)
	}
	if d.ToolUseID == "" {
		return fmt.Errorf("%s: empty toolUseID: %w", op, ErrConfig)
	}
	if d.ApproverID == "" {
		return fmt.Errorf("%s: empty approverID: %w", op, ErrConfig)
	}
	if len(d.Signature) == 0 {
		return fmt.Errorf("%s: empty signature: %w", op, ErrConfig)
	}
	var o approveOptions
	for _, opt := range opts {
		opt(&o)
	}
	name := approvalDecisionStep(d.ToolUseID, d.ApproverID, d.Approved, d.Signature)
	if o.verifierFor != nil {
		if err := checkDecision(ctx, store, op, d, name, o.verifierFor); err != nil {
			return err
		}
	}
	_, err := store.Do(ctx, d.RunID, name, func(context.Context) (Record, error) {
		return Record{Kind: StepApproval, ToolUseID: d.ToolUseID, Approved: d.Approved, Approver: d.ApproverID, Signature: d.Signature}, nil
	})
	return err
}

// checkDecision is WithDecisionCheck's pre-flight: the decision verifies for the recorded
// call, and the approver has no other valid decision on it (an identical one is fine).
func checkDecision(ctx context.Context, store Durable, op string, d Decision, name string, verifierFor ApproverVerifierFor) error {
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
	s := ApprovalSubject{RunID: d.RunID, ToolUseID: d.ToolUseID, ToolName: call.Name, Args: call.Args}
	if !v.Verify(ApprovalDecisionBytes(s, d.ApproverID, d.Approved), d.Signature) {
		return fmt.Errorf("%s: approver %q: %s: %w", op, d.ApproverID, ReasonBadSig, ErrInvalidApproval)
	}
	for _, r := range recs {
		if !IsApprovalDecision(r, d.ToolUseID) || r.Approver != d.ApproverID || r.Name == name {
			continue
		}
		if v.Verify(ApprovalDecisionBytes(s, d.ApproverID, r.Approved), r.Signature) {
			return fmt.Errorf("%s: approver %q already decided on call %q: %w", op, d.ApproverID, d.ToolUseID, ErrAlreadyDecided)
		}
	}
	return nil
}
