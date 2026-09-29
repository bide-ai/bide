package audit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"slices"
	"sort"

	"github.com/bide-ai/bide/agent"
)

// Evidence kinds for an m-of-n approval, in the order ApprovalEvidence emits them.
const (
	KindCall          = "call"           // the model turn that requested the gated call (its name and arguments)
	KindApproval      = "approval"       // one approver decision record
	KindApprovalTally = "approval-tally" // the gate's journaled terminal tally
	KindTool          = "tool"           // the call's result: the action, or the denial the model received
)

// ProveApproval builds a ProofBundle proving one approver-decision record, named step, is
// committed in the tree sth signs. Decision records are named per decision (see
// agent.ApprovalTally.Records and agent.DecisionCheck.Step), so the step name identifies one
// exactly. The disclosed record carries the approver id and signature, so a verifier holding
// the approver's key can check it over agent.ApprovalDecisionBytes independently. ProveStep
// matches only StepValue records, so it cannot reach a decision.
func ProveApproval(ctx context.Context, store agent.Durable, runID, step string, sth SignedTreeHead) (ProofBundle, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return ProofBundle{}, fmt.Errorf("audit: load journal %s: %w", runID, err)
	}
	for i, r := range recs {
		if r.Kind == agent.StepApproval && r.Name == step && r.Approver != "" {
			return ProveRecord(ctx, store, runID, i, sth)
		}
	}
	return ProofBundle{}, fmt.Errorf("audit: no approver decision %q in run %s", step, runID)
}

// ApprovalEvidence builds the complete evidence for an m-of-n gated call under one STH, in
// this order: the model turn that requested the call (Kind "call"), every decision record the
// gate read (Kind "approval", from the journaled tally's Records, in journal order), the
// gate's terminal tally (Kind "approval-tally"), and the call's result (Kind "tool"). The
// result appends to EvidencePackage.Actions; drop the trailing "tool" entry if the package
// already carries the call.
//
// It discloses everything the gate read, valid or not, so VerifyApprovals can recount from the
// same inputs and detect an omission. It errors if the call has no journaled tally (it was not
// m-of-n gated, or its gate has not reached an outcome), no recorded request, or no result, or
// if a proof cannot be built against sth.
func ApprovalEvidence(ctx context.Context, store agent.Durable, runID, toolUseID string, sth SignedTreeHead) ([]EvidenceAction, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("audit: load journal %s: %w", runID, err)
	}
	index := make(map[string]int, len(recs))
	for i, r := range recs {
		if _, dup := index[r.Name]; !dup {
			index[r.Name] = i
		}
	}
	tallyName := agent.ApprovalTallyStep(toolUseID)
	tallyIdx, ok := index[tallyName]
	if !ok || recs[tallyIdx].Kind != agent.StepValue {
		return nil, fmt.Errorf("audit: no journaled approval tally for call %q in run %s (not m-of-n gated, or its gate has not decided)", toolUseID, runID)
	}
	var tally agent.ApprovalTally
	if err := json.Unmarshal(recs[tallyIdx].Result, &tally); err != nil {
		return nil, fmt.Errorf("audit: decode %s: %w", tallyName, err)
	}
	callIdx, call, ok := agent.FindToolCall(recs, toolUseID)
	if !ok {
		return nil, fmt.Errorf("audit: no recorded request for call %q in run %s", toolUseID, runID)
	}
	action, err := ProveToolCall(ctx, store, runID, toolUseID, sth)
	if err != nil {
		return nil, err
	}

	prove := func(i int) (ProofBundle, error) { return ProveRecord(ctx, store, runID, i, sth) }
	callPB, err := prove(callIdx)
	if err != nil {
		return nil, err
	}
	out := []EvidenceAction{{Label: call.Name, Kind: KindCall, Ref: toolUseID, Bundle: callPB}}
	for _, name := range tally.Records {
		i, ok := index[name]
		if !ok {
			return nil, fmt.Errorf("audit: the tally for call %q names decision %q, which is not in run %s", toolUseID, name, runID)
		}
		pb, err := prove(i)
		if err != nil {
			return nil, err
		}
		out = append(out, EvidenceAction{Label: recs[i].Approver, Kind: KindApproval, Ref: name, Bundle: pb})
	}
	tallyPB, err := prove(tallyIdx)
	if err != nil {
		return nil, err
	}
	out = append(out,
		EvidenceAction{Label: "approval tally", Kind: KindApprovalTally, Ref: tallyName, Bundle: tallyPB},
		EvidenceAction{Label: call.Name, Kind: KindTool, Ref: toolUseID, Bundle: action})
	return out, nil
}

