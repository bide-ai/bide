package audit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"

	"github.com/bide-ai/bide/agent"
)

// findApproval returns the journal index of approverID's decision record for toolUseID, or -1.
func findApproval(recs []agent.Record, toolUseID, approverID string) int {
	name := agent.ApprovalStepName(toolUseID, approverID)
	for i, r := range recs {
		if r.Kind == agent.StepApproval && r.Name == name {
			return i
		}
	}
	return -1
}

// ProveApproval builds a ProofBundle proving the approver-decision record keyed
// "approval:"+toolUseID+":"+approverID (a StepApproval) is committed in the tree sth signs.
// Resolves the record by its step name and delegates to ProveRecord. Fan-in counterpart to
// ProveToolCall for approvals: ProveStep matches only StepValue records, so it cannot find a
// decision. The disclosed record carries the approver id and signature, so a verifier holding
// the approver's key can check it over agent.ApprovalDecisionBytes independently.
func ProveApproval(ctx context.Context, store agent.Durable, runID, toolUseID, approverID string, sth SignedTreeHead) (ProofBundle, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return ProofBundle{}, fmt.Errorf("audit: load journal %s: %w", runID, err)
	}
	idx := findApproval(recs, toolUseID, approverID)
	if idx < 0 {
		return ProofBundle{}, fmt.Errorf("audit: no decision by approver %q on tool call %q in run %s", approverID, toolUseID, runID)
	}
	return ProveRecord(ctx, store, runID, idx, sth)
}

// ApprovalEvidence builds, under one STH, one EvidenceAction per approver in approvers that
// recorded a decision on toolUseID before the tool call completed, followed by the
// EvidenceAction for the tool-result record itself. The ordering is explicit and load-bearing:
// approver decisions first (in the order given by approvers), the action last, and every
// included decision sits at a lower journal index than the action. That lets an offline
// verifier confirm k distinct signed eligible approvers decided BEFORE the action executed by
// checking each decision's signature (Record.Approver, Record.Signature over
// agent.ApprovalDecisionBytes) and comparing Inclusion.Index against the action's.
//
// Approvers with no decision, or whose decision was journaled after the action (and so could
// not have gated it), are omitted rather than failing the call; duplicate ids are proven once.
// It errors if the tool call has no completed result, or if any proof cannot be built against
// sth. Decisions carry Kind "approval" and Ref set to the decision's step name; the action
// carries Kind "tool" and Ref toolUseID, so the result appends directly to
// EvidencePackage.Actions.
func ApprovalEvidence(ctx context.Context, store agent.Durable, runID, toolUseID string, approvers []string, sth SignedTreeHead) ([]EvidenceAction, error) {
	action, err := ProveToolCall(ctx, store, runID, toolUseID, sth)
	if err != nil {
		return nil, err
	}
	recs, err := store.History(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("audit: load journal %s: %w", runID, err)
	}
	out := make([]EvidenceAction, 0, len(approvers)+1)
	seen := make(map[string]bool, len(approvers))
	for _, id := range approvers {
		if seen[id] {
			continue
		}
		seen[id] = true
		idx := findApproval(recs, toolUseID, id)
		if idx < 0 || idx >= action.Inclusion.Index {
			continue
		}
		pb, err := ProveRecord(ctx, store, runID, idx, sth)
		if err != nil {
			return nil, err
		}
		out = append(out, EvidenceAction{Label: id, Kind: "approval", Ref: agent.ApprovalStepName(toolUseID, id), Bundle: pb})
	}
	return append(out, EvidenceAction{Label: toolUseID, Kind: "tool", Ref: toolUseID, Bundle: action}), nil
}

// ApprovalVerdict is the offline result of checking an m-of-n approval gate from evidence.
type ApprovalVerdict struct {
	// Need is the policy's k.
	Need int `json:"need"`
	// Counted lists the approvers whose approval counted, in evidence order: eligible, signed
	// with a signature that verifies under the approver's key, proven under the action's
	// signed tree head, and journaled before the action.
	Counted []string `json:"counted"`
	// Ignored lists every other decision on the action, with the reason it did not count.
	Ignored []IgnoredDecision `json:"ignored,omitempty"`
	// OK reports len(Counted) >= Need.
	OK bool `json:"ok"`
}

// IgnoredDecision is one approver decision that did not count toward the gate, and why.
type IgnoredDecision struct {
	Approver string `json:"approver"`
	Reason   string `json:"reason"`
}

