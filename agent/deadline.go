package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bide-ai/bide/internal/strictjson"
)

// This file holds approval deadlines (ApprovalPolicy.Timeout): the record that fixes a gate's
// deadline, and the reading of it the gate, Approve and SubmitDecision share. See
// docs/design/approval-deadlines.md.

// ErrApprovalExpired is returned (wrapped) when an approval gate's deadline has passed: by Approve
// and SubmitDecision for a decision on such a gate (also ErrConfig: nothing is recorded), and by a
// plan flow for a node whose gate expired (also plan.ErrApprovalDenied). An agent's model reads an
// expired call as denied (ExpiredResult).
var ErrApprovalExpired = errors.New("approval expired")

// ExpiredResult is the result an agent's model reads for a call whose approval gate expired: an
// error result, as for a denial.
const ExpiredResult = `"tool call denied: the approval gate expired"`

// expiredDecision is the Result of the record a one-decision gate (SingleApproval) writes as its
// decision when its deadline passes: a denial that says why.
const expiredDecision = `"expired"`

// ApprovalDeadlineStep is the journal name of the record that fixes the deadline of the gate on
// toolUseID (a tool call's ID, or a plan flow node's key).
func ApprovalDeadlineStep(toolUseID string) string { return "approval-deadline:" + encodeID(toolUseID) }

// ApprovalDeadline is what the record named ApprovalDeadlineStep holds. The gate writes it once
// (insert-once), the first time it waits under a policy with a Timeout, and reads the deadline from
// it from then on. RequestedAt is when the request was recorded (a plan flow node's
// approval-request, or an agent call's approval-pending note, by their recorded_at, or the drive's
// clock when neither has one), and ExpiresAt is RequestedAt plus the timeout, both in
// FormatTimestamp's form. TimeoutMS is the timeout it was opened with.
type ApprovalDeadline struct {
	ToolUseID   string `json:"tool_use_id"`
	RequestedAt string `json:"requested_at"`
	ExpiresAt   string `json:"expires_at"`
	TimeoutMS   int64  `json:"timeout_ms"`
}

// decodeDeadline reads r as the deadline record of toolUseID, strictly; ok is false for a record
// that is not one (a hand-written row the gate does not accept as a deadline).
func decodeDeadline(r Record, toolUseID string) (ApprovalDeadline, time.Time, bool) {
	if r.Kind != StepValue || r.Name != ApprovalDeadlineStep(toolUseID) {
		return ApprovalDeadline{}, time.Time{}, false
	}
	var d ApprovalDeadline
	if err := strictjson.Unmarshal(r.Result, &d, nil); err != nil || d.ToolUseID != toolUseID {
		return ApprovalDeadline{}, time.Time{}, false
	}
	at, err := ParseTimestamp(d.ExpiresAt)
	if err != nil {
		return ApprovalDeadline{}, time.Time{}, false
	}
	return d, at, true
}

// RecordedDeadline returns the deadline the gate on toolUseID recorded in recs (a run's records),
// and its parsed ExpiresAt, or ok=false when the gate has none.
func RecordedDeadline(recs []Record, toolUseID string) (d ApprovalDeadline, expiresAt time.Time, ok bool) {
	for _, r := range recs {
		if r.Name == ApprovalDeadlineStep(toolUseID) {
			return decodeDeadline(r, toolUseID)
		}
	}
	return ApprovalDeadline{}, time.Time{}, false
}

// lateFor reports whether a decision recorded at recordedAt counts as late under the deadline
// expiresAt: recorded after it, or with no readable recorded time (it cannot be shown on time).
func lateFor(recordedAt string, expiresAt time.Time) bool {
	t, err := ParseTimestamp(recordedAt)
	return err != nil || t.After(expiresAt)
}

