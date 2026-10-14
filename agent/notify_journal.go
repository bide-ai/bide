package agent

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"time"
)

// JournalNotifyEvent is an event the journal of a run records that needs a person: a gate the run
// paused at, an effect whose outcome is unknown, a run that aborted. audit.VerifyNotifications
// compares these to a notification trail, so an event with no trail record is found.
type JournalNotifyEvent struct {
	// Key is the event key a notifier gives the event (ApprovalKey, HaltedKey, AbortedKey).
	Key string
	// Kind is NotificationApprovalPending, NotificationOutcomeUnknown or NotificationRunAborted.
	Kind NotificationKind
	// RunID is the root run, whose notification trail holds the event (NotifyTrailName).
	RunID string
	// At is when the journal says the event began (the pause, the attempt, the abort); zero if the
	// journal does not say.
	At time.Time
	// Current reports whether the journal still shows the event unresolved: a gate with no
	// decision, an attempt with no result. An event that is not current was resolved since.
	Current bool
}

// JournalNotifyEvents returns the events recs, the history of runID, records that need a person,
// in journal order (gates, then halts in key order, then the abort). It reads the journal as the
// notifier's sweep does and with the same keys, so a notifier that saw the event wrote a trail
// record under the same Key. It differs from the sweep in what it keeps: a gate that was decided
// is returned with Current false, because the person was owed a notification while it was open,
// and a halt is returned whatever its age (the caller applies the age the notifier waits before
// reporting one, WithNotifyHaltAge).
//
// What it cannot see: a halt resolved by a later result leaves no attempt marker without a result,
// so the journal no longer shows it; and recovery_failing and overdue are not journal facts.
func JournalNotifyEvents(runID string, recs []Record) []JournalNotifyEvent {
	root := treeRootID(runID)
	have := make(map[string]bool, len(recs))
	for _, r := range recs {
		have[r.Name] = true
	}
	var out []JournalNotifyEvent
	for _, r := range recs {
		if r.Kind != StepValue || !strings.HasPrefix(r.Name, "approval-pending:") {
			continue
		}
		var w ApprovalWait
		if err := json.Unmarshal(r.Result, &w); err != nil || w.ToolUseID == "" {
			continue
		}
		at, _ := time.Parse(time.RFC3339Nano, w.RecordedAt)
		out = append(out, JournalNotifyEvent{
			Key: ApprovalKey(runID, w.ToolUseID, 0), Kind: NotificationApprovalPending, RunID: root, At: at,
			Current: !slices.ContainsFunc(gateDecidedNames(w.ToolUseID), func(n string) bool { return have[n] }),
		})
	}
	live := liveAttempts(recs)
	for _, base := range slices.Sorted(maps.Keys(live)) {
		m := live[base]
		var kind OpKind
		var id, result string
		switch {
		case strings.HasPrefix(base, "attempt:tool:"):
			kind, id = OpTool, m.ToolUseID
			result = ToolResultStep(m.ToolUseID)
		case strings.HasPrefix(base, "attempt:step:"):
			kind, id = OpStep, strings.TrimPrefix(base, "attempt:step:")
			result = id
		default:
			continue
		}
		at := markerTime(m.AttemptedAt)
		if have[result] || at.IsZero() {
			continue
		}
		out = append(out, JournalNotifyEvent{
			Key: HaltedKey(runID, kind, id, at.UnixMilli()), Kind: NotificationOutcomeUnknown, RunID: root, At: at, Current: true,
		})
	}
	if !IsSubRun(runID) {
		for _, r := range recs {
			if r.Name == runAbortedStep {
				at, _ := abortedAt(r)
				out = append(out, JournalNotifyEvent{Key: AbortedKey(runID), Kind: NotificationRunAborted, RunID: root, At: at, Current: true})
				break
			}
		}
	}
	return out
}
