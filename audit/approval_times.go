package audit

import (
	"math"
	"time"

	"github.com/bide-ai/bide/agent"
)

// DefaultDecisionTimeTolerance is how far two clocks may disagree before CheckDecisionTimes flags
// it: a decision's signed time running ahead of its recorded time, or a recorded time out of
// journal order or after the signed tree head that covers it.
const DefaultDecisionTimeTolerance = 5 * time.Minute

// DecisionTime is the two times of one counted approver decision, kept apart on purpose:
//
//   - DecidedAt is when the approver says they decided, by their own clock. It is inside the
//     signed decision (agent.ApprovalDecisionBytes), so it cannot be changed after signing, but it
//     is the approver's claim: their clock may be wrong.
//   - RecordedAt is when the decision was journaled, by the clock of the process that recorded it
//     (SubmitDecision's caller: its WithClock, else its context's clock, else its wall clock).
//     It is that submitter's claim, not a time bide vouches for: it is not signed, and an
//     identical resubmission keeps the first submitter's time. The journal's Merkle trail anchors
//     it, with the record, under the signed tree head, so it cannot change after the head is
//     signed, and the head's own time bounds it (FlagRecordedAfterHead).
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
// or not in canonical form, or when the two are too far apart for a time.Duration (about 292
// years; Sub would saturate). It is shown, not judged; the flags compare the times themselves.
func (d DecisionTime) Gap() (gap time.Duration, ok bool) {
	decided, err1 := agent.ParseTimestamp(d.DecidedAt)
	recorded, err2 := agent.ParseTimestamp(d.RecordedAt)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	gap = recorded.Sub(decided)
	if gap == math.MinInt64 || gap == math.MaxInt64 {
		return 0, false
	}
	return gap, true
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
	// FlagRecordedAtInvalid: the decision's recorded_at is missing or not a canonical time. bide
	// writes one with every decision, and it is not signed, so whoever wrote the journal changed
	// or dropped it; readers show it quoted, as text, and FlagDecidedAfterRecorded is not checked.
	FlagRecordedAtInvalid = "recorded_at is missing or not a canonical time (bide writes one with every decision; it is not signed), so it is shown only as quoted text"
	// FlagRecordedOutOfOrder: the decision's recorded_at is more than the tolerance before the
	// recorded_at of a decision journaled earlier, or before the run asked: the journal's order
	// and its times disagree, so a submitter's clock is wrong or the time was not the recording's.
	FlagRecordedOutOfOrder = "recorded_at is earlier than the journal order allows (before an earlier decision's recorded_at, or before the run asked): a submitter's clock is off, or the time was set by the submitter"
	// FlagRecordedAfterHead: the decision's recorded_at is more than the tolerance after the signed
	// tree head that covers its record was signed: the record existed by then, so the time is not
	// when it was recorded.
	FlagRecordedAfterHead = "recorded_at is later than the signed tree head that covers the decision: the record existed by then, so this time is not when it was recorded"
)

// TimeCheck is what CheckDecisionTimes compares decisions' times with: when the run asked
// (ApprovalVerdict.AskedAt), when the signed tree head covering the decisions was signed
// (ApprovalVerdict.HeadAt; "" skips that check), and how far clocks may disagree (Tolerance;
// negative is zero).
type TimeCheck struct {
	AskedAt   string
	HeadAt    string
	Tolerance time.Duration
}

// FlagDecisionTimes returns times with Flags recomputed under tolerance: FlagDecidedAfterRecorded
// when a decision's DecidedAt is more than tolerance after its RecordedAt (FlagRecordedAtInvalid
// in its place when RecordedAt is missing or not a canonical time), and
// FlagDecidedBeforeAsked when its DecidedAt is before askedAt, the time the run asked for approval
// (ApprovalVerdict.AskedAt), and FlagAskedUnknown in its place when askedAt is "" or not a
// canonical time. A negative tolerance is treated as zero. The input is not modified. No flag
// affects the count or the verdict.
func FlagDecisionTimes(times []DecisionTime, askedAt string, tolerance time.Duration) []DecisionTime {
	return CheckDecisionTimes(times, TimeCheck{AskedAt: askedAt, Tolerance: tolerance})
}

// CheckDecisionTimes returns times, in journal order, with Flags recomputed: FlagDecisionTimes'
// flags under c.AskedAt and c.Tolerance, and, for a canonical RecordedAt,
// FlagRecordedOutOfOrder when it is more than the tolerance before an earlier decision's
// RecordedAt or before c.AskedAt, and FlagRecordedAfterHead when it is more than the tolerance
// after c.HeadAt. The input is not modified. No flag affects the count or the verdict.
func CheckDecisionTimes(times []DecisionTime, c TimeCheck) []DecisionTime {
	tolerance := max(c.Tolerance, 0)
	asked, askedErr := agent.ParseTimestamp(c.AskedAt)
	head, headErr := agent.ParseTimestamp(c.HeadAt)
	latest := asked // the latest recorded time the journal order has reached
	if askedErr != nil {
		latest = time.Time{}
	}
	out := make([]DecisionTime, len(times))
	for i, d := range times {
		d.Flags = nil
		decided, err := agent.ParseTimestamp(d.DecidedAt)
		recorded, rerr := agent.ParseTimestamp(d.RecordedAt)
		switch {
		case rerr != nil:
			d.Flags = append(d.Flags, FlagRecordedAtInvalid)
		case err == nil && decided.After(recorded.Add(tolerance)):
			d.Flags = append(d.Flags, FlagDecidedAfterRecorded)
		}
		switch {
		case askedErr != nil:
			d.Flags = append(d.Flags, FlagAskedUnknown)
		case err == nil && decided.Before(asked):
			d.Flags = append(d.Flags, FlagDecidedBeforeAsked)
		}
		if rerr == nil {
			if !latest.IsZero() && recorded.Add(tolerance).Before(latest) {
				d.Flags = append(d.Flags, FlagRecordedOutOfOrder)
			}
			if recorded.After(latest) {
				latest = recorded
			}
			if headErr == nil && recorded.After(head.Add(tolerance)) {
				d.Flags = append(d.Flags, FlagRecordedAfterHead)
			}
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

// reaskedAt returns why and when the gate on toolUseID asked again, from the newest proven re-ask
// note among actions (agent.ApprovalReaskStep: the separation-of-duties one over the format one), a
// KindStep action under sth and runID that reads strictly; "" and "" when there is none.
func reaskedAt(actions []EvidenceAction, recs []agent.Record, toolUseID string, sth SignedTreeHead, runID string, logV Verifier) (reason, at string) {
	for _, name := range []string{agent.ApprovalReaskStep(toolUseID, true), agent.ApprovalReaskStep(toolUseID, false)} {
		for i, a := range actions {
			r := recs[i]
			if a.Kind != KindStep || r.Kind != agent.StepValue || r.Name != name {
				continue
			}
			if a.Bundle.Verify(logV) != nil || !sameTreeHead(a.Bundle.STH, sth) || a.Bundle.RunID != runID {
				continue
			}
			var w agent.ApprovalWait
			if err := UnmarshalStrict(r.Result, &w); err != nil || w.ToolUseID != toolUseID || w.Reask == "" {
				continue
			}
			if _, err := agent.ParseTimestamp(w.RecordedAt); err != nil {
				w.RecordedAt = ""
			}
			return w.Reask, w.RecordedAt
		}
	}
	return "", ""
}

// validTimestamp reports whether s is in agent.FormatTimestamp's one form.
func validTimestamp(s string) bool {
	_, err := agent.ParseTimestamp(s)
	return err == nil
}
