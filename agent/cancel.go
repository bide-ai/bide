// cancel.go holds Cancel (D1) and Status (D8): the two calls that act on a run from outside its
// drives (docs/design/api-v1.md, item 1).

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
)

// RunStatus is a run's state, as Status reads it.
type RunStatus struct {
	State RunState
	// Terminal is the text the run's first end marker records: the reason given to Cancel for a
	// cancelled run, the failure that aborted a saga; empty otherwise.
	Terminal string
	// Records is the number of records the run's journal held when Status read it, the journal
	// header included.
	Records int
}

var errP14NotBuilt = errors.New("p14: not built yet")

// cancelReason is the record run:cancelled and run:cancel-requested hold.
type cancelReason struct {
	Reason string `json:"reason"`
}

// protocol:lifecycle begin CGet CIns CRead CReq

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
// Cancel accepts any run ID but the empty one: a sub-run's or a session turn's run is a run too
// (see Session.SendMessage for what a cancelled turn does to its session).
func Cancel(ctx context.Context, j *Journal, runID, reason string) error {
	if j == nil {
		return fmt.Errorf("Cancel: nil journal: %w", ErrConfig)
	}
	if runID == "" {
		return fmt.Errorf("Cancel: empty runID: %w", ErrConfig)
	}
	// CGet: a run that is over is refused (its first end marker is the run's end).
	var first *endMarker
	for _, name := range endOfRunMarkers {
		e, ok, err := j.getEntry(ctx, runID, name)
		if err != nil {
			return err
		}
		if ok && (first == nil || e.Seq < first.seq) {
			first = &endMarker{name: name, seq: e.Seq}
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
		if _, err := j.put(ctx, runID, runCancelRequestedStep, Record{Kind: StepValue, Result: b}); err != nil {
			return err
		}
		return nil
	}
	// CIns, CRead: run:cancelled, then the end markers read back; the first is the run's end.
	end, err := writeEnd(ctx, j, runID, runCancelledStep, Record{Kind: StepValue, Result: b}, endOthers(runCancelledStep, false))
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
	return fmt.Errorf("Cancel run %s: its first end marker is %s: %w", runID, first, ErrRunEnded)
}

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
		if r.Kind == StepValue && r.Name == runStartStep {
			started = true
		}
		recs = append(recs, r)
	}
	st := RunStatus{State: RunNotStarted, Records: len(recs)}
	if !started {
		return st, nil
	}
	end, ok := firstEnd(recs)
	if !ok {
		st.State = RunStarted
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
