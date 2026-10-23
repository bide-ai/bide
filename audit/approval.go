package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/escalation"
)

// EvidenceKind names what one item of an EvidencePackage proves.
type EvidenceKind string

// Evidence kinds for an m-of-n approval, in the order ApprovalEvidence emits them.
const (
	KindCall          EvidenceKind = "call"           // the model turn that requested the gated call (its name and arguments)
	KindApproval      EvidenceKind = "approval"       // one approver decision record
	KindApprovalTally EvidenceKind = "approval-tally" // the gate's journaled terminal tally
	KindTool          EvidenceKind = "tool"           // the call's result: the action, or the denial the model received
)

// Other evidence kinds.
const (
	KindStep           EvidenceKind = "step"            // a durable step's recorded value
	KindGrant          EvidenceKind = "grant"           // an anchored signed grant leaf
	KindRunCertificate EvidenceKind = "run-certificate" // the package's run certificate (a report item only)
	KindConsistency    EvidenceKind = "consistency"     // the package's consistency proof (a report item only)
)

// ProveApproval builds a ProofBundle proving one approver-decision record, named step, is
// committed in the tree sth signs. Decision records are named per decision (see
// agent.ApprovalTally.Records and agent.DecisionCheck.Step), so the step name identifies one
// exactly. The disclosed record carries the approver id and signature, so a verifier holding
// the approver's key can check it over agent.ApprovalDecisionBytes independently. ProveStep
// matches only StepValue records, so it cannot reach a decision.
func ProveApproval(ctx context.Context, store *agent.Journal, runID, step string, sth SignedTreeHead) (ProofBundle, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return ProofBundle{}, fmt.Errorf("audit: load journal %s: %w", runID, err)
	}
	for i, r := range recs {
		if r.Kind == agent.StepApproval && r.Name == step && r.Approver() != "" {
			return ProveRecord(ctx, store, runID, i, sth)
		}
	}
	return ProofBundle{}, fmt.Errorf("audit: no approver decision %q in run %s", step, runID)
}

