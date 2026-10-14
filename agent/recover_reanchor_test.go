package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// reanchoringStore is a MemStore that records the runs it was asked to Reanchor, as an
// audit.AuditedStore over it would be asked.
type reanchoringStore struct {
	*MemStore
	mu   sync.Mutex
	runs map[string]int
	err  error
}

func (r *reanchoringStore) Reanchor(_ context.Context, runID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runs == nil {
		r.runs = map[string]int{}
	}
	r.runs[runID]++
	return r.err
}

func (r *reanchoringStore) count(runID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runs[runID]
}

// wrapperOf is a store wrapper that passes everything through, so Reanchor is found only through
// Unwrap.
type wrapperOf struct{ Store }

func (w wrapperOf) Unwrap() Store { return w.Store }

func pausedAt(id string) error {
	return &ApprovalPending{RunRef: RunRef{RunID: id, RootRunID: id}, ToolUseID: "c1", ToolName: "pay"}
}

// A pass that finds a run waiting on a decision (a quorum gate with one of two decisions in, say)
// re-signs it: a decision another process recorded is under a signed head within one pass, not at
// the app's next write to the run.
func TestRecoverLoop_ReanchorsRunsWaitingOnAPerson(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &reanchoringStore{MemStore: NewMemStore()}
		j := mustJournal(wrapperOf{s}) // found through Unwrap
		for _, id := range []string{"gate", "halt", "timer", "done", "interrupt"} {
			seedRun(t, s.MemStore, id)
		}
		stop := runLoop(t, j, func(ctx context.Context, id string, _ RunStart) error {
			switch id {
			case "gate":
				return pausedAt(id)
			case "halt":
				return &OutcomeUnknown{RunRef: RunRef{RunID: id, RootRunID: id}, Op: OpRef{Kind: OpTool, ID: "t1"}}
			case "timer":
				return &TimerPending{RunRef: RunRef{RunID: id, RootRunID: id}, Name: "w", FireAt: time.Now().Add(time.Hour)}
			case "interrupt":
				return &InterruptPending{RunRef: RunRef{RunID: id, RootRunID: id}, Name: "q"}
			}
			return nil
		}, WithRecoverInterval(20*time.Millisecond))
		time.Sleep(100 * time.Millisecond)
		if err := stop(); !errors.Is(err, context.Canceled) {
			t.Fatalf("RecoverLoop returned %v", err)
		}
		for _, id := range []string{"gate", "halt"} {
			if s.count(id) == 0 {
				t.Errorf("run %s waits on a person and was not re-signed", id)
			}
		}
		for _, id := range []string{"timer", "done", "interrupt"} {
			if n := s.count(id); n != 0 {
				t.Errorf("run %s does not wait on a decision or a halt resolution and was re-signed %d times", id, n)
			}
		}
	})
}

// A run another holder leases is that holder's to sign: the pass does not drive it and does not
// re-sign it.
func TestRecoverLoop_DoesNotReanchorALeasedRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &reanchoringStore{MemStore: NewMemStore()}
		j := mustJournal(s)
		seedRun(t, s.MemStore, "r")
		if ok, _ := s.AcquireLease(context.Background(), "r", "other#0", time.Hour); !ok {
			t.Fatal("setup: lease")
		}
		stop := runLoop(t, j, func(context.Context, string, RunStart) error { return pausedAt("r") },
			WithRecoverInterval(20*time.Millisecond))
		time.Sleep(100 * time.Millisecond)
		stop()
		if n := s.count("r"); n != 0 {
			t.Fatalf("re-signed a run it did not drive %d times", n)
		}
	})
}

// A failed re-sign goes to the error handler, wrapping ErrStorage, and does not stop the loop: the
// next pass tries again.
func TestRecoverLoop_ReanchorFailureIsReported(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		boom := errors.New("anchor down")
		s := &reanchoringStore{MemStore: NewMemStore(), err: boom}
		j := mustJournal(s)
		seedRun(t, s.MemStore, "r")
		var mu sync.Mutex
		var got []error
		stop := runLoop(t, j, func(context.Context, string, RunStart) error { return pausedAt("r") },
			WithRecoverInterval(20*time.Millisecond),
			WithRecoverErrors(func(err error) { mu.Lock(); got = append(got, err); mu.Unlock() }))
		time.Sleep(100 * time.Millisecond)
		stop()
		mu.Lock()
		defer mu.Unlock()
		if len(got) < 2 {
			t.Fatalf("got %d reports, want one per pass", len(got))
		}
		if !errors.Is(got[0], boom) || !errors.Is(got[0], ErrStorage) {
			t.Fatalf("report %v does not wrap the anchor's error and ErrStorage", got[0])
		}
	})
}

// WithReanchor replaces the store's own: an application whose signer is not an AuditedStore in the
// journal's chain names the call, and a nil function turns re-signing off.
func TestRecoverLoop_WithReanchor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &reanchoringStore{MemStore: NewMemStore()}
		j := mustJournal(s)
		seedRun(t, s.MemStore, "r")
		var mu sync.Mutex
		var own int
		stop := runLoop(t, j, func(context.Context, string, RunStart) error { return pausedAt("r") },
			WithRecoverInterval(20*time.Millisecond),
			WithReanchor(func(context.Context, string) error { mu.Lock(); own++; mu.Unlock(); return nil }))
		time.Sleep(100 * time.Millisecond)
		stop()
		mu.Lock()
		n := own
		mu.Unlock()
		if n == 0 || s.count("r") != 0 {
			t.Fatalf("own function called %d times, the store's %d; want the function only", n, s.count("r"))
		}

		s2 := &reanchoringStore{MemStore: NewMemStore()}
		seedRun(t, s2.MemStore, "r")
		stop = runLoop(t, mustJournal(s2), func(context.Context, string, RunStart) error { return pausedAt("r") },
			WithRecoverInterval(20*time.Millisecond), WithReanchor(nil))
		time.Sleep(100 * time.Millisecond)
		stop()
		if s2.count("r") != 0 {
			t.Fatal("WithReanchor(nil) did not turn re-signing off")
		}
	})
}

