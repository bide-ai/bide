package agent

import (
	"slices"
)

// GateStatus is where an approval gate stands on one call, as the gate itself reads it.
type GateStatus string

const (
	// GateWaiting: an m-of-n gate with no recorded outcome. The run is paused, or will pause when it
	// reaches the call, and decisions are taken. Tally is the count of the journal (Passed or
	// Unreachable once the decisions suffice, though the gate has not recorded it yet), and Reason
	// says why the gate asks again after a recorded tally it does not accept (PendingReapproval,
	// PendingSoDReapproval).
	GateWaiting GateStatus = "waiting"
	// GateDecided: the call has an outcome the gate acts on: its result is recorded, or a recorded
	// denial (a human's Approve(false), or an m-of-n tally that did not pass), or the gate's recorded
	// terminal tally that it accepts under the policy now. Ran, Denied and Recorded say which.
	GateDecided GateStatus = "decided"
	// GateSingle: the policy is a one-decision gate (SingleApproval). It records an unsigned decision
	// (Approve), not signed per-approver ones, so there is no tally. SingleDecided and
	// SingleApproved say what is recorded.
	GateSingle GateStatus = "single"
	// GateUngated: no policy now (a redeploy removed the gate): the call runs without approval.
	GateUngated GateStatus = "ungated"
	// GateMisconfigured: the gate would refuse to evaluate, so the run fails at the call rather than
	// pauses: an invalid policy, approvers that share a key, no approver verifiers, a policy that
	// excludes the requester for a run that records none, a tally journaled by something that is not
	// this gate. Err is what the gate refuses with (it wraps ErrConfig, or ErrStorage for a tally
	// that does not read).
	GateMisconfigured GateStatus = "misconfigured"
)

// GateStanding is the result of EvaluateGate.
type GateStanding struct {
	Status GateStatus
	// Tally is the gate's recorded outcome (Recorded) or the count of the journal. For a recorded
	// outcome, Checks classify the decision records by a recount and Counted is cleared for a
	// decision the recorded tally does not name.
	Tally    ApprovalTally
	Recorded bool
	Checks   []DecisionCheck
	Reason   string // why a waiting gate asks again
	Err      error  // set when Status is GateMisconfigured
	// Ran: the call has a result. Denied: it was denied (a recorded denial, or the result of one).
	Ran, Denied bool
	// SingleDecided, SingleApproved: for GateSingle, a recorded unsigned decision and its verdict.
	SingleDecided, SingleApproved bool
}

// EvaluateGate says where the approval gate stands on the call s in runID's journal recs, under the
// policy pol it is built with now (nil: the tool has no gate) and the approver verifiers, exactly as
// the agent's own gate reads it: the same function counts for both, so a service that tells approvers
// whether a call waits cannot disagree with the run. It records nothing.
//
// The order is the loop's: a recorded result, a recorded denial (final, whatever the policy is now),
// the policy (no gate, a one-decision gate), then, for an m-of-n gate, the checks it makes on every
// evaluation (the policy valid, verifiers present, one seat per key, the requester excluded where
// the policy says so, a recorded tally readable), the recorded outcome it accepts or supersedes
// (GateReask), and the count.
//
// recs are the run's records (Journal.History), s names the call (RunID, ToolUseID, ToolName, Args),
// and a call of a sub-agent is evaluated in the sub-run's journal. A plan flow node's gate is
// evaluated with EvaluateGateAfter.
func EvaluateGate(recs []Record, s ApprovalSubject, pol *ApprovalPolicy, verifierFor ApproverVerifierFor) GateStanding {
	return EvaluateGateAfter(recs, s, pol, verifierFor, "")
}

