package agent

import (
	"context"
	"errors"
	"iter"
	"sync"
	"testing"
)

// pcHookStore wraps a Store: it counts Inserts, and calls onGet after each Get and onLoad after
// each Load's first yielded entry, so a test can land another writer's entry at an exact point of
// a reader's sequence of reads (no sleeps).
type pcHookStore struct {
	Store
	mu      sync.Mutex
	inserts []string
	onGet   func(runID, name string)
	onLoad  func(runID string)
}

func (s *pcHookStore) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	e, ins, err := s.Store.Insert(ctx, runID, name, data)
	if ins {
		s.mu.Lock()
		s.inserts = append(s.inserts, name)
		s.mu.Unlock()
	}
	return e, ins, err
}

func (s *pcHookStore) Get(ctx context.Context, runID, name string) (Entry, bool, error) {
	e, ok, err := s.Store.Get(ctx, runID, name)
	if f := s.onGet; f != nil {
		f(runID, name)
	}
	return e, ok, err
}

func (s *pcHookStore) Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error] {
	return func(yield func(Entry, error) bool) {
		first := true
		for e, err := range s.Store.Load(ctx, runID, after) {
			if !yield(e, err) {
				return
			}
			if first && err == nil {
				first = false
				if f := s.onLoad; f != nil {
					f(runID)
				}
			}
		}
	}
}

func (s *pcHookStore) takeInserts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.inserts
	s.inserts = nil
	return out
}

// pcJournal returns a journal over a pcHookStore on a MemStore.
func pcJournal(t *testing.T) (*Journal, *pcHookStore) {
	t.Helper()
	hs := &pcHookStore{Store: NewMemStore()}
	j, err := NewJournal(hs)
	if err != nil {
		t.Fatal(err)
	}
	return j, hs
}

// pcStart journals runID's run:start.
func pcStart(t *testing.T, j *Journal, runID string, saga bool) {
	t.Helper()
	b, err := marshalJournal(RunStart{Input: UserText("go"), Saga: saga})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.put(context.Background(), runID, runStartStep, Record{Kind: StepValue, Result: b}); err != nil {
		t.Fatal(err)
	}
}

// pcMark journals the value step name (an end marker, a request) of runID with text.
func pcMark(t *testing.T, j *Journal, runID, name, text string) {
	t.Helper()
	if _, err := j.put(context.Background(), runID, name, Record{Kind: StepValue, Result: mustJSON(text)}); err != nil {
		t.Fatal(err)
	}
}

// Rule 1: Cancel reads the end markers and refuses a run that is over: a completed or aborted run
// is ErrRunEnded with nothing written, and a cancelled one is cancelled already (nil, nothing
// written).
func TestP14Rule01_CancelRefusesARunThatIsOver(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, marker string
		saga         bool
		want         error
	}{
		{"completed", runCompleteStep, false, ErrRunEnded},
		{"aborted saga", runAbortedStep, true, ErrRunEnded},
		{"completed saga", runCompleteStep, true, ErrRunEnded},
		{"cancelled", runCancelledStep, false, nil},
		{"cancelled saga", runCancelledStep, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j, hs := pcJournal(t)
			pcStart(t, j, "r", tc.saga)
			pcMark(t, j, "r", tc.marker, "x")
			hs.takeInserts()
			err := Cancel(ctx, j, "r", "stop")
			if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("Cancel = %v, want %v", err, tc.want)
			}
			if got := hs.takeInserts(); len(got) != 0 {
				t.Fatalf("Cancel of a run that is over wrote %v", got)
			}
		})
	}
	// The first end marker decides: a run completed and then cancelled (two keys can both land)
	// is over as completed.
	j, hs := pcJournal(t)
	pcStart(t, j, "r", false)
	pcMark(t, j, "r", runCompleteStep, "")
	pcMark(t, j, "r", runCancelledStep, "late")
	hs.takeInserts()
	if err := Cancel(ctx, j, "r", "stop"); !errors.Is(err, ErrRunEnded) {
		t.Fatalf("Cancel of a run completed first = %v, want ErrRunEnded", err)
	}
	// And one cancelled first, then completed, is cancelled.
	j, _ = pcJournal(t)
	pcStart(t, j, "r", false)
	pcMark(t, j, "r", runCancelledStep, "first")
	pcMark(t, j, "r", runCompleteStep, "")
	if err := Cancel(ctx, j, "r", "stop"); err != nil {
		t.Fatalf("Cancel of a run cancelled first = %v, want nil", err)
	}
	// A live run is cancelled: run:cancelled {reason} is written.
	j, hs = pcJournal(t)
	pcStart(t, j, "r", false)
	hs.takeInserts()
	if err := Cancel(ctx, j, "r", "stop"); err != nil {
		t.Fatalf("Cancel of a live run = %v", err)
	}
	if got := hs.takeInserts(); len(got) != 1 || got[0] != runCancelledStep {
		t.Fatalf("Cancel wrote %v, want [run:cancelled]", got)
	}
	rec, ok, err := j.Get(ctx, "r", runCancelledStep)
	if err != nil || !ok || string(rec.Result) != `{"reason":"stop"}` {
		t.Fatalf("run:cancelled = %s, %v, %v; want {\"reason\":\"stop\"}", rec.Result, ok, err)
	}
	if err := Cancel(ctx, nil, "r", "x"); !errors.Is(err, ErrConfig) {
		t.Fatalf("Cancel with a nil journal = %v, want ErrConfig", err)
	}
	if err := Cancel(ctx, j, "", "x"); !errors.Is(err, ErrConfig) {
		t.Fatalf("Cancel of an empty run ID = %v, want ErrConfig", err)
	}
	// A session turn's run (and a sub-run) is a run: its ID is accepted.
	turn := sessionTurnRunID("chat", 0)
	pcStart(t, j, turn, false)
	if err := Cancel(ctx, j, turn, "x"); err != nil {
		t.Fatalf("Cancel of a session turn's run = %v", err)
	}
}

