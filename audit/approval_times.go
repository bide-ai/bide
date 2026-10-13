package audit

import (
	"time"

	"github.com/bide-ai/bide/agent"
)

// DefaultDecisionTimeTolerance is how far a decision's signed time may run ahead of the time bide
// recorded it before FlagDecisionTimes flags the approver's clock as running ahead.
const DefaultDecisionTimeTolerance = 5 * time.Minute

// DecisionTime is the two times of one counted approver decision, kept apart on purpose:
//
//   - DecidedAt is when the approver says they decided, by their own clock. It is inside the
//     signed decision (agent.ApprovalDecisionBytes), so it cannot be changed after signing, but it
//     is the approver's claim: their clock may be wrong.
//   - RecordedAt is when bide journaled the decision, by bide's clock. It is not signed; the
//     journal's Merkle trail anchors it, with the record, under the signed tree head.
//
// Neither time affects the count (see agent.TallyApprovals). Flags are sanity checks for a person
// reading the evidence (FlagDecisionTimes); they are never a verdict and never fail verification.
type DecisionTime struct {
	Approver   string   `json:"approver"`
	Step       string   `json:"step"`
	DecidedAt  string   `json:"decided_at"`
	RecordedAt string   `json:"recorded_at,omitempty"`
	Flags      []string `json:"flags,omitempty"`
}

// Gap returns RecordedAt minus DecidedAt: how long after the approver's signed time bide recorded
// the decision (negative when the signed time is later). ok is false when either time is missing
// or not in canonical form. It is shown, not judged.
func (d DecisionTime) Gap() (gap time.Duration, ok bool) {
	decided, err1 := agent.ParseTimestamp(d.DecidedAt)
	recorded, err2 := agent.ParseTimestamp(d.RecordedAt)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return recorded.Sub(decided), true
}

// Sanity flags on a decision's times (DecisionTime.Flags). Each is a note for a person, not a
// finding against the gate.
const (
	// FlagDecidedAfterRecorded: the signed decision time is more than the tolerance after bide
	// recorded the decision, so the approver's clock was running ahead (a decision cannot be
	// recorded before it is made).
	FlagDecidedAfterRecorded = "decided_at is later than recorded_at by more than the tolerance: the approver's clock appears to run ahead"
	// FlagDecidedBeforeAsked: the signed decision time is before the run recorded that it paused
	// for approval. The gate orders decisions by their place in the journal, never by this time:
	// it reads only decisions on the exact call, and for a plan flow node only those recorded after
	// the gate's request (SubmitDecision refuses one recorded before it). So a decision with this
	// flag was recorded after the request; its signed time says it was made before the approver
	// was asked, which points at a clock that runs behind, or a decision signed in advance.
	FlagDecidedBeforeAsked = "decided_at is before the run asked for approval: the approver's clock appears to run behind, or the decision was signed in advance; the gate orders decisions by journal position, so this did not change the count"
	// FlagAskedUnknown: the evidence does not say when the run asked for approval (no request or
	// pause record with a time, as from an earlier version, or a pause record lost to a crash), so
	// FlagDecidedBeforeAsked could not be checked. It is a note, not a problem.
	FlagAskedUnknown = "when the run asked for approval is not available, so decided-before-asked was not checked"
)

// FlagDecisionTimes returns times with Flags recomputed under tolerance: FlagDecidedAfterRecorded
// when a decision's DecidedAt is more than tolerance after its RecordedAt, and
// FlagDecidedBeforeAsked when its DecidedAt is before askedAt, the time the run asked for approval
// (ApprovalVerdict.AskedAt), and FlagAskedUnknown in its place when askedAt is "" or not a
// canonical time. A negative tolerance is treated as zero. The input is not modified. No flag
// affects the count or the verdict.
func FlagDecisionTimes(times []DecisionTime, askedAt string, tolerance time.Duration) []DecisionTime {
	tolerance = max(tolerance, 0)
	asked, askedErr := agent.ParseTimestamp(askedAt)
	out := make([]DecisionTime, len(times))
	for i, d := range times {
		d.Flags = nil
		if gap, ok := d.Gap(); ok && -gap > tolerance {
			d.Flags = append(d.Flags, FlagDecidedAfterRecorded)
		}
		switch decided, err := agent.ParseTimestamp(d.DecidedAt); {
		case askedErr != nil:
			d.Flags = append(d.Flags, FlagAskedUnknown)
		case err == nil && decided.Before(asked):
			d.Flags = append(d.Flags, FlagDecidedBeforeAsked)
		}
		out[i] = d
	}
	return out
}

// askedAt returns when the run asked for approval on toolUseID's gate: for a gated plan flow node,
// the recorded_at of its request (request, already proven by the caller), which the ordering rule
// is anchored to; otherwise, or when the request has none, the recorded_at of the proven
// approval-pending record among actions (a KindStep action under sth and runID). It returns ""
// when the evidence holds neither with a time.
func askedAt(actions []EvidenceAction, recs []agent.Record, request agent.Record, flowNode bool, toolUseID string, sth SignedTreeHead, runID string, logV Verifier) string {
	if flowNode {
		var q agent.ApprovalRequest
		if UnmarshalStrict(request.Result, &q) == nil && q.ToolUseID == toolUseID {
			if _, err := agent.ParseTimestamp(q.RecordedAt); err == nil {
				return q.RecordedAt
			}
		}
	}
	name := agent.ApprovalPendingStep(toolUseID)
	for i, a := range actions {
		r := recs[i]
		if a.Kind != KindStep || r.Kind != agent.StepValue || r.Name != name {
			continue
		}
		if a.Bundle.Verify(logV) != nil || !sameTreeHead(a.Bundle.STH, sth) || a.Bundle.RunID != runID {
			continue
		}
		var w agent.ApprovalWait
		if err := UnmarshalStrict(r.Result, &w); err != nil || w.ToolUseID != toolUseID {
			continue
		}
		if _, err := agent.ParseTimestamp(w.RecordedAt); err != nil {
			continue
		}
		return w.RecordedAt
	}
	return ""
}
