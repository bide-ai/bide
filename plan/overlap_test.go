package plan

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// xprocStore is one process's handle on a store shared by separate processes: it does not
// Unwrap, so a Journal over one handle shares no in-process state (in-flight steps, remembered
// claims) with a Journal over another, and two drivers can both reach the same step; the shared
// store's single-winner Insert decides between them. If meet is set, an Insert of an attempt
// marker waits there first, forcing two drivers into the same window (released once, then open).
type xprocStore struct {
	agent.Store
	meet *rendezvous
}

func (s *xprocStore) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	if s.meet != nil && strings.HasPrefix(name, "attempt:") {
		s.meet.arrive()
	}
	return s.Store.Insert(ctx, runID, name, data)
}

type rendezvous struct {
	mu   sync.Mutex
	n    int
	want int
	ch   chan struct{}
}

func newRendezvous(want int) *rendezvous { return &rendezvous{want: want, ch: make(chan struct{})} }

func (r *rendezvous) arrive() {
	r.mu.Lock()
	r.n++
	if r.n == r.want {
		close(r.ch)
	}
	r.mu.Unlock()
	select {
	case <-r.ch:
	case <-time.After(2 * time.Second):
	}
}

// Two drivers of the same flow run overlap and reach a non-idempotent node's attempt marker at
// the same moment. Exactly one may run its body; the other must halt with OutcomeUnknown.
func TestOverlappingDrivers_FlowNodeFiresOnce(t *testing.T) {
	var fired atomic.Int32
	b := New[int, string]("charge-flow")
	charge := b.Step("charge", func(_ context.Context, n int) (int, error) {
		fired.Add(1)
		deadline := time.Now().Add(300 * time.Millisecond)
		for fired.Load() < 2 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		return n, nil
	})
	done := b.Step("done", func(context.Context, int) (string, error) { return "done", nil })
	b.Edge(charge, done)
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}

	shared, meet := agent.NewMemStore(), newRendezvous(2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = flow.Run(context.Background(), agenttest.MustJournal(&xprocStore{Store: shared, meet: meet}), "r1", 1)
		}()
	}
	wg.Wait()

	if n := fired.Load(); n != 1 {
		t.Fatalf("two overlapping drivers ran the node %d times, want 1 (errs=%v)", n, errs)
	}
	var halts, oks int
	for _, err := range errs {
		var halt *agent.OutcomeUnknown
		switch {
		case err == nil:
			oks++
		case errors.As(err, &halt) && halt.Op.ID == "node:charge":
			halts++
		default:
			t.Fatalf("unexpected driver error: %v", err)
		}
	}
	if oks != 1 || halts != 1 {
		t.Fatalf("drivers: %d completed, %d halted; want 1 and 1 (errs=%v)", oks, halts, errs)
	}
}
