package audit

import (
	"context"
	"fmt"

	"github.com/bide-ai/bide/agent"
)

// approvalStepName is the durable step name agent.ApproveAs journals one approver's decision
// under. It must stay in lockstep with agent's approvalStepPrefix.
func approvalStepName(toolUseID, approverID string) string {
	return "approval:" + toolUseID + ":" + approverID
}

// findApproval returns the journal index of approverID's decision record for toolUseID, or -1.
func findApproval(recs []agent.Record, toolUseID, approverID string) int {
	name := approvalStepName(toolUseID, approverID)
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
		out = append(out, EvidenceAction{Label: id, Kind: "approval", Ref: approvalStepName(toolUseID, id), Bundle: pb})
	}
	return append(out, EvidenceAction{Label: toolUseID, Kind: "tool", Ref: toolUseID, Bundle: action}), nil
}
