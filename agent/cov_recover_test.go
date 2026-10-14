package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// recorder is a Resumer that records the runs it was handed.
type recorder struct {
	mu   sync.Mutex
	runs []string
}

func (r *recorder) resume(_ context.Context, runID string, _ RunStart) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, runID)
	return nil
}

// Recover refuses a nil Resumer and a store that cannot list runs, before touching any run.
func TestRecover_Refusals(t *testing.T) {
	ctx := context.Background()
	if _, err := Recover(ctx, memJournal(), nil); !errors.Is(err, ErrConfig) {
		t.Errorf("nil Resumer = %v; want ErrConfig", err)
	}
	rec := &recorder{}
	if _, err := Recover(ctx, mustJournal(plainStore{NewMemStore()}), rec.resume); !errors.Is(err, ErrConfig) {
		t.Errorf("a store with no Lister = %v; want ErrConfig", err)
	}
	if _, err := Recover(ctx, memJournal(), rec.resume, WithNotifier(nil, NewMemNotifyTrail())); !errors.Is(err, ErrConfig) {
		t.Errorf("a nil notifier sink = %v; want ErrConfig", err)
	}
	if len(rec.runs) != 0 {
		t.Errorf("refused passes resumed %v", rec.runs)
	}
}

// A run whose recorded start cannot be read or decoded is reported (ErrStorage) and not resumed,
// and it does not stop the pass: the healthy runs around it are still recovered.
func TestRecover_ContinuesPastAnUnreadableStart(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	seedRun(t, s, "a-ok")
	seedRun(t, s, "z-ok")
	j := mustJournal(s)
	writeRaw(t, j, "m-bad", runStartStep, Record{Kind: StepValue, Result: []byte(`[1]`)})
	writeRaw(t, j, "m-bad", "seed", Record{Kind: StepValue})
	rec := &recorder{}
	n, err := Recover(ctx, j, rec.resume)
	if !errors.Is(err, ErrStorage) {
		t.Fatalf("Recover = %v; want the bad start reported as ErrStorage", err)
	}
	if n != 2 || len(rec.runs) != 2 || rec.runs[0] != "a-ok" || rec.runs[1] != "z-ok" {
		t.Fatalf("recovered %d runs %v; want a-ok and z-ok only", n, rec.runs)
	}
}

// A store that cannot read run:start at all is a storage error for each run, and no run is resumed
// blind.
func TestRecover_UnreadableStartIsStorageError(t *testing.T) {
	s := NewMemStore()
	seedRun(t, s, "a")
	rec := &recorder{}
	_, err := Recover(context.Background(), mustJournal(covGetFailLister{covGetFail{s, runStartStep}, s}), rec.resume)
	if !errors.Is(err, ErrStorage) || len(rec.runs) != 0 {
		t.Fatalf("Recover = %v, resumed %v; want ErrStorage and none", err, rec.runs)
	}
}

// covGetFailLister keeps the Lister of the store covGetFail wraps.
type covGetFailLister struct {
	covGetFail
	l *MemStore
}

func (c covGetFailLister) Unwrap() Store { return c.l }

// A listing that fails partway is reported (ErrStorage) with the runs recovered before it counted.
func TestRecover_ListingFailureKeepsWhatWasRecovered(t *testing.T) {
	s := covRunFault{MemStore: NewMemStore(), runsErr: true}
	seedRun(t, s.MemStore, "a")
	rec := &recorder{}
	n, err := Recover(context.Background(), mustJournal(s), rec.resume)
	if !errors.Is(err, ErrStorage) || n != 1 || len(rec.runs) != 1 {
		t.Fatalf("Recover = %d, %v (resumed %v); want 1 recovered and ErrStorage", n, err, rec.runs)
	}
}

// A run in a journal format this version cannot read is reported as a *JournalVersionError (not as
// an unstarted run, which a later pass would skip silently), and not resumed.
func TestRecover_ForeignFormatIsReportedAsSuch(t *testing.T) {
	s := NewMemStore()
	seedHeader(t, s, "old", `{"name":"`+headerStep+`","kind":"`+string(StepHeader)+`","format":"bide-journal-999"}`)
	rec := &recorder{}
	_, err := Recover(context.Background(), mustJournal(s), rec.resume)
	var ve *JournalVersionError
	if !errors.As(err, &ve) || ve.RunID != "old" || len(rec.runs) != 0 {
		t.Fatalf("Recover = %v, resumed %v; want a *JournalVersionError for old and none resumed", err, rec.runs)
	}
}
