// cancel.go holds Cancel (D1) and Status (D8): the two calls that act on a run from outside its
// drives (docs/design/api-v1.md, item 1).

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// RunState is a run's state as Status reads it from the run's journal.
type RunState string

const (
	// RunNotStarted is a run with no run:start record.
	RunNotStarted RunState = "not_started"
	// RunStarted is a run with a run:start record and no end marker: running, paused, halted, or
	// stopped at a limit. Pauses are not journaled, so a paused run is RunStarted.
	RunStarted RunState = "started"
	// RunCompleted is a run whose first end marker is run:complete.
	RunCompleted RunState = "completed"
	// RunAborted is a saga whose first end marker is run:aborted: a step failed and its rollback
	// finished.
	RunAborted RunState = "aborted"
	// RunCancelled is a run whose first end marker is run:cancelled.
	RunCancelled RunState = "cancelled"
	// RunEnded is a run whose first end marker was redacted (RunStatus.EndRedacted): it is over, but
	// how it ended (completed, aborted or cancelled) is not attested, because a tombstone's name is
	// outside the leaf hash, so Status never reads it from the name. bide audit redactions reports
	// the kind the signed redaction trail recorded.
	RunEnded RunState = "ended"
)

// RunStatus is a run's state, as Status reads it.
type RunStatus struct {
	// StartRedacted is set when the run's run:start was redacted: the run started only by the
	// record's name, which a tombstone carries but nothing outside the database attests (see
	// Record.Redacted).
	StartRedacted bool
	// EndRedacted is set when the run's first end marker was redacted: the run is over, as the
	// marker's name says, and the text the marker recorded (Terminal) and its time (EndedAt) are
	// gone with its content (see Record.Redacted).
	EndRedacted bool
	State       RunState
	// Terminal is the text the run's first end marker records: the reason given to Cancel for a
	// cancelled run, the failure that aborted a saga; empty otherwise.
	Terminal string
	// Records is the number of records the run's journal held when Status read it, the journal
	// header included.
	Records int
	// EndedAt is when the run's first end marker was written (its Record.RecordedAt; for a
	// run:aborted that has none, the aborted_at it records), in FormatTimestamp's form, by the
	// clock of the process that wrote it. It is "" for a run with no end marker, and for one whose
	// end marker records no time (written before they did): never a time Status supplies. It is
	// informational: nothing that drives, resumes or recovers a run reads it.
	EndedAt string
}

var errP14NotBuilt = errors.New("p14: not built yet")

// CancelOption configures Cancel: WithClock, the clock that stamps the record's RecordedAt.
type CancelOption interface {
	applyCancel(*cancelConfig) error
}

type cancelConfig struct{ now func() time.Time }

func (f clockOption) applyCancel(c *cancelConfig) error {
	if err := f.check(); err != nil {
		return err
	}
	c.now = f
	return nil
}

// cancelReason is the record run:cancelled and run:cancel-requested hold.
type cancelReason struct {
	Reason string `json:"reason"`
}

// protocol:lifecycle begin CGet CIns CRead CReq
// protocol:sessions begin Cancel

