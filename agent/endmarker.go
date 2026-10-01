// endmarker.go holds the rule every reader and writer of a run's end markers follows (D1, model 10's
// L3): the first end marker in journal order (run:complete, run:aborted or run:cancelled) is the
// run's end. The keys are distinct, so two can land (A1 is per key); a writer reads the markers back
// after its own write and reports the first.

package agent

import (
	"context"
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