// ApprovalVerdict is the offline result of checking an m-of-n approval gate from evidence.
type ApprovalVerdict struct {
	// Need is the expected policy's k.
	Need int `json:"need"`
	// ToolName and Args are the call the approvers decided on, from the proven request.
	ToolName string          `json:"tool_name"`
	Args     json.RawMessage `json:"args,omitempty"`
	// Counted lists the approvers whose approval counted in the recount, in journal order.
	Counted []string `json:"counted"`
	// DeniedBy lists the approvers whose counted decision was a denial.
	DeniedBy []string `json:"denied_by,omitempty"`
	// Ignored lists every other disclosed decision, with the reason it did not count.
	Ignored []IgnoredDecision `json:"ignored,omitempty"`
	// Problems lists every way the evidence is inconsistent with itself or with the expected
	// policy: an omitted decision the gate read, a recount that disagrees with the gate's
	// journaled tally, a different enforced policy, or records out of order. Any problem fails
	// the verdict.
	Problems []string `json:"problems,omitempty"`
	// OK reports no Problems and at least Need approvals counted.
	OK bool `json:"ok"`
}

// IgnoredDecision is one disclosed approver decision that did not count toward the gate.
type IgnoredDecision struct {
	Approver string `json:"approver"`
	Step     string `json:"step"`
	Reason   string `json:"reason"`
}