// Rule 4 (L3), Cancel's side: every writer of an end marker reads the markers back and reports
// the first. run:complete lands after Cancel's checks and before its insert; Cancel's own
// run:cancelled lands second, so the run completed, and Cancel says so (model 10's
// regress/cancel-verdict is Cancel reporting cancelled here).
func TestP14Rule04_CancelReadsBackTheFirstEndMarker(t *testing.T) {
	ctx := context.Background()
	j, hs := pcJournal(t)
	pcStart(t, j, "r", false)
	fired := false
	hs.onGet = func(runID, name string) {
		if name == runStartStep && !fired { // Cancel's last read before its insert
			fired = true
			if _, err := j.put(ctx, runID, runCompleteStep, Record{Kind: StepValue}); err != nil {
				t.Error(err)
			}
		}
	}
	err := Cancel(ctx, j, "r", "stop")
	if !fired {
		t.Fatal("the hook never fired: Cancel did not read run:start")
	}
	if !errors.Is(err, ErrRunEnded) {
		t.Fatalf("Cancel = %v, want ErrRunEnded: run:complete is the first end marker", err)
	}
	st, serr := Status(ctx, j, "r")
	if serr != nil || st.State != RunCompleted {
		t.Fatalf("Status = %+v, %v; want completed", st, serr)
	}
}

// Rule 5 (L4), Cancel's side: Cancel reads run:start. A run with none is ErrNotStarted and nothing
// is written; on a saga Cancel writes the rollback request run:cancel-requested, not the end
// marker run:cancelled, so recovery still lists the run, and Status says Started.
func TestP14Rule05_CancelNotStartedAndSagaRequest(t *testing.T) {
	ctx := context.Background()
	j, hs := pcJournal(t)
	// A run that holds records but no run:start (a Signal sent to a mistyped ID).
	pcMark(t, j, "r", "signal:x", "hi")
	hs.takeInserts()
	err := Cancel(ctx, j, "r", "stop")
	if !errors.Is(err, ErrNotStarted) || !errors.Is(err, ErrConfig) {
		t.Fatalf("Cancel of a run with no run:start = %v, want ErrNotStarted", err)
	}
	if got := hs.takeInserts(); len(got) != 0 {
		t.Fatalf("Cancel of a run with no run:start wrote %v", got)
	}
	// And one with no entries at all.
	if err := Cancel(ctx, j, "empty", "stop"); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("Cancel of an empty run = %v, want ErrNotStarted", err)
	}
	if got := hs.takeInserts(); len(got) != 0 {
		t.Fatalf("Cancel of an empty run wrote %v", got)
	}

	pcStart(t, j, "saga", true)
	hs.takeInserts()
	if err := Cancel(ctx, j, "saga", "stop"); err != nil {
		t.Fatalf("Cancel of a saga = %v", err)
	}
	if got := hs.takeInserts(); len(got) != 1 || got[0] != runCancelRequestedStep {
		t.Fatalf("Cancel of a saga wrote %v, want [run:cancel-requested]", got)
	}
	rec, ok, err := j.Get(ctx, "saga", runCancelRequestedStep)
	if err != nil || !ok || string(rec.Result) != `{"reason":"stop"}` {
		t.Fatalf("run:cancel-requested = %s, %v, %v", rec.Result, ok, err)
	}
	if _, ok, _ := j.Get(ctx, "saga", runCancelledStep); ok {
		t.Fatal("Cancel of a saga wrote run:cancelled: recovery would never roll it back (L4)")
	}
	if over, err := runEnded(ctx, j, "saga"); err != nil || over {
		t.Fatalf("runEnded of a saga with a rollback request = %v, %v; want not over", over, err)
	}
	st, err := Status(ctx, j, "saga")
	if err != nil || st.State != RunStarted {
		t.Fatalf("Status of a saga with a rollback request = %+v, %v; want started", st, err)
	}
	// A second Cancel while the request is pending is a no-op success.
	if err := Cancel(ctx, j, "saga", "again"); err != nil {
		t.Fatalf("second Cancel of a saga = %v", err)
	}
	if got := hs.takeInserts(); len(got) != 0 {
		t.Fatalf("second Cancel of a saga wrote %v", got)
	}
}
