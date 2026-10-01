// endmarker.go holds the rule every reader and writer of a run's end markers follows (D1, model 10's
// L3): the first end marker in journal order (run:complete, run:aborted or run:cancelled) is the
// run's end. The keys are distinct, so two can land (A1 is per key); a writer reads the markers back
// after its own write and reports the first.

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
)

// endMarker is one end marker of a run, as read: its key, its record, and its position.
type endMarker struct {
	name string
	rec  Record
	seq  int64 // its Seq (a Journal read), or its index in a History (any other Durable)
}

// firstEnd returns the first end marker in recs, which are in journal order.
func firstEnd(recs []Record) (endMarker, bool) {
	for i, r := range recs {
		if r.Kind == StepValue && slices.Contains(endOfRunMarkers, r.Name) {
			return endMarker{name: r.Name, rec: r, seq: int64(i)}, true
		}
	}
	return endMarker{}, false
}

// putEntry records rec as the step name of runID unless it is recorded (first writer wins), and
// returns the record the journal holds with its Seq.
func (j *Journal) putEntry(ctx context.Context, runID, name string, rec Record) (Record, int64, error) {
	if err := j.ensureHeader(ctx, runID); err != nil {
		return Record{}, 0, err
	}
	data, err := JournalEntry(name, rec)
	if err != nil {
		return Record{}, 0, fmt.Errorf("encode step %q: %w (%w)", name, err, ErrStorage)
	}
	e, _, err := j.store.Insert(ctx, runID, name, data)
	if err != nil {
		return Record{}, 0, storageErr(fmt.Sprintf("record step %q of run %s", name, runID), err)
	}
	got, err := decodeStored(runID, name, e.Data)
	return got, e.Seq, err
}

// writeEnd writes the end marker name of runID (first writer wins) and returns the run's end as
// the writer must report it: the first end marker in journal order, read back after the write.
// others are the end markers that can precede this one in the run (an end marker the run cannot
// hold is not read). Over a Journal the read-back is one Get per name in others: the writer's own
// marker is visible, so by A2 every marker before it is too, and the lowest Seq is the first. Over
// another Durable it is one History.
func writeEnd(ctx context.Context, d Durable, runID, name string, rec Record, others []string) (endMarker, error) {
	j := journalOf(d)
	if j == nil {
		if _, err := putRecord(ctx, d, runID, name, rec); err != nil {
			return endMarker{}, err
		}
		recs, err := d.History(ctx, runID)
		if err != nil {
			return endMarker{}, fmt.Errorf("load history %s: %w (%w)", runID, err, ErrStorage)
		}
		first, _ := firstEnd(recs)
		return first, nil
	}
	got, seq, err := j.putEntry(ctx, runID, name, rec)
	if err != nil {
		return endMarker{}, err
	}
	first := endMarker{name: name, rec: got, seq: seq}
	for _, o := range others {
		e, ok, err := j.getEntry(ctx, runID, o)
		if err != nil {
			return endMarker{}, err
		}
		if !ok || e.Seq > first.seq {
			continue
		}
		r, err := decodeStored(runID, o, e.Data)
		if err != nil {
			return endMarker{}, err
		}
		first = endMarker{name: o, rec: r, seq: e.Seq}
	}
	return first, nil
}

// endOthers returns the end markers other than own that a run can hold beside it: run:aborted only
// in a saga, since only a saga's rollback writes it.
func endOthers(own string, saga bool) []string {
	out := make([]string, 0, 2)
	for _, m := range endOfRunMarkers {
		if m != own && (saga || m != runAbortedStep) {
			out = append(out, m)
		}
	}
	return out
}

// endedErr is what a drive returns for a run whose end is end and is not run:complete:
// ErrRunCancelled for a cancelled run; for a saga whose rollback finished, the ErrConfig a drive
// that is not a saga's gets (a saga's drive rolls it back again, memoized, and returns
// *SagaAborted).
func endedErr(runID string, end endMarker) error {
	if end.name == runCancelledStep {
		return fmt.Errorf("run %s: %w", runID, ErrRunCancelled)
	}
	return fmt.Errorf("run %s was aborted: it is a saga whose rollback finished; drive it as a saga (RunSaga, WithSaga): %w", runID, ErrConfig)
}