// VerifyApprovals checks offline that the m-of-n gate on toolUseID held, trusting only its
// inputs: actions (ApprovalEvidence's output, alone or inside an EvidencePackage), the policy
// the auditor expects, verifierFor (each approver's key, the same resolver shape the gate uses
// at run time), and logPub (the log operator's key, obtained out of band).
//
// It verifies the proofs of the request, the gate's journaled tally, and the call's result,
// all under one signed tree head and in that journal order, then recounts the disclosed
// decisions with agent.TallyApprovals, the exact rule the gate ran, against the proven call's
// name and arguments. It reports a Problem if the evidence omits a decision the gate read, if
// the recount disagrees with the journaled tally, or if the gate enforced a different policy
// than expected. A decision signed for other arguments, another run, or another call does not
// verify and is listed in Ignored.
//
// It returns an error, not a verdict, when the evidence cannot be evaluated: an invalid
// policy, a missing request, tally, or result, or one of those whose proof does not verify.
// It certifies that the gate held as recorded, not that the approvers' judgment was right.
func VerifyApprovals(actions []EvidenceAction, toolUseID string, policy agent.ApprovalPolicy, verifierFor agent.ApproverVerifierFor, logPub ed25519.PublicKey) (ApprovalVerdict, error) {
	v := ApprovalVerdict{Need: policy.Need}
	if err := policy.Validate(); err != nil {
		return v, fmt.Errorf("audit: expected policy: %w", err)
	}
	if verifierFor == nil {
		return v, fmt.Errorf("audit: no approver verifier resolver")
	}

	find := func(match func(EvidenceAction) bool, what string) (*EvidenceAction, error) {
		for i := range actions {
			if match(actions[i]) {
				return &actions[i], nil
			}
		}
		return nil, fmt.Errorf("audit: the evidence has no %s for call %q", what, toolUseID)
	}
	var call agent.ToolUse
	req, err := find(func(a EvidenceAction) bool {
		if a.Kind != KindCall {
			return false
		}
		_, tu, ok := agent.FindToolCall([]agent.Record{a.Bundle.Record}, toolUseID)
		call = tu
		return ok
	}, "request")
	if err != nil {
		return v, err
	}
	tallyName := agent.ApprovalTallyStep(toolUseID)
	tallyAct, err := find(func(a EvidenceAction) bool {
		return a.Kind == KindApprovalTally && a.Bundle.Record.Name == tallyName
	}, "approval tally")
	if err != nil {
		return v, err
	}
	result, err := find(func(a EvidenceAction) bool {
		return a.Kind == KindTool && a.Bundle.Record.Kind == agent.StepToolResult && a.Bundle.Record.ToolUseID == toolUseID
	}, "result")
	if err != nil {
		return v, err
	}
	sth, runID := result.Bundle.STH, result.Bundle.RunID
	for _, a := range []*EvidenceAction{req, tallyAct, result} {
		if !verifyBundle(a.Bundle, logPub) {
			return v, fmt.Errorf("audit: the proof of the %s for call %q does not verify under the log key", a.Kind, toolUseID)
		}
		if !sameTreeHead(a.Bundle.STH, sth) || a.Bundle.RunID != runID {
			return v, fmt.Errorf("audit: the %s for call %q is not proven under the same signed tree head and run as its result", a.Kind, toolUseID)
		}
	}
	v.ToolName, v.Args = call.Name, call.Args
	var tally agent.ApprovalTally
	if err := json.Unmarshal(tallyAct.Bundle.Record.Result, &tally); err != nil {
		return v, fmt.Errorf("audit: decode the journaled tally: %w", err)
	}
	problem := func(format string, args ...any) { v.Problems = append(v.Problems, fmt.Sprintf(format, args...)) }

	reqIdx, tallyIdx, resultIdx := req.Bundle.Inclusion.Index, tallyAct.Bundle.Inclusion.Index, result.Bundle.Inclusion.Index
	if !(reqIdx < tallyIdx && tallyIdx < resultIdx) {
		problem("records out of order: request at %d, tally at %d, result at %d (want request < tally < result)", reqIdx, tallyIdx, resultIdx)
	}
	if tally.Need != policy.Need || !slices.Equal(tally.Approvers, policy.Approvers) {
		problem("the gate enforced need %d of %v, not the expected need %d of %v", tally.Need, tally.Approvers, policy.Need, policy.Approvers)
	}

	// The disclosed decisions whose proofs hold, keyed by record name.
	type disclosed struct {
		rec   agent.Record
		index int
	}
	read := make(map[string]bool, len(tally.Records))
	for _, name := range tally.Records {
		read[name] = true
	}
	proven := map[string]disclosed{}
	for _, a := range actions {
		r := a.Bundle.Record
		if a.Kind != KindApproval || !agent.IsApprovalDecision(r, toolUseID) {
			continue
		}
		if _, dup := proven[r.Name]; dup {
			continue
		}
		ignore := func(reason string) {
			v.Ignored = append(v.Ignored, IgnoredDecision{Approver: r.Approver, Step: r.Name, Reason: reason})
		}
		switch {
		case !verifyBundle(a.Bundle, logPub):
			ignore("inclusion proof does not verify under the log key")
		case !sameTreeHead(a.Bundle.STH, sth) || a.Bundle.RunID != runID:
			ignore("not proven under the same signed tree head and run as the result")
		case !read[r.Name]:
			ignore("not read by the gate (recorded after it decided)")
		default:
			proven[r.Name] = disclosed{r, a.Bundle.Inclusion.Index}
		}
	}

	// Completeness: every decision the gate read is disclosed, proven, and before the tally.
	var recount []disclosed
	for _, name := range tally.Records {
		d, ok := proven[name]
		switch {
		case !ok:
			problem("the evidence omits, or cannot prove, decision %q that the gate read", name)
		case d.index >= tallyIdx:
			problem("decision %q is at %d, not before the tally at %d", name, d.index, tallyIdx)
		default:
			recount = append(recount, d)
		}
	}
	sort.Slice(recount, func(i, j int) bool { return recount[i].index < recount[j].index })
	recs := make([]agent.Record, len(recount))
	for i, d := range recount {
		recs[i] = d.rec
	}

	subject := agent.ApprovalSubject{RunID: runID, ToolUseID: toolUseID, ToolName: call.Name, Args: call.Args}
	got, checks := agent.TallyApprovals(recs, subject, policy, verifierFor)
	for _, c := range checks {
		if !c.Counted {
			v.Ignored = append(v.Ignored, IgnoredDecision{Approver: c.Approver, Step: c.Step, Reason: c.Reason})
		}
	}
	v.Counted, v.DeniedBy = got.ApprovedBy, got.DeniedBy
	if !slices.Equal(got.ApprovedBy, tally.ApprovedBy) || !slices.Equal(got.DeniedBy, tally.DeniedBy) {
		problem("the recount (approved by %v, denied by %v) disagrees with the gate's journaled tally (approved by %v, denied by %v)",
			got.ApprovedBy, got.DeniedBy, tally.ApprovedBy, tally.DeniedBy)
	}
	v.OK = len(v.Problems) == 0 && got.Passed()
	return v, nil
}

func sameTreeHead(a, b SignedTreeHead) bool {
	return a.Size == b.Size && bytes.Equal(a.Root, b.Root) && bytes.Equal(a.Signature, b.Signature)
}

func verifyBundle(b ProofBundle, logPub ed25519.PublicKey) bool {
	ok, err := b.Verify(logPub)
	return err == nil && ok
}