// requestStamp is the recorded time of the request of the gate on toolUseID in recs: the plan flow
// node's approval-request, or the agent call's approval-pending note, or "" when neither records one.
func requestStamp(recs []Record, toolUseID string) string {
	for _, r := range recs {
		switch r.Name {
		case ApprovalRequestStep(toolUseID):
			if q, ok := decodeApprovalRequest(r, toolUseID); ok && q.RecordedAt != "" {
				return q.RecordedAt
			}
		case ApprovalPendingStep(toolUseID):
			var w ApprovalWait
			if json.Unmarshal(r.Result, &w) == nil && w.RecordedAt != "" {
				return w.RecordedAt
			}
		}
	}
	return ""
}

// openDeadline returns the deadline of the gate on toolUseID: the recorded one, or, for a policy
// with a Timeout and none recorded, the one it records now (insert-once: another drive's, recorded
// first, is the one returned). recs are the run's records as the caller read them, for the recorded
// deadline and the request's time. ok is false for a gate with no deadline.
func openDeadline(ctx context.Context, d *Journal, runID, toolUseID string, pol *ApprovalPolicy, recs []Record) (time.Time, bool, error) {
	if _, at, ok := RecordedDeadline(recs, toolUseID); ok {
		return at, true, nil
	}
	if pol == nil || pol.Timeout <= 0 {
		return time.Time{}, false, nil
	}
	now := clockFrom(ctx)()
	requested := now
	if t, err := ParseTimestamp(requestStamp(recs, toolUseID)); err == nil {
		requested = t
	}
	rec := ApprovalDeadline{ToolUseID: toolUseID, RequestedAt: FormatTimestamp(requested), ExpiresAt: FormatTimestamp(requested.Add(pol.Timeout)), TimeoutMS: pol.Timeout.Milliseconds()}
	if _, err := ParseTimestamp(rec.ExpiresAt); err != nil {
		return time.Time{}, false, fmt.Errorf("approval gate on %s: the deadline %s has no canonical form (years 0001 to 9999): %w", toolUseID, rec.ExpiresAt, ErrConfig)
	}
	b, err := marshalJournal(rec)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("encode %s: %w (%w)", ApprovalDeadlineStep(toolUseID), err, ErrConfig)
	}
	got, _, err := d.putNew(ctx, runID, ApprovalDeadlineStep(toolUseID), Record{Kind: StepValue, Result: b})
	if err != nil {
		return time.Time{}, false, err
	}
	_, at, ok := decodeDeadline(got, toolUseID)
	if !ok {
		return time.Time{}, false, fmt.Errorf("run %s: the recorded %s does not read as a deadline: %w", runID, ApprovalDeadlineStep(toolUseID), ErrStorage)
	}
	return at, true, nil
}

// pastDeadline reports whether now has reached expiresAt.
func pastDeadline(now time.Time, expiresAt time.Time) bool { return !now.Before(expiresAt) }

// checkNotExpired refuses (ErrApprovalExpired, ErrConfig) a decision on the gate of toolUseID whose
// recorded deadline the clock has reached: it could not count. One point read of the deadline.
func checkNotExpired(ctx context.Context, d *Journal, op, runID, toolUseID string, now func() time.Time) error {
	r, ok, err := d.Get(ctx, runID, ApprovalDeadlineStep(toolUseID))
	if err != nil {
		return fmt.Errorf("%s: read %s (run %s): %w (%w)", op, ApprovalDeadlineStep(toolUseID), runID, err, ErrStorage)
	}
	if !ok {
		return nil
	}
	dl, at, ok := decodeDeadline(r, toolUseID)
	if !ok || !pastDeadline(now(), at) {
		return nil
	}
	return fmt.Errorf("%s: the approval gate on %s in run %s expired at %s, so a decision on it cannot count and was not recorded: %w (%w)", op, toolUseID, runID, dl.ExpiresAt, ErrApprovalExpired, ErrConfig)
}

// singleExpired reports whether r, a one-decision gate's decision record, is the gate's expiry: the
// record the gate wrote when its deadline passed, or a decision recorded after the deadline (it
// took the decision's name, so the gate reads it as the expiry).
func singleExpired(r Record, expiresAt time.Time, hasDeadline bool) bool {
	if string(r.Result) == expiredDecision {
		return true
	}
	return hasDeadline && lateFor(r.RecordedAt(), expiresAt)
}