// Cancel cancels runID's run (D1), recording reason. It takes no lease: it writes one record, and
// the run's drives act on it.
//
//   - A run that is over is not cancelled: Cancel reads the run's end markers first, and a run
//     whose first end marker (in journal order) is run:complete or run:aborted is ErrRunEnded,
//     with nothing written. A run already cancelled is cancelled: Cancel returns nil and writes
//     nothing, so a Cancel retried after a lost reply succeeds.
//   - A run with no run:start record (one never driven, or a mistyped run ID) is ErrNotStarted,
//     with nothing written.
//   - A run that is not a saga gets the end marker run:cancelled {reason}. Recovery no longer
//     lists it, and a drive checks for the marker when it starts, at every turn boundary, and once
//     it has won a side effect's attempt claim, before calling the tool: it then starts nothing
//     more and returns ErrRunCancelled. A call already past that check finishes and records its
//     result. run:cancelled and the run's completion are different keys, so both can land; the
//     first in journal order is the run's end for every reader, and Cancel reads the markers back
//     after its write: if the run completed first, Cancel returns ErrRunEnded.
//   - A saga (a run started with WithSaga) gets a rollback request, run:cancel-requested {reason},
//     which is not an end marker: recovery still lists the run, and the drive that sees the
//     request (at the same checks) starts nothing more, rolls the run back, and then writes
//     run:cancelled. Cancel returns once the request is durable; until the rollback finishes,
//     Status reports the saga RunStarted. A saga whose run:complete lands before the request is
//     seen is complete.
//
// What "starts nothing more" covers, precisely: a side-effect call's claim won after the check saw
// the marker never fires (its attempt is recorded as not started). A retry-safe call (ReadOnly or
// Idempotent: no claim) is not checked one by one: a retry-safe call already dispatched in the
// current turn may still run, and the run stops at its next turn boundary or claim. A sub-run (a
// sub-agent's, a SubRunFor run, a delegation) reads its tree root's cancellation too, when it
// opens, at its turn boundaries and after every won claim, so a Cancel of the root stops the whole
// tree; it reads the root's markers from the root's store, also when its own agent journals to
// another. A plan flow's run checks it when the flow opens, at every node, and after every won claim.
//
// Cancel's reads are not one atomic step: a drive can write run:complete between Cancel's first
// read and its write. The read-back after the write settles it: the first end marker in journal
// order is the run's end, and Cancel reports ErrRunEnded if it is not run:cancelled.
//
// Cancel accepts any run ID but the empty one: a sub-run's or a session turn's run is a run too
// (see Session.Send for what a cancelled turn does to its session).
//
// The run:cancelled record (and a saga's run:cancel-requested) notes when it was written
// (Record.RecordedAt), by WithClock's clock, else the clock ctx carries, else the wall clock. It is
// informational, for people and auditors: no drive, recovery pass or check reads it, and the first
// end marker in journal order is the run's end whatever the times say.
func Cancel(ctx context.Context, j *Journal, runID, reason string, opts ...CancelOption) error {
	if j == nil {
		return fmt.Errorf("Cancel: nil journal: %w", ErrConfig)
	}
	if runID == "" {
		return fmt.Errorf("Cancel: empty runID: %w", ErrConfig)
	}
	cfg := cancelConfig{now: clockFrom(ctx)}
	if err := applyOptions("Cancel", &cfg, opts, CancelOption.applyCancel); err != nil {
		return err
	}
	// CGet: a run that is over is refused (its first end marker is the run's end).
	var first *endMarker
	for _, name := range endOfRunMarkers {
		e, ok, err := j.getEntry(ctx, runID, name)
		if err != nil {
			return err
		}
		if ok && (first == nil || e.Seq < first.seq) {
			r, err := decodeStored(runID, name, e.Data)
			if err != nil {
				return err
			}
			first = &endMarker{name: endKind(name, r), rec: r, seq: e.Seq}
		}
	}
	if first != nil {
		return cancelVerdict(runID, first.name)
	}
	start, ok, err := RecordedStart(ctx, j, runID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("Cancel run %s: %w", runID, ErrNotStarted)
	}
	b, err := marshalJournal(cancelReason{Reason: reason})
	if err != nil {
		return fmt.Errorf("Cancel run %s: encode the reason: %w (%w)", runID, err, ErrConfig)
	}
	if start.Saga {
		// CReq: a saga's rollback request, not an end marker.
		if _, err := j.put(ctx, runID, runCancelRequestedStep, Record{Kind: StepValue, Result: b, ApproverSignature: recordedStamp(cfg.now)}); err != nil {
			return err
		}
		return nil
	}
	// CIns, CRead: run:cancelled, then the end markers read back; the first is the run's end.
	end, err := writeEnd(ctx, j, runID, runCancelledStep, Record{Kind: StepValue, Result: b, ApproverSignature: recordedStamp(cfg.now)}, endOthers(runCancelledStep, false))
	if err != nil {
		return err
	}
	return cancelVerdict(runID, end.name)
}

// cancelVerdict is what Cancel reports for a run whose first end marker is first.
func cancelVerdict(runID, first string) error {
	if first == runCancelledStep {
		return nil
	}
	if first == "" {
		return fmt.Errorf("Cancel run %s: its end marker was redacted, so how it ended is not attested, and the run is over: %w", runID, ErrRunEnded)
	}
	return fmt.Errorf("Cancel run %s: its first end marker is %s: %w", runID, first, ErrRunEnded)
}