// VerifyApprovals checks offline that k distinct eligible approvers signed approval of the
// tool call toolUseID before it executed. actions is either the output of ApprovalEvidence or
// a whole EvidencePackage's Actions with the decisions appended; the action is located by
// Kind "tool" and its tool-use id, and decisions by Kind "approval" on the same tool call, so
// order does not matter.
//
// It trusts only its inputs: logPub is the log operator's key (obtained out of band), and
// verifierFor resolves an approver id to the key that verifies that approver's signature. The
// same resolver the gate uses at run time (agent.Agent.WithApproverVerifiers) works here. A
// decision counts only if its proof verifies under logPub against the SAME signed tree head
// as the action, it belongs to the same run, the approver is in policy.Approvers, it is an
// approval, its journal index is below the action's, its signature verifies over
// agent.ApprovalDecisionBytes, and it is that approver's first counted decision. Every other
// decision is reported in Ignored with a reason.
//
// It returns an error, rather than a verdict, when the evidence cannot be evaluated at all:
// an invalid policy, no action for toolUseID, or an action whose own proof does not verify.
// It certifies that the gate held, not that the approvers' judgment was right.
func VerifyApprovals(actions []EvidenceAction, toolUseID string, policy agent.ApprovalPolicy, verifierFor agent.ApproverVerifierFor, logPub ed25519.PublicKey) (ApprovalVerdict, error) {
	v := ApprovalVerdict{Need: policy.Need}
	if policy.Need < 1 || policy.Need > len(policy.Approvers) {
		return v, fmt.Errorf("audit: approval policy Need = %d, want 1 <= Need <= %d approvers", policy.Need, len(policy.Approvers))
	}
	if verifierFor == nil {
		return v, fmt.Errorf("audit: no approver verifier resolver")
	}

	var action *EvidenceAction
	for i := range actions {
		a := &actions[i]
		if a.Kind == "tool" && a.Bundle.Record.ToolUseID == toolUseID {
			action = a
			break
		}
	}
	if action == nil {
		return v, fmt.Errorf("audit: no tool action for tool call %q in the evidence", toolUseID)
	}
	if ok, err := action.Bundle.Verify(logPub); err != nil || !ok {
		return v, fmt.Errorf("audit: the proof for tool call %q does not verify under the log key", toolUseID)
	}

	eligible := make(map[string]bool, len(policy.Approvers))
	for _, id := range policy.Approvers {
		eligible[id] = true
	}
	counted := map[string]bool{}
	ignore := func(id, reason string) {
		v.Ignored = append(v.Ignored, IgnoredDecision{Approver: id, Reason: reason})
	}
	for _, a := range actions {
		r := a.Bundle.Record
		if a.Kind != "approval" || r.Kind != agent.StepApproval || r.ToolUseID != toolUseID {
			continue
		}
		switch {
		case !sameTreeHead(a.Bundle.STH, action.Bundle.STH):
			ignore(r.Approver, "not proven under the action's signed tree head")
		case a.Bundle.RunID != action.Bundle.RunID:
			ignore(r.Approver, "belongs to a different run")
		case !verifyBundle(a.Bundle, logPub):
			ignore(r.Approver, "inclusion proof does not verify under the log key")
		case !eligible[r.Approver]:
			ignore(r.Approver, "not an eligible approver")
		case counted[r.Approver]:
			ignore(r.Approver, "duplicate decision")
		case !r.Approved:
			ignore(r.Approver, "denied")
		case a.Bundle.Inclusion.Index >= action.Bundle.Inclusion.Index:
			ignore(r.Approver, "recorded after the action")
		case !verifyDecision(verifierFor, a.Bundle.RunID, r):
			ignore(r.Approver, "signature does not verify under the approver's key")
		default:
			counted[r.Approver] = true
			v.Counted = append(v.Counted, r.Approver)
		}
	}
	v.OK = len(v.Counted) >= policy.Need
	return v, nil
}

func sameTreeHead(a, b SignedTreeHead) bool {
	return a.Size == b.Size && bytes.Equal(a.Root, b.Root) && bytes.Equal(a.Signature, b.Signature)
}

func verifyBundle(b ProofBundle, logPub ed25519.PublicKey) bool {
	ok, err := b.Verify(logPub)
	return err == nil && ok
}

func verifyDecision(verifierFor agent.ApproverVerifierFor, runID string, r agent.Record) bool {
	ver, ok := verifierFor(r.Approver)
	if !ok || ver == nil {
		return false
	}
	return ver.Verify(agent.ApprovalDecisionBytes(runID, r.ToolUseID, r.Approver, r.Approved), r.Signature)
}
