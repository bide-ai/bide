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
	// Approvers is the gate's approvers, in policy order; nil for a 1-of-1 gate and for other kinds.
	// A notifier routes an approval by them (NotifyRouting.Receivers).
	Approvers []string
	// ResolvedAt is, for an event that is not current, when what resolved it was journaled: a
	// gate's newest decision (signed or a single approver's) or the result agent.ResolveHalt
	// injected for a halt, by the recorded_at of that record (the writing process's clock, not
	// signed), so an all-clear can be judged late. Zero when the event is current, or when the
	// record that resolved it holds no time (written before records carried one, or a result the
	// halted driver itself recorded).
	ResolvedAt time.Time
}

// JournalNotifyEvents returns the events recs, the history of runID, records that need a person,
// in journal order (gates, then halts in key order, then the abort). It reads the journal as the
// notifier's sweep does and with the same keys, so a notifier that saw the event wrote a trail
// record under the same Key. It differs from the sweep in what it keeps: a gate that was decided
// is returned with Current false, because the person was owed a notification while it was open,
// and a halt is returned whatever its age (the caller applies the age the notifier waits before
// reporting one, WithNotifyHaltAge).
//
// A halt resolved by agent.ResolveHalt is returned with Current false and ResolvedAt the time its
// injected result records; a halt a later drive resolved by running the call is not a halt the
// journal shows. recovery_failing and overdue are not journal facts.
func JournalNotifyEvents(runID string, recs []Record) []JournalNotifyEvent {
	root := runStartRoot(runID, recs)
	have := make(map[string]bool, len(recs))
	for _, r := range recs {
		have[r.Name] = true
	}
	byName := valuesByName(recs)
	var out []JournalNotifyEvent
	for i, r := range recs {
		if r.Kind != StepValue || !strings.HasPrefix(r.Name, "approval-pending:") {
			continue
		}
		var w ApprovalWait
		if err := json.Unmarshal(r.Result, &w); err != nil || w.ToolUseID == "" {
			continue
		}
		at, _ := time.Parse(time.RFC3339Nano, w.RecordedAt)
		ev := JournalNotifyEvent{
			Key: ApprovalKey(runID, w.ToolUseID, 0), Kind: NotificationApprovalPending, RunID: root, At: at,
			Current: !slices.ContainsFunc(gateDecidedNamesIn(recs, w.ToolUseID), func(n string) bool { return have[n] }),
		}
		first := 0
		if w.Reask != "" || r.Name != ApprovalPendingStep(w.ToolUseID) {
			// A re-ask note is an ask of its own, owed a notification of its own; one the journal does
			// not prove (ValidReaskNote) is not an event.
			if !ValidReaskNote(byName, w.ToolUseID, r.Name, w) {
				continue
			}
			gen, _ := IdentReaskGen(w.ToolUseID, r.Name)
			ev.Key = reaskKeyFor(runID, w.ToolUseID, w.Reask, gen)
			ev.Current = !slices.ContainsFunc(reaskDecidedNames(w.ToolUseID, w.Reask, gen), func(n string) bool { return have[n] })
			first = i + 1 // the decisions that resolve it come after the note
		}
		if !w.Single {
			ev.Approvers = slices.Clone(w.Approvers)
		}
		if !ev.Current {
			for _, d := range recs[first:] {
				// A signed m-of-n decision or a single approver's (Approve): either resolves the gate.
				if d.Kind != StepApproval || d.ToolUseID != w.ToolUseID {
					continue
				}
				if t, err := ParseTimestamp(d.RecordedAt()); err == nil && t.After(ev.ResolvedAt) {
					ev.ResolvedAt = t
				}
			}
		}
		out = append(out, ev)
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
		if at.IsZero() {
			continue
		}
		if have[result] {
			// Only a result ResolveHalt injected shows a halt that was resolved: any other result
			// is a call that ran to its end.
			for _, r := range recs {
				if r.Name == result && r.Reconciled {
					t, _ := ParseTimestamp(r.RecordedAt())
					out = append(out, JournalNotifyEvent{
						Key: HaltedKey(runID, kind, id, at.UnixMilli()), Kind: NotificationOutcomeUnknown, RunID: root, At: at, ResolvedAt: t,
					})
					break
				}
			}
			continue
		}
		out = append(out, JournalNotifyEvent{
			Key: HaltedKey(runID, kind, id, at.UnixMilli()), Kind: NotificationOutcomeUnknown, RunID: root, At: at, Current: true,
		})
	}
	if root == runID {
		for _, r := range recs {
			if r.Name == runAbortedStep && !r.Redacted { // an erased marker does not attest an abort
				at, _ := abortedAt(r)
				out = append(out, JournalNotifyEvent{Key: AbortedKey(runID), Kind: NotificationRunAborted, RunID: root, At: at, Current: true})
				break
			}
		}
	}
	return out
}

// valuesByName is the first StepValue record of each name in recs.
func valuesByName(recs []Record) map[string]Record {
	m := make(map[string]Record, len(recs))
	for _, r := range recs {
		if r.Kind == StepValue {
			if _, dup := m[r.Name]; !dup {
				m[r.Name] = r
			}
		}
	}
	return m
}