// protocol:sessions end
// protocol:lifecycle end

// protocol:lifecycle begin SPick SStart SGet

// Status reads runID's state from its journal (D8), with one Load: a prefix of the journal, so
// the state is the run's at one instant. A run with no run:start record is RunNotStarted. The
// first end marker in journal order is the run's end (RunCompleted, RunAborted, RunCancelled),
// with the text it records in Terminal. A run with none is RunStarted, whether it is running,
// paused, halted, or stopped at a limit: pauses are not journaled. A saga whose cancellation's
// rollback has not finished (it holds run:cancel-requested and no end marker) is RunStarted.
//
// For a session turn's run, RunCompleted says the run finished, not that the session recorded the
// turn: a turn whose run completed is answered once its session records it (Session.History,
// Session.Turns), which the next send of its message does.
func Status(ctx context.Context, j *Journal, runID string) (RunStatus, error) {
	if j == nil {
		return RunStatus{}, fmt.Errorf("Status: nil journal: %w", ErrConfig)
	}
	var recs []Record
	started := false
	for r, err := range j.Records(ctx, runID) {
		if err != nil {
			return RunStatus{}, err
		}
		// A redacted run:start (Record.Redacted) keeps its name and its place, so the run still
		// started: its input and principal are what a redaction erases, not the fact.
		if (r.Kind == StepValue || r.Redacted) && r.Name == runStartStep {
			started = true
		}
		recs = append(recs, r)
	}
	st := RunStatus{State: RunNotStarted, Records: len(recs)}
	for _, r := range recs {
		if r.Redacted && r.Name == runStartStep {
			st.StartRedacted = true
		}
	}
	if !started {
		return st, nil
	}
	end, ok := firstEnd(recs)
	if !ok {
		st.State = RunStarted
		return st, nil
	}
	st.EndedAt = endedAt(end.rec)
	st.EndRedacted = end.rec.Redacted
	if end.rec.Redacted {
		st.State = RunEnded
		return st, nil
	}
	switch end.name {
	case runCompleteStep:
		st.State = RunCompleted
	case runAbortedStep:
		st.State, st.Terminal = RunAborted, endText(end.rec)
	default:
		st.State, st.Terminal = RunCancelled, endText(end.rec)
	}
	return st, nil
}

// protocol:lifecycle end

// abortValue is the value of run:aborted: why the saga aborted, and when (UTC, RFC 3339 with
// milliseconds, by the drive's clock, as RunStart.StartedAt). AbortedAt is informational: no drive,
// resume, recovery pass or at-most-once check reads it. A run:aborted an earlier version wrote is
// the reason alone, as a JSON string, and loads and behaves the same. Its JSON is also a
// cancelReason, so a reader of the reason (endText) reads both.
type abortValue struct {
	Reason    string `json:"reason"`
	AbortedAt string `json:"aborted_at,omitempty"`
}

// abortedAt is when the run:aborted record r says the run aborted, and false when it says none (one
// an earlier version wrote, or a value that is not an abortValue).
func abortedAt(r Record) (time.Time, bool) {
	var v abortValue
	if json.Unmarshal(r.Result, &v) != nil || v.AbortedAt == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, v.AbortedAt)
	return t, err == nil
}

// endedAt is the time the end marker r says it was written: its recorded_at, else (when that is
// absent or not in FormatTimestamp's form) a run:aborted's valid aborted_at, else "".
func endedAt(r Record) string {
	if s := r.RecordedAt(); canonicalTimestamp(s) {
		return s
	}
	var v abortValue
	if r.Name == runAbortedStep && json.Unmarshal(r.Result, &v) == nil && canonicalTimestamp(v.AbortedAt) {
		return v.AbortedAt
	}
	return ""
}

// endText is the text an end marker records: a cancellation's reason, or a saga's failure text.
func endText(r Record) string {
	var c cancelReason
	if json.Unmarshal(r.Result, &c) == nil {
		return c.Reason
	}
	var s string
	if json.Unmarshal(r.Result, &s) == nil {
		return s
	}
	return ""
}