// EvaluateGateAfter is EvaluateGate for a plan flow node's gate (s.ToolUseID a node key), which
// counts only the decisions recorded after the gate's request, the step ApprovalRequestStep(key)
// names, and whose result is the key's own record. With since "" it is EvaluateGate.
func EvaluateGateAfter(recs []Record, s ApprovalSubject, pol *ApprovalPolicy, verifierFor ApproverVerifierFor, since string) GateStanding {
	// A recorded result: the call ran, or was denied (a denial is recorded as a result).
	var singleDecided, singleApproved bool
	for _, r := range recs {
		switch {
		case r.Kind == StepValue && r.Name == s.ToolUseID && planNodeKey(s.ToolUseID):
			return GateStanding{Status: GateDecided, Ran: true} // a plan flow node's result is its key's own record
		case (r.Kind == StepToolResult || r.Kind == StepSagaFail) && r.ToolUseID == s.ToolUseID:
			st := GateStanding{Status: GateDecided, Ran: true, Denied: r.Kind == StepToolResult && r.IsError && string(r.Result) == `"tool call denied by human"`}
			// The tally the gate recorded, for a reader that shows how it was decided.
			byName := map[string]Record{}
			for _, x := range recs {
				if x.Kind == StepValue && (x.Name == ApprovalTallyStep(s.ToolUseID) || x.Name == ApprovalRetallyStep(s.ToolUseID) || x.Name == ApprovalSoDRetallyStep(s.ToolUseID)) {
					if _, dup := byName[x.Name]; !dup {
						byName[x.Name] = x
					}
				}
			}
			if rt, ok := RecordedTally(byName, s.ToolUseID); ok {
				if t, err := decodeTally(s.RunID, rt); err == nil {
					st.Tally, st.Recorded, st.Checks = t, true, recountFor(recs, s, t, pol, verifierFor)
				}
			}
			return st
		case since != "" && r.Name == since:
			// A flow gate honours a single decision only if it follows the request: one recorded
			// before it (node keys are predictable) was not made on the node's input, as flowGate reads it.
			singleDecided, singleApproved = false, false
		case r.Kind == StepApproval && r.ToolUseID == s.ToolUseID && r.Approver() == "":
			singleDecided, singleApproved = true, r.Approved // the last one, as the loop reads it
		}
	}
	byName := map[string]Record{}
	for _, r := range recs {
		if r.Kind == StepValue && (r.Name == ApprovalTallyStep(s.ToolUseID) || r.Name == ApprovalRetallyStep(s.ToolUseID) || r.Name == ApprovalSoDRetallyStep(s.ToolUseID)) {
			if _, dup := byName[r.Name]; !dup {
				byName[r.Name] = r
			}
		}
	}
	// A recorded denial is final, whatever the gate is now: a human's Approve(false) or a tally that
	// did not pass.
	if singleDecided && !singleApproved {
		return GateStanding{Status: GateDecided, Denied: true}
	}
	if rt, ok := RecordedTally(byName, s.ToolUseID); ok {
		t, err := decodeTally(s.RunID, rt)
		if err != nil {
			return GateStanding{Status: GateMisconfigured, Err: err}
		}
		if !t.Passed() {
			return GateStanding{Status: GateDecided, Denied: true, Tally: t, Recorded: true, Checks: recountFor(recs, s, t, pol, verifierFor)}
		}
	}
	switch {
	case pol == nil:
		return GateStanding{Status: GateUngated}
	case pol.single():
		return GateStanding{Status: GateSingle, SingleDecided: singleDecided, SingleApproved: singleApproved}
	}
	if err := gatePolicyError(pol, verifierFor); err != nil {
		return GateStanding{Status: GateMisconfigured, Err: err}
	}
	c, err := countGate(recs, s.RunID, s, pol, verifierFor, since)
	if err != nil {
		return GateStanding{Status: GateMisconfigured, Err: err}
	}
	if c.recorded {
		return GateStanding{Status: GateDecided, Tally: c.tally, Recorded: true, Checks: recountFor(recs, s, c.tally, pol, verifierFor)}
	}
	return GateStanding{Status: GateWaiting, Tally: c.tally, Checks: c.checks, Reason: c.tally.reapproval}
}

// recountFor classifies the decision records of a gate that has a recorded outcome: a recount under
// the policy and keys now, with a decision cleared that the recorded tally does not name (it did not
// count then), and one the recount cannot verify (a key rotated since) kept when the tally names it.
func recountFor(recs []Record, s ApprovalSubject, t ApprovalTally, pol *ApprovalPolicy, verifierFor ApproverVerifierFor) []DecisionCheck {
	if pol == nil || pol.single() || verifierFor == nil {
		return nil
	}
	_, checks := TallyApprovals(recs, s, *pol, verifierFor)
	for i := range checks {
		by := t.DeniedBy
		if checks[i].Approved {
			by = t.ApprovedBy
		}
		named := slices.Contains(by, checks[i].Approver)
		switch {
		case checks[i].Counted && !named:
			checks[i].Counted, checks[i].Reason = false, "not counted by the gate's recorded tally"
		case !checks[i].Counted && checks[i].Reason == ReasonNoKey && named:
			checks[i].Counted, checks[i].Reason = true, ""
		}
	}
	return checks
}

// GateReask returns why a gate asks again instead of acting on its recorded tally t, or "": t passed
// on decisions that predate DecisionFormat (PendingReapproval), or passed without excluding the
// requester while the policy now excludes it (PendingSoDReapproval). A reader that has no policy
// object, only the fact that the gate excludes the requester, asks with excludePrincipal.
func GateReask(t ApprovalTally, excludePrincipal bool) string {
	switch {
	case predatesFormat(t):
		return PendingReapproval
	case excludePrincipal && t.Passed() && !t.ExcludePrincipal:
		return PendingSoDReapproval
	}
	return ""
}
