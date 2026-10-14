package agent

import (
	"context"
	"errors"
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