// ApprovalEvidence builds the complete evidence for an m-of-n gated call under one STH, in
// this order: for a gate whose tally excluded the requester (agent.ApprovalTally.ExcludePrincipal),
// the run's run:principal record (Kind "step", Ref "run:principal"; agent.RunPrincipal), which names
// the requesters and nothing of the run's input; the model turn that requested the call (Kind
// "call"), the record that the run
// paused at the gate (agent.ApprovalWait, Kind "step", when the journal holds one: it carries when
// approvers were asked), every decision record the
// gate read (Kind "approval", from the journaled tally's Records, in journal order), the
// gate's terminal tally (Kind "approval-tally"), and the call's result (Kind "tool", labelled
// by its tool-use id as every tool action is). For a gated plan flow node, toolUseID is the
// node's key ("node:<name>", "node:iter:<n>:<name>"): the request is the gate's recorded
// agent.ApprovalRequest and the result is the node's own record (Kind "step"). The result appends to EvidencePackage.Actions,
// where each entry verifies as a package action; drop the trailing "tool" entry if the package
// already carries the call.
//
// It discloses everything the gate read, valid or not, so VerifyApprovals can recount from the
// same inputs and detect an omission. It errors if the call has no journaled tally (it was not
// m-of-n gated, or its gate has not reached an outcome), no recorded request, or no result, or
// if a proof cannot be built against sth. The tally is read as VerifyApprovals reads it
// (UnmarshalStrict), so a tally that reads two ways is ErrMalformed here too.
func ApprovalEvidence(ctx context.Context, store *agent.Journal, runID, toolUseID string, sth SignedTreeHead) ([]EvidenceAction, error) {
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
	// The gate's outcome, as the runtime reads it (agent.RecordedTally): a re-approval tally over a
	// tally it supersedes (one that predates agent.DecisionFormat, or one that passed without
	// excluding the requester when the gate's policy came to exclude it), else its tally.
	// A gate with a certificate-bound approver records under agent.ApprovalIdentTallyStep, and an exec
	// hold supersedes the tally of its generation: the holds are part of what decides the outcome.
	byName := agent.GateTallyValues(recs, toolUseID)
	rt, ok := agent.RecordedTally(byName, toolUseID)
	if !ok {
		return nil, fmt.Errorf("audit: no journaled approval tally for call %q in run %s (not m-of-n gated, or its gate has not decided)", toolUseID, runID)
	}
	tallyName := rt.Name
	tallyIdx := index[tallyName]
	var tally agent.ApprovalTally
	if err := UnmarshalStrict(recs[tallyIdx].Result, &tally); err != nil {
		return nil, fmt.Errorf("audit: decode %s: %w", tallyName, err)
	}
	callIdx, call, ok := agent.FindToolCall(recs, toolUseID)
	if !ok {
		return nil, fmt.Errorf("audit: no recorded request for call %q in run %s", toolUseID, runID)
	}
	// A gated plan flow node's request is its ApprovalRequest, and its result is the node's own
	// record (a step named by the node's key); an agent tool call's result is its tool result.
	flowNode := recs[callIdx].Name == agent.ApprovalRequestStep(toolUseID)
	// The escalation instance of a call (the taint policy's gate, subject "<call>#taint") has no result of
	// its own: its result is the call's.
	resultID := toolUseID
	if base, ok := escalation.Base(toolUseID); ok {
		resultID = base
	}
	result := EvidenceAction{Label: resultID, Kind: KindTool, Ref: resultID}
	if flowNode {
		result.Kind = KindStep
		result.Bundle, err = ProveStep(ctx, store, runID, toolUseID, sth)
	} else {
		result.Bundle, err = ProveToolCall(ctx, store, runID, resultID, sth)
	}
	if err != nil {
		return nil, err
	}

	prove := func(i int) (ProofBundle, error) { return ProveRecord(ctx, store, runID, i, sth) }
	callPB, err := prove(callIdx)
	if err != nil {
		return nil, err
	}
	var out []EvidenceAction
	if tally.ExcludePrincipal {
		// Separation of duties: the requesters are the principals the run's run:principal names, so
		// the evidence proves that record (it holds no run input), for VerifyApprovals to recount
		// with the same exclusion.
		i := slices.IndexFunc(recs, agent.IsRunPrincipal)
		if i < 0 {
			return nil, fmt.Errorf("audit: the gate on call %q excluded the requester, but run %s has no run:principal record naming one", toolUseID, runID)
		}
		pb, err := prove(i)
		if err != nil {
			return nil, err
		}
		out = append(out, EvidenceAction{Label: recs[i].Name, Kind: KindStep, Ref: recs[i].Name, Bundle: pb})
	}
	out = append(out, EvidenceAction{Label: call.Name, Kind: KindCall, Ref: toolUseID, Bundle: callPB})
	if i, ok := index[agent.ApprovalPendingStep(toolUseID)]; ok && recs[i].Kind == agent.StepValue {
		pb, err := prove(i)
		if err != nil {
			return nil, err
		}
		out = append(out, EvidenceAction{Label: recs[i].Name, Kind: KindStep, Ref: recs[i].Name, Bundle: pb})
	}
	for _, name := range tally.Records {
		i, ok := index[name]
		if !ok {
			return nil, fmt.Errorf("audit: the tally for call %q names decision %q, which is not in run %s", toolUseID, name, runID)
		}
		pb, err := prove(i)
		if err != nil {
			return nil, err
		}
		out = append(out, EvidenceAction{Label: recs[i].Approver(), Kind: KindApproval, Ref: name, Bundle: pb})
	}
	tallyPB, err := prove(tallyIdx)
	if err != nil {
		return nil, err
	}
	// A re-approval tally travels with the tallies it supersedes, in journal order: VerifyApprovals
	// honors it only over the tally it supersedes, which it can tell only from that tally.
	var superseded []int
	holds := map[int]bool{}
	for _, n := range agent.SupersededTallies(byName, tallyName) {
		superseded = append(superseded, index[n])
	}
	// An exec hold travels with the identity tally it superseded (Kind "step"): without it the
	// superseded tally would read as the outcome.
	for _, n := range agent.SupersededTallies(byName, tallyName) {
		if g, ok := agent.IdentTallyGen(toolUseID, n); ok {
			if i, ok := index[agent.ApprovalExecHoldStep(toolUseID, g)]; ok && recs[i].Kind == agent.StepValue {
				superseded = append(superseded, i)
				holds[i] = true
			}
		}
	}
	slices.Sort(superseded)
	for _, i := range superseded {
		pb, err := prove(i)
		if err != nil {
			return nil, err
		}
		if holds[i] {
			out = append(out, EvidenceAction{Label: recs[i].Name, Kind: KindStep, Ref: recs[i].Name, Bundle: pb})
			continue
		}
		out = append(out, EvidenceAction{Label: "approval tally (superseded)", Kind: KindApprovalTally, Ref: recs[i].Name, Bundle: pb})
	}
	// The gate's re-ask notes (agent.ApprovalReaskStep), when it asked again: when, why, and the
	// policy it asked under (Kind "step").
	reaskNames := []string{agent.ApprovalReaskStep(toolUseID, false), agent.ApprovalReaskStep(toolUseID, true)}
	for _, r := range recs { // a certificate-bound gate's re-asks, one per generation
		if _, ok := agent.IdentReaskGen(toolUseID, r.Name); ok && r.Kind == agent.StepValue {
			reaskNames = append(reaskNames, r.Name)
		}
	}
	for _, n := range reaskNames {
		if i, ok := index[n]; ok && recs[i].Kind == agent.StepValue {
			pb, err := prove(i)
			if err != nil {
				return nil, err
			}
			out = append(out, EvidenceAction{Label: n, Kind: KindStep, Ref: n, Bundle: pb})
		}
	}
	out = append(out,
		EvidenceAction{Label: "approval tally", Kind: KindApprovalTally, Ref: tallyName, Bundle: tallyPB},
		result)
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
	// Times lists the two times of every counted decision (approvals and denials), in journal
	// order, with sanity flags under DefaultDecisionTimeTolerance (see FlagDecisionTimes). They are
	// for a person reading the verdict: no time, and no flag, affects the count or OK.
	Times []DecisionTime `json:"times,omitempty"`
	// AskedAt is when the run asked for approval, by the run's clock: for a gated plan flow node the
	// recorded_at of its proven request (agent.ApprovalRequest), else that of the proven
	// approval-pending record (agent.ApprovalWait); "" when the evidence holds neither with a time
	// (each decision is then flagged FlagAskedUnknown).
	AskedAt string `json:"asked_at,omitempty"`
	// Requesters are the requesters the recount excluded, from the proven run:principal record
	// (agent.RunPrincipal.Requesters: the principals of the runs above the run, root first, then the
	// run's own), when the expected policy excludes the requester
	// (agent.ApprovalPolicy.ExcludePrincipal); empty otherwise, or when the evidence does not prove
	// one.
	Requesters []string `json:"requesters,omitempty"`
	// Reask and ReaskedAt: when the gate asked again over a recorded tally it did not act on, why
	// (agent.PendingReapproval, agent.PendingSoDReapproval) and when (its recorded_at, by the run's
	// clock), from the newest proven re-ask note (agent.ApprovalReaskStep); empty when the evidence
	// holds none. Display only, like AskedAt.
	Reask     string `json:"reask,omitempty"`
	ReaskedAt string `json:"reasked_at,omitempty"`
	// HeadAt is when the signed tree head the evidence is proven under was signed (its
	// TimestampNanos, in agent.FormatTimestamp's form): every record it covers, the decisions
	// included, was journaled by then.
	HeadAt string `json:"head_at,omitempty"`
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

// The audit verifiers are approver verifiers: they report key identities (KeyIDs).
var (
	_ agent.ApproverVerifier = Ed25519Verifier{}
	_ agent.ApproverVerifier = MLDSAVerifier{}
	_ agent.ApproverVerifier = HybridVerifier{}
)

// VerifyApprovals checks offline that the m-of-n gate on toolUseID held, trusting only its
// inputs: actions (ApprovalEvidence's output, alone or inside an EvidencePackage), the policy
// the auditor expects, verifierFor (each approver's key, the same resolver shape the gate uses
// at run time), and logV (the log operator's key, obtained out of band).
//
// It verifies the proofs of the request, the gate's journaled tally, and the call's result,
// all under one signed tree head and in that journal order (for a gated plan flow node, the
// request is its agent.ApprovalRequest and the result its node record, as ApprovalEvidence
// emits them), then recounts the disclosed
// decisions with agent.TallyApprovals, the exact rule the gate ran, against the proven call's
// name and arguments. It reports a Problem if the evidence omits a decision the gate read, if
// the recount disagrees with the journaled tally, or if the gate enforced a different policy
// than expected. A decision signed for other arguments, another run, or another call, or under
// another scheme than its approver's key, does not verify and is listed in Ignored. So does a
// decision whose signed decision time was changed, is missing (one signed under an earlier
// decision format), or is not in canonical form.
//
// Separation of duties: when the expected policy excludes the requester, or the journaled tally
// says the gate did, the evidence must prove the run's run:principal record (ApprovalEvidence
// emits it; it holds the principals and nothing of the run's input), under the same signed tree
// head and run, before the request. The recount reads the requesters from it, as the gate did, and
// lists their decisions in Ignored (agent.ReasonRequester). It reports a Problem if the gate's
// exclusion differs from the expected policy's, if the tally names other requesters than the
// run:principal, or if the run:principal is missing, not proven, or not before the request; without
// it the recount counts no one.
//
// It reports each counted decision's signed decision time and recorded time in Times, flagged
// under DefaultDecisionTimeTolerance, and when the run was asked in AskedAt. These are display
// only: they never change Counted, Problems or OK.
//
// It returns the verdict with a nil error when the gate held (OK), and with an error wrapping
// ErrNotVerified when it did not: Problems, too few approvals, or evidence that lacks the request,
// the tally, or the result, or whose proof of one of them does not verify. An error wrapping
// agent.ErrConfig is an invalid expected policy, no resolver, or a policy two of whose approvers
// resolve to one signing key under verifierFor or one of whose approvers' verifiers reports no key
// identity (see agent.ApprovalPolicy.ValidateKeys); ErrFormat or ErrMalformed, a proof or a record
// that cannot be read. It certifies that the gate held as recorded, not that the approvers'
// judgment was right.
func VerifyApprovals(actions []EvidenceAction, toolUseID string, policy agent.ApprovalPolicy, verifierFor agent.ApproverVerifierFor, logV Verifier) (ApprovalVerdict, error) {
	v := ApprovalVerdict{Need: policy.Need}
	if err := policy.Validate(); err != nil {
		return v, fmt.Errorf("audit: expected policy: %w", err)
	}
	if verifierFor == nil {
		return v, fmt.Errorf("audit: no approver verifier resolver: %w", agent.ErrConfig)
	}
	if err := checkVerifier(logV); err != nil {
		return v, err
	}
	if err := policy.ValidateKeys(verifierFor); err != nil {
		return v, fmt.Errorf("audit: expected policy under the approver keys: %w", err)
	}

	// Each action's proven record, decoded once. An action whose record bytes do not decode is
	// malformed evidence.
	recs := make([]agent.Record, len(actions))
	for i, a := range actions {
		r, err := a.Bundle.Record()
		if err != nil {
			return v, fmt.Errorf("audit: %s action %d: %w", a.Kind, i, err)
		}
		recs[i] = r
	}
	find := func(match func(EvidenceAction, agent.Record) bool, what string) (int, error) {
		for i := range actions {
			if match(actions[i], recs[i]) {
				return i, nil
			}
		}
		return -1, notVerified("audit: the evidence has no %s for call %q", what, toolUseID)
	}
	var call agent.ToolUse
	var flowNode bool
	reqI, err := find(func(a EvidenceAction, r agent.Record) bool {
		if a.Kind != KindCall {
			return false
		}
		_, tu, ok := agent.FindToolCall([]agent.Record{r}, toolUseID)
		call, flowNode = tu, ok && r.Name == agent.ApprovalRequestStep(toolUseID)
		return ok
	}, "request")
	if err != nil {
		return v, err
	}
	// The gate's outcome, as the runtime reads it (agent.RecordedTally): a re-approval tally over a
	// tally it supersedes, else its tally. A re-approval tally beside any other tally is not the
	// gate's outcome. Every tally that decides the choice must be proven, like the outcome (below).
	byName := make(map[string]agent.Record, 3)
	tallyAt := map[string]int{}
	for i, a := range actions {
		// An exec hold (Kind "step") supersedes the identity tally of its generation, so it decides
		// which tally is the outcome like a tally does.
		_, isHold := agent.ExecHoldGen(toolUseID, recs[i].Name)
		if a.Kind != KindApprovalTally && !(a.Kind == KindStep && isHold && recs[i].Kind == agent.StepValue) {
			continue
		}
		if _, dup := byName[recs[i].Name]; !dup {
			byName[recs[i].Name] = recs[i]
			tallyAt[recs[i].Name] = i
		}
	}
	rt, found := agent.RecordedTally(byName, toolUseID)
	if !found {
		return v, notVerified("audit: the evidence has no %s for call %q", "approval tally", toolUseID)
	}
	tallyName := rt.Name
	tallyI, err := find(func(a EvidenceAction, r agent.Record) bool {
		return a.Kind == KindApprovalTally && r.Name == tallyName
	}, "approval tally")
	if err != nil {
		return v, err
	}
	// The result of a gated plan flow node is the node's record, named by its key.
	resultI, err := find(func(a EvidenceAction, r agent.Record) bool {
		if flowNode {
			return a.Kind == KindStep && r.Kind == agent.StepValue && r.Name == toolUseID
		}
		resultID := toolUseID
		if base, ok := escalation.Base(toolUseID); ok {
			resultID = base // an escalation instance's result is its call's
		}
		return a.Kind == KindTool && r.Kind == agent.StepToolResult && r.ToolUseID == resultID
	}, "result")
	if err != nil {
		return v, err
	}
	req, tallyAct, result := &actions[reqI], &actions[tallyI], &actions[resultI]
	sth, runID := result.Bundle.STH, result.Bundle.RunID
	mustProve := []*EvidenceAction{req, tallyAct, result}
	// The tallies the outcome supersedes decide which tally is the outcome: they are proven too, and
	// must come before it.
	superseded := agent.SupersededTallies(byName, tallyName)
	for _, n := range superseded {
		mustProve = append(mustProve, &actions[tallyAt[n]])
		if g, ok := agent.IdentTallyGen(toolUseID, n); ok {
			if at, ok := tallyAt[agent.ApprovalExecHoldStep(toolUseID, g)]; ok {
				mustProve = append(mustProve, &actions[at])
			}
		}
	}
	for _, a := range mustProve {
		if err := a.Bundle.Verify(logV); err != nil {
			if !errors.Is(err, ErrNotVerified) {
				return v, err
			}
			return v, fmt.Errorf("audit: the proof of the %s for call %q does not verify under the log key: %w", a.Kind, toolUseID, err)
		}
		if !sameTreeHead(a.Bundle.STH, sth) || a.Bundle.RunID != runID {
			return v, notVerified("audit: the %s for call %q is not proven under the same signed tree head and run as its result", a.Kind, toolUseID)
		}
	}
	v.ToolName, v.Args = call.Name, call.Args
	// The tally is read strictly (UnmarshalStrict: no duplicate or case-variant name, no unknown
	// field), as the run certificate's leaves are: a result that read one way to this check and
	// another to a reader of the proof would let the verdict rest on a policy the proof does not
	// say the gate enforced.
	var tally agent.ApprovalTally
	if err := UnmarshalStrict(recs[tallyI].Result, &tally); err != nil {
		return v, fmt.Errorf("audit: decode the journaled tally: %w", err)
	}
	problem := func(format string, args ...any) { v.Problems = append(v.Problems, fmt.Sprintf(format, args...)) }

	reqIdx, tallyIdx, resultIdx := req.Bundle.Inclusion.Index, tallyAct.Bundle.Inclusion.Index, result.Bundle.Inclusion.Index
	if !(reqIdx < tallyIdx && tallyIdx < resultIdx) {
		problem("records out of order: request at %d, tally at %d, result at %d (want request < tally < result)", reqIdx, tallyIdx, resultIdx)
	}
	for _, n := range superseded {
		if at := actions[tallyAt[n]].Bundle.Inclusion.Index; at >= tallyIdx {
			problem("the superseded tally %q is at %d, not before the tally that supersedes it at %d", n, at, tallyIdx)
		}
		if g, ok := agent.IdentTallyGen(toolUseID, n); ok {
			// The hold of that generation must be journaled after the tally it holds and before the
			// tally that replaces it.
			if hi, held := tallyAt[agent.ApprovalExecHoldStep(toolUseID, g)]; held {
				if at := actions[hi].Bundle.Inclusion.Index; at >= tallyIdx || at <= actions[tallyAt[n]].Bundle.Inclusion.Index {
					problem("the exec hold of generation %d is at %d, not between the tally it holds and the tally that replaces it at %d", g, at, tallyIdx)
				}
			}
		}
	}
	if tally.Need != policy.Need || !slices.Equal(tally.Approvers, policy.Approvers) {
		problem("the gate enforced need %d of %v, not the expected need %d of %v", tally.Need, tally.Approvers, policy.Need, policy.Approvers)
	}
	// Separation of duties: the run's run:principal names the requesters, proven like the request.
	var start []agent.Record
	if policy.ExcludePrincipal || tally.ExcludePrincipal {
		if tally.ExcludePrincipal != policy.ExcludePrincipal {
			problem("the gate's exclusion of the requester (exclude_principal %v) differs from the expected policy's (%v)", tally.ExcludePrincipal, policy.ExcludePrincipal)
		}
		startI := -1
		for i, a := range actions {
			if a.Kind == KindStep && agent.IsRunPrincipal(recs[i]) {
				startI = i
				break
			}
		}
		if startI < 0 {
			problem("the evidence has no run:principal record, which names the requesters the policy excludes; the recount counts no one")
		} else {
			sa := &actions[startI]
			err := sa.Bundle.Verify(logV)
			if err != nil && !errors.Is(err, ErrNotVerified) {
				return v, err
			}
			switch {
			case err != nil:
				problem("the proof of the run:principal record does not verify under the log key; the recount counts no one")
			case !sameTreeHead(sa.Bundle.STH, sth) || sa.Bundle.RunID != runID:
				problem("the run:principal record is not proven under the same signed tree head and run as the result; the recount counts no one")
			case sa.Bundle.Inclusion.Index >= reqIdx:
				problem("the run:principal record is at %d, not before the request at %d; the recount counts no one", sa.Bundle.Inclusion.Index, reqIdx)
			default:
				start = []agent.Record{recs[startI]}
			}
		}
		if rp, ok := agent.RecordedPrincipal(start); ok {
			v.Requesters = rp.Requesters()
			if tally.ExcludePrincipal && !slices.Equal(tally.Requesters, v.Requesters) {
				problem("the gate's tally names requesters %q, but the run's run:principal names %q", tally.Requesters, v.Requesters)
			}
		}
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
	for i, a := range actions {
		r := recs[i]
		if a.Kind != KindApproval || !agent.IsApprovalDecision(r, toolUseID) {
			continue
		}
		if _, dup := proven[r.Name]; dup {
			continue
		}
		ignore := func(reason string) {
			v.Ignored = append(v.Ignored, IgnoredDecision{Approver: r.Approver(), Step: r.Name, Reason: reason})
		}
		err := a.Bundle.Verify(logV)
		if err != nil && !errors.Is(err, ErrNotVerified) {
			return v, err
		}
		switch {
		case err != nil:
			ignore("inclusion proof does not verify under the log key")
		case !sameTreeHead(a.Bundle.STH, sth) || a.Bundle.RunID != runID:
			ignore("not proven under the same signed tree head and run as the result")
		case !read[r.Name] && tally.Expired:
			ignore(agent.ReasonAfterExpiry)
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
	decisions := make([]agent.Record, len(recount))
	for i, d := range recount {
		decisions[i] = d.rec
	}

	subject := agent.ApprovalSubject{RunID: runID, ToolUseID: toolUseID, ToolName: call.Name, Args: call.Args}
	got, checks := agent.TallyApprovals(append(start, decisions...), subject, policy, verifierFor)
	for _, c := range checks {
		if !c.Counted {
			v.Ignored = append(v.Ignored, IgnoredDecision{Approver: c.Approver, Step: c.Step, Reason: c.Reason})
			continue
		}
		r := proven[c.Step].rec
		v.Times = append(v.Times, DecisionTime{Approver: c.Approver, Step: c.Step, DecidedAt: r.DecidedAt(), RecordedAt: r.RecordedAt()})
	}
	v.AskedAt = askedAt(actions, recs, recs[reqI], flowNode, toolUseID, sth, runID, logV)
	v.Reask, v.ReaskedAt = reaskedAt(actions, recs, toolUseID, sth, runID, logV)
	if at := agent.FormatTimestamp(time.Unix(0, sth.TimestampNanos)); validTimestamp(at) {
		v.HeadAt = at
	}
	v.Times = CheckDecisionTimes(v.Times, TimeCheck{AskedAt: v.AskedAt, HeadAt: v.HeadAt, Tolerance: DefaultDecisionTimeTolerance})
	v.Counted, v.DeniedBy = got.ApprovedBy, got.DeniedBy
	if !slices.Equal(got.ApprovedBy, tally.ApprovedBy) || !slices.Equal(got.DeniedBy, tally.DeniedBy) {
		problem("the recount (approved by %v, denied by %v) disagrees with the gate's journaled tally (approved by %v, denied by %v)",
			got.ApprovedBy, got.DeniedBy, tally.ApprovedBy, tally.DeniedBy)
	}
	v.OK = len(v.Problems) == 0 && got.Passed()
	if !v.OK {
		return v, notVerified("audit: the approval gate on call %q: %d of %d required approvals verified, %d problems", toolUseID, len(v.Counted), v.Need, len(v.Problems))
	}
	return v, nil
}

func sameTreeHead(a, b SignedTreeHead) bool {
	return a.Size == b.Size && bytes.Equal(a.Root, b.Root) && bytes.Equal(a.Signature, b.Signature)
}
