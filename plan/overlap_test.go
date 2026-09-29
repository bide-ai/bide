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
)

// xprocStore gives Do the semantics of a store shared by separate processes: no in-process
// singleflight, so two drivers can both run fn for the same name, and an atomic insert, so a later
// insert of an already-recorded name returns the earlier record. If meet is set, Do on an attempt
// marker waits there first, forcing two drivers into the same window (released once, then open).
type xprocStore struct {
	mu    sync.Mutex
	order map[string][]agent.Record
	index map[string]map[string]int
	meet  *rendezvous
}

func newXprocStore() *xprocStore {
	return &xprocStore{order: map[string][]agent.Record{}, index: map[string]map[string]int{}}
}

func (s *xprocStore) Do(ctx context.Context, runID, name string, fn func(context.Context) (agent.Record, error)) (agent.Record, error) {
	if s.meet != nil && strings.HasPrefix(name, "attempt:") {
		s.meet.arrive()
	}
	s.mu.Lock()
	if i, ok := s.index[runID][name]; ok {
		r := s.order[runID][i]
		s.mu.Unlock()
		return r, nil
	}
	s.mu.Unlock()
	rec, err := fn(ctx)
	if err != nil {
		return agent.Record{}, err
	}
	rec.Name = name
	s.mu.Lock()
	defer s.mu.Unlock()
	if i, ok := s.index[runID][name]; ok {
		return s.order[runID][i], nil
	}
	if s.index[runID] == nil {
		s.index[runID] = map[string]int{}
	}
	s.index[runID][name] = len(s.order[runID])
	s.order[runID] = append(s.order[runID], rec)
	return rec, nil
}

func (s *xprocStore) History(_ context.Context, runID string) ([]agent.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agent.Record(nil), s.order[runID]...), nil
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
// the same moment. Exactly one may run its body; the other must halt with HaltAmbiguous.
func TestOverlappingDrivers_FlowNodeFiresOnce(t *testing.T) {
	var fired atomic.Int32
	b := New[int, string]("charge-flow")
	charge := b.Step("charge", func(n int) (int, error) {
		fired.Add(1)
		deadline := time.Now().Add(300 * time.Millisecond)
		for fired.Load() < 2 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		return n, nil
	})
	done := b.Step("done", func(int) (string, error) { return "done", nil })
	b.Edge(charge, done)
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}

	store := newXprocStore()
	store.meet = newRendezvous(2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = flow.Run(context.Background(), store, "r1", 1)
		}()
	}
	wg.Wait()

	if n := fired.Load(); n != 1 {
		t.Fatalf("two overlapping drivers ran the node %d times, want 1 (errs=%v)", n, errs)
	}
	var halts, oks int
	for _, err := range errs {
		var halt *HaltAmbiguous
		switch {
		case err == nil:
			oks++
		case errors.As(err, &halt) && halt.Step == "charge":
			halts++
		default:
			t.Fatalf("unexpected driver error: %v", err)
		}
	}
	if oks != 1 || halts != 1 {
		t.Fatalf("drivers: %d completed, %d halted; want 1 and 1 (errs=%v)", oks, halts, errs)
	}
}