// endVerdict reads runID's journal and returns its end as a drive reports it: the recorded answer
// of a completed run, or endedErr's error. A run with no end marker is reported cancelled: only a
// drive that read run:cancelled asks.
func (a *Agent) endVerdict(ctx context.Context, runID string) (Message, error) {
	recs, err := a.store.History(ctx, runID)
	if err != nil {
		return Message{}, fmt.Errorf("load history %s: %w (%w)", runID, err, ErrStorage)
	}
	end, ok := firstEnd(recs)
	switch {
	case !ok:
		return Message{}, fmt.Errorf("run %s: %w", runID, ErrRunCancelled)
	case end.name == runCompleteStep:
		msg, _ := completedAnswer(recs)
		return msg, nil
	}
	return Message{}, endedErr(runID, end)
}

// cancelTrip signals, inside the loop, that a saga's drive saw its rollback request: the saga
// path rolls the run back and writes run:cancelled.
type cancelTrip struct{ reason string }

func (e *cancelTrip) Error() string { return "saga cancelled: " + e.reason }

// protocol:lifecycle begin DTurn DPost

// cancelSeen reads the run's cancellation marker (run:cancelled, or a saga's rollback request):
// one Get.
func (a *Agent) cancelSeen(ctx context.Context, runID string, p *runPlan) (bool, error) {
	_, ok, err := lookup(ctx, a.store, runID, p.cancelKey)
	return ok, err
}

// postClaim is a won claim's check before its call (rule 3, L2): if the run was cancelled, the
// attempt is recorded as not started, so it never fires, and stop is true.
//
//go:noinline
func (a *Agent) postClaim(ctx context.Context, runID string, p *runPlan, markerKey string, marker Record) (stop bool, err error) {
	seen, err := a.cancelSeen(ctx, runID, p)
	if err != nil {
		if nerr := recordNotStarted(ctx, a.store, runID, markerKey, marker); nerr != nil {
			err = fmt.Errorf("%w (%w)", err, nerr)
		}
		return false, err
	}
	if !seen {
		return false, nil
	}
	return true, recordNotStarted(ctx, a.store, runID, markerKey, marker)
}

// leaveCancelled ends a drive that found its run cancelled: a saga's rolls back (cancelTrip), any
// other reports the run's end (endVerdict). leave settles the drive's spend.
//
//go:noinline
func (a *Agent) leaveCancelled(ctx context.Context, runID string, p *runPlan, leave func(error) (Message, usageTotals, int, error), tot *usageTotals, turns int) (Message, usageTotals, int, error) {
	if p.saga {
		r, _, err := lookup(ctx, a.store, runID, runCancelRequestedStep)
		if err != nil {
			return leave(err)
		}
		return leave(&cancelTrip{reason: endText(r)})
	}
	msg, err := a.endVerdict(ctx, runID)
	if err != nil {
		return leave(err)
	}
	if _, _, _, err := leave(nil); err != nil {
		return Message{}, *tot, turns, err
	}
	return msg, *tot, turns, nil
}

// protocol:lifecycle end

// startInput is the input run:start in recs journals, or the zero message.
func startInput(recs []Record) Message {
	r, ok := recordNamed(recs, runStartStep)
	if !ok {
		return Message{}
	}
	var s RunStart
	_ = json.Unmarshal(r.Result, &s)
	return s.Input
}

// answerRecorded reports whether the conversation msgs ends in the run's answer: an assistant turn
// with no tool calls, or a successful call of the terminal tool.
func answerRecorded(msgs []Message, terminal string) bool {
	if n := len(msgs); n > 0 && msgs[n-1].Role == RoleAssistant && len(msgs[n-1].toolUses()) == 0 {
		return true
	}
	_, ok := terminalCallDone(msgs, terminal)
	return ok
}
