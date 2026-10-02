package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// prCountingStore counts the Gets of run:start through it. It unwraps to the store it wraps, so
// Recover finds that store's Lister and Leaser.
type prCountingStore struct {
	Store
	startGets atomic.Int64
}

func (s *prCountingStore) Unwrap() Store { return s.Store }

func (s *prCountingStore) Get(ctx context.Context, runID, name string) (Entry, bool, error) {
	if name == runStartStep {
		s.startGets.Add(1)
	}
	return s.Store.Get(ctx, runID, name)
}

// prJournal returns a journal over a fresh MemStore that counts its run:start Gets.
func prJournal(t *testing.T) (*Journal, *prCountingStore) {
	t.Helper()
	cs := &prCountingStore{Store: NewMemStore()}
	j, err := NewJournal(cs)
	if err != nil {
		t.Fatal(err)
	}
	return j, cs
}

// prPut records a value step name in run id.
func prPut(t *testing.T, j *Journal, id, name string, result string) {
	t.Helper()
	rec := Record{Kind: StepValue}
	if result != "" {
		rec.Result = []byte(result)
	}
	if _, err := j.put(context.Background(), id, name, rec); err != nil {
		t.Fatalf("put %s/%s: %v", id, name, err)
	}
}

// prStart records st as run id's run:start.
func prStart(t *testing.T, j *Journal, id string, st RunStart) {
	t.Helper()
	b, err := marshalJournal(st)
	if err != nil {
		t.Fatal(err)
	}
	prPut(t, j, id, runStartStep, string(b))
}

// Rule 6: Recover excludes the runs holding run:complete, run:aborted or run:cancelled, and not a
// saga holding only its rollback request (run:cancel-requested), which it hands to the resumer
// with its run:start.
func TestP14Rule06_CancelRequestedSagaIsRecovered(t *testing.T) {
	ctx := context.Background()
	j, _ := prJournal(t)
	prStart(t, j, "requested", RunStart{Input: UserText("trip"), Saga: true})
	prPut(t, j, "requested", runCancelRequestedStep, `{"reason":"stop"}`)
	for _, end := range endOfRunMarkers {
		prStart(t, j, "ended-"+end, RunStart{Input: UserText("x"), Saga: true})
		prPut(t, j, "ended-"+end, end, "")
	}
	var got []string
	var starts []RunStart
	n, err := Recover(ctx, j, func(_ context.Context, id string, st RunStart) error {
		got, starts = append(got, id), append(starts, st)
		return nil
	})
	if err != nil || n != 1 || len(got) != 1 || got[0] != "requested" {
		t.Fatalf("Recover = %d, %v, resumed %v; want only the saga holding its rollback request", n, err, got)
	}
	if !starts[0].Saga || starts[0].Input.Text() != "trip" {
		t.Fatalf("resumer got start %+v; want the run's run:start (a saga answering \"trip\")", starts[0])
	}
}

// Rule 14: recovery reads run:start under the lease for every run it visits; a run with none is
// skipped (not resumed, not counted) and reported (ErrNotStarted) once per process, however many
// passes visit it.
func TestP14Rule14_NotStartedSkippedAndReportedOnce(t *testing.T) {
	ctx := context.Background()
	j, cs := prJournal(t)
	prPut(t, j, "mistyped", signalStep("go"), "")
	resumed := 0
	resume := func(context.Context, string, RunStart) error { resumed++; return nil }
	n, err := Recover(ctx, j, resume)
	if n != 0 || resumed != 0 || !errors.Is(err, ErrNotStarted) {
		t.Fatalf("first pass = %d, %v (resumed %d); want 0 and ErrNotStarted", n, err, resumed)
	}
	n, err = Recover(ctx, j, resume)
	if n != 0 || resumed != 0 || err != nil {
		t.Fatalf("second pass = %d, %v (resumed %d); want 0 and no second report", n, err, resumed)
	}
	if g := cs.startGets.Load(); g != 2 {
		t.Fatalf("run:start read %d times over two passes; want once per pass", g)
	}
}

// Rule 15 (model 10, finding L7, findings/not-started-remembered): what the process remembers is
// the report, not the skip. A pass finds the run before its first drive wrote run:start (a worker
// took its lease first); the first drive then writes run:start and dies; the next pass recovers it.
func TestP14Rule15_StartedAfterSkipIsRecovered(t *testing.T) {
	ctx := context.Background()
	j, _ := prJournal(t)
	prPut(t, j, "r", signalStep("early"), "") // the run exists, with no run:start yet
	var resumed []RunStart
	resume := func(_ context.Context, _ string, st RunStart) error { resumed = append(resumed, st); return nil }
	if n, err := Recover(ctx, j, resume); n != 0 || !errors.Is(err, ErrNotStarted) {
		t.Fatalf("pass before the first drive = %d, %v; want the run skipped and reported", n, err)
	}
	prStart(t, j, "r", RunStart{Input: UserText("late")}) // the first drive starts the run, then dies
	n, err := Recover(ctx, j, resume)
	if err != nil || n != 1 || len(resumed) != 1 || resumed[0].Input.Text() != "late" {
		t.Fatalf("pass after the first drive = %d, %v, resumed %+v; want the run recovered with its start", n, err, resumed)
	}
}