// A re-sign that hangs is cut off at its deadline: it does not hold the run's lease and the loop's
// only slot for as long as the anchor blocks, and the other waiting run is re-signed.
func TestRecoverLoop_HungReanchorDoesNotStarveOtherRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := NewMemStore()
		for _, id := range []string{"a", "b"} {
			seedRun(t, s, id)
		}
		var mu sync.Mutex
		signed := map[string]int{}
		var reports []error
		stop := runLoop(t, mustJournal(s), func(_ context.Context, id string, _ RunStart) error { return pausedAt(id) },
			WithRecoverInterval(20*time.Millisecond), WithRecoverConcurrency(1), WithReanchorTimeout(time.Second),
			WithRecoverErrors(func(err error) { mu.Lock(); reports = append(reports, err); mu.Unlock() }),
			WithReanchor(func(ctx context.Context, id string) error {
				if id == "a" {
					<-ctx.Done() // the anchor hangs
					return ctx.Err()
				}
				mu.Lock()
				signed[id]++
				mu.Unlock()
				return nil
			}))
		time.Sleep(10 * time.Second)
		stop()
		mu.Lock()
		defer mu.Unlock()
		if signed["b"] == 0 {
			t.Fatal("run b was never re-signed: run a's hung re-sign holds the only slot")
		}
		if len(reports) == 0 || !errors.Is(reports[0], context.DeadlineExceeded) || !errors.Is(reports[0], ErrStorage) {
			t.Fatalf("the hung re-sign was reported as %v, want the deadline wrapping ErrStorage", reports)
		}
	})
}

func TestWithReanchorTimeout_MustBePositive(t *testing.T) {
	s := NewMemStore()
	err := RecoverLoop(context.Background(), mustJournal(s), func(context.Context, string, RunStart) error { return nil }, WithReanchorTimeout(0))
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("a zero timeout: %v, want ErrConfig", err)
	}
}

// Parallel sub-agents that wait together return their pauses joined: every one is re-signed, with
// the root, and a pause wrapped in another error is found too.
func TestWaitingRuns_WalksJoinedErrors(t *testing.T) {
	err := errors.Join(
		&ApprovalPending{RunRef: RunRef{RunID: "root>sub1", RootRunID: "root"}, ToolUseID: "c1"},
		fmt.Errorf("sub2: %w", &OutcomeUnknown{RunRef: RunRef{RunID: "root>sub2", RootRunID: "root"}}),
		&TimerPending{RunRef: RunRef{RunID: "root>sub3", RootRunID: "root"}},
	)
	got := waitingRuns(err)
	slices.Sort(got)
	if want := []string{"root", "root>sub1", "root>sub2"}; !slices.Equal(got, want) {
		t.Fatalf("waitingRuns = %v, want %v", got, want)
	}
	if waitingRuns(nil) != nil || waitingRuns(errors.New("x")) != nil {
		t.Fatal("a non-pause is a waiting run")
	}
}

// Recover, the single pass, re-signs a waiting run as RecoverLoop does, and WithReanchor(nil)
// turns it off for it too.
func TestRecover_Reanchors(t *testing.T) {
	resume := func(context.Context, string, RunStart) error { return pausedAt("r") }
	s := &reanchoringStore{MemStore: NewMemStore()}
	seedRun(t, s.MemStore, "r")
	if _, err := Recover(context.Background(), mustJournal(s), resume); err != nil || s.count("r") != 1 {
		t.Fatalf("Recover: %v, re-signed %d times; want once", err, s.count("r"))
	}
	if _, err := Recover(context.Background(), mustJournal(s), resume, WithReanchor(nil)); err != nil || s.count("r") != 1 {
		t.Fatalf("Recover with WithReanchor(nil): %v, re-signed %d times in all; want still once", err, s.count("r"))
	}
}

// Nested and wrapped joins are all walked, and a chain that loops through Unwrap ends.
func TestWaitingRuns_NestedJoinsAndCycles(t *testing.T) {
	p := func(id string) error {
		return &ApprovalPending{RunRef: RunRef{RunID: id, RootRunID: "root"}, ToolUseID: "c"}
	}
	err := fmt.Errorf("drive: %w", errors.Join(p("a"), fmt.Errorf("x: %w, y: %w", errors.Join(p("b"), errors.New("e")), p("c"))))
	got := waitingRuns(err)
	for _, id := range []string{"a", "b", "c", "root"} {
		if !slices.Contains(got, id) {
			t.Errorf("waitingRuns = %v, misses %s", got, id)
		}
	}
	loop := &cycleErr{}
	loop.next = loop
	if got := waitingRuns(loop); got != nil {
		t.Fatalf("a cycle gave %v", got)
	}
	deep := error(p("deep"))
	for range maxWaitingDepth + 5 {
		deep = fmt.Errorf("w: %w", deep)
	}
	if got := waitingRuns(deep); got != nil {
		t.Fatalf("a pause deeper than the cap was found: %v", got)
	}
}

type cycleErr struct{ next error }

func (e *cycleErr) Error() string { return "cycle" }
func (e *cycleErr) Unwrap() error { return e.next }
