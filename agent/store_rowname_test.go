package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// memTamper replaces the stored encoding of step name in run runID with data, as a corrupted or
// edited backing store would.
func memTamper(t *testing.T, m *MemStore, runID, name string, data []byte) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	rl := m.runs[runID]
	i, ok := rl.byName[name]
	if !ok {
		t.Fatalf("no step %q in run %s", name, runID)
	}
	rl.order[i] = data
}

// A row's key is the name it was recorded under, and the record it holds carries that name. A
// row whose record names another step (a row copied or edited in the backing store) must not be
// read as that other step: the loop and the recovery supervisor find records by their name, so a
// row under "x" holding a record named run:complete would mark an unfinished run complete.
func TestMemStore_RowWhoseRecordNamesAnotherStepIsRefused(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	if _, err := m.Do(ctx, "r", "x", func(context.Context) (Record, error) { return Record{Kind: StepValue, Result: []byte(`1`)}, nil }); err != nil {
		t.Fatal(err)
	}
	forged, err := JournalEntry(runCompleteStep, Record{Kind: StepValue})
	if err != nil {
		t.Fatal(err)
	}
	memTamper(t, m, "r", "x", forged)

	if done, err := IsComplete(ctx, m, "r"); !errors.Is(err, ErrStorage) {
		t.Errorf("IsComplete = %v, %v; want an ErrStorage error, not a run marked complete by a misfiled row", done, err)
	}
	if _, err := m.History(ctx, "r"); !errors.Is(err, ErrStorage) || !strings.Contains(err.Error(), `"x"`) {
		t.Errorf("History = %v; want ErrStorage naming the row's key", err)
	}
	ran := false
	if _, err := m.Do(ctx, "r", "x", func(context.Context) (Record, error) { ran = true; return Record{}, nil }); !errors.Is(err, ErrStorage) {
		t.Errorf("Do(memoized) = %v; want ErrStorage", err)
	}
	if ran {
		t.Error("Do ran fn for a recorded step")
	}
}

// A record with a field this version does not know (written by a newer version) still reads: the
// name check does not make the journal decoding strict.
func TestMemStore_UnknownFieldStillReads(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	if _, err := m.Do(ctx, "r", "x", func(context.Context) (Record, error) { return Record{Kind: StepValue, Result: []byte(`1`)}, nil }); err != nil {
		t.Fatal(err)
	}
	memTamper(t, m, "r", "x", []byte(`{"name":"x","kind":"value","result":1,"future_field":{"a":1}}`))
	recs, err := m.History(ctx, "r")
	if err != nil || len(recs) != 1 || recs[0].Name != "x" {
		t.Fatalf("History = %+v, %v; want the one record", recs, err)
	}
	if rec, err := m.Do(ctx, "r", "x", func(context.Context) (Record, error) { return Record{}, errors.New("must not run") }); err != nil || string(rec.Result) != "1" {
		t.Fatalf("Do = %+v, %v", rec, err)
	}
}