// RecoverLoop reports a not-started run to WithRecoverErrors once, however many passes read it.
func TestP14Rule14_RecoverLoopReportsNotStartedOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		j, cs := prJournal(t)
		prPut(t, j, "mistyped", signalStep("go"), "")
		var mu sync.Mutex
		var reports []error
		var resumed atomic.Int64
		stop := runLoop(t, j, func(context.Context, string, RunStart) error {
			resumed.Add(1)
			return nil
		}, WithRecoverInterval(10*time.Millisecond), WithRecoverErrors(func(err error) {
			mu.Lock()
			reports = append(reports, err)
			mu.Unlock()
		}))
		for i := 0; cs.startGets.Load() < 4 && i < 100; i++ {
			time.Sleep(10 * time.Millisecond) // synctest: fake time, advances only when every goroutine waits
		}
		if err := stop(); !errors.Is(err, context.Canceled) {
			t.Fatalf("RecoverLoop = %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if g := cs.startGets.Load(); g < 4 {
			t.Fatalf("run:start read %d times in 100 intervals; want a read every pass", g)
		}
		if len(reports) != 1 || !errors.Is(reports[0], ErrNotStarted) || resumed.Load() != 0 {
			t.Fatalf("reports = %v, resumed %d; want one ErrNotStarted over %d passes and no resume", reports, resumed.Load(), cs.startGets.Load())
		}
	})
}

// A run no Resumer drives (ErrNotResumable) is reported once per process, is not a failure on
// later passes, and is not counted as recovered.
func TestP14Recover_NotResumableReportedOnce(t *testing.T) {
	ctx := context.Background()
	j, _ := prJournal(t)
	prStart(t, j, "flow", RunStart{Input: UserText("{}"), Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}})
	calls := 0
	resume := func(context.Context, string, RunStart) error {
		calls++
		return fmt.Errorf("not mine: %w", ErrNotResumable)
	}
	n, err := Recover(ctx, j, resume)
	if n != 0 || !errors.Is(err, ErrNotResumable) {
		t.Fatalf("first pass = %d, %v; want 0 and ErrNotResumable", n, err)
	}
	n, err = Recover(ctx, j, resume)
	if n != 0 || err != nil || calls != 2 {
		t.Fatalf("second pass = %d, %v (calls %d); want 0, no second report, the resumer asked again", n, err, calls)
	}
}

// ResumeAny hands a run to each Resumer in order until one does not return ErrNotResumable.
func TestP14ResumeAny_Order(t *testing.T) {
	ctx := context.Background()
	var order []string
	r := func(name string, err error) Resumer {
		return func(context.Context, string, RunStart) error { order = append(order, name); return err }
	}
	boom := errors.New("boom")
	if err := ResumeAny(r("a", ErrNotResumable), r("b", fmt.Errorf("x: %w", ErrNotResumable)), r("c", boom), r("d", nil))(ctx, "r", RunStart{}); !errors.Is(err, boom) {
		t.Fatalf("ResumeAny = %v; want c's error", err)
	}
	if fmt.Sprint(order) != "[a b c]" {
		t.Fatalf("order = %v; want [a b c]", order)
	}
	order = nil
	if err := ResumeAny(r("a", nil), r("b", nil))(ctx, "r", RunStart{}); err != nil || fmt.Sprint(order) != "[a]" {
		t.Fatalf("ResumeAny = %v, order %v; want nil after a", err, order)
	}
	if err := ResumeAny(r("a", ErrNotResumable))(ctx, "r", RunStart{}); !errors.Is(err, ErrNotResumable) {
		t.Fatalf("ResumeAny with none willing = %v; want ErrNotResumable", err)
	}
	if err := ResumeAny()(ctx, "r", RunStart{}); !errors.Is(err, ErrNotResumable) {
		t.Fatalf("ResumeAny() = %v; want ErrNotResumable", err)
	}
	if err := ResumeAny(r("a", ErrNotResumable), nil)(ctx, "r", RunStart{}); !errors.Is(err, ErrConfig) {
		t.Fatalf("ResumeAny with a nil Resumer = %v; want ErrConfig", err)
	}
}

// A nil Resumer is ErrConfig for Recover and RecoverLoop.
func TestP14Recover_NilResumer(t *testing.T) {
	j, _ := prJournal(t)
	if _, err := Recover(context.Background(), j, nil); !errors.Is(err, ErrConfig) {
		t.Fatalf("Recover(nil) = %v; want ErrConfig", err)
	}
	if err := RecoverLoop(context.Background(), j, nil); !errors.Is(err, ErrConfig) {
		t.Fatalf("RecoverLoop(nil) = %v; want ErrConfig", err)
	}
}
