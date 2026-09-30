package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// xprocStore gives Do the semantics of a store shared by separate processes: no in-process
// singleflight, so two drivers can both run fn for the same name, and the insert is atomic, so a
// later insert of an already-recorded name returns the earlier record (as SQLite and Postgres do
// with a primary key). If meet is set, every Do on an attempt marker waits there first, forcing two
// drivers into the same window.
type xprocStore struct {
	mu    sync.Mutex
	order map[string][]Record
	index map[string]map[string]int
	meet  *rendezvous
}

func newXprocStore() *xprocStore {
	return &xprocStore{order: map[string][]Record{}, index: map[string]map[string]int{}}
}

func (s *xprocStore) lookup(runID, name string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i, ok := s.index[runID][name]; ok {
		return s.order[runID][i], true
	}
	return Record{}, false
}

func (s *xprocStore) Do(ctx context.Context, runID, name string, fn func(context.Context) (Record, error)) (Record, error) {
	if s.meet != nil && strings.HasPrefix(name, "attempt:") {
		s.meet.arrive()
	}
	if r, ok := s.lookup(runID, name); ok {
		return r, nil
	}
	rec, err := fn(ctx) // outside the lock, like a separate process doing its work
	if err != nil {
		return Record{}, err
	}
	rec.Name = name
	s.mu.Lock()
	defer s.mu.Unlock()
	if i, ok := s.index[runID][name]; ok {
		return s.order[runID][i], nil // the other process's insert landed first
	}
	if s.index[runID] == nil {
		s.index[runID] = map[string]int{}
	}
	s.index[runID][name] = len(s.order[runID])
	s.order[runID] = append(s.order[runID], rec)
	return rec, nil
}

func (s *xprocStore) History(_ context.Context, runID string) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Record(nil), s.order[runID]...), nil
}

// rendezvous releases its callers once want of them have arrived (or after a safety timeout).
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

// Two drivers of the same run overlap, as when a node stalls past its lease and a second node
// takes over: both see the same pending non-idempotent call, both reach its attempt marker at the
// same moment. Exactly one may run the side effect; the other must halt, not run it again. The
// charge holds itself open, so a second driver that got past its claim would be caught inside it.
func TestOverlappingDrivers_SideEffectFiresOnce(t *testing.T) {
	ctx := context.Background()
	store := newXprocStore()
	asst := Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "c1", Name: "charge", Args: json.RawMessage(`{}`)}}}
	if _, err := store.Do(ctx, "r1", "@llm/0", func(context.Context) (Record, error) {
		return Record{Kind: StepModel, Message: &asst}, nil
	}); err != nil {
		t.Fatal(err)
	}
	store.meet = newRendezvous(2)

	var fired atomic.Int32
	charge := Func("charge", "charge the card", Safety{}, func(context.Context, struct{}) (string, error) {
		fired.Add(1)
		deadline := time.Now().Add(300 * time.Millisecond)
		for fired.Load() < 2 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		return "ok", nil
	})
	drive := func() error {
		_, err := New(&greedyModel{script: [][]Emit{textTurn("done")}}, store, charge).Run(ctx, "r1", "pay")
		return err
	}

	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = drive() }()
	}
	wg.Wait()

	if n := fired.Load(); n != 1 {
		t.Fatalf("two overlapping drivers ran the charge %d times, want 1 (errs=%v)", n, errs)
	}
	var halts, oks int
	for _, err := range errs {
		var halt *ResumeHalt
		switch {
		case err == nil:
			oks++
		case errors.As(err, &halt) && halt.Op.ID == "c1":
			// The loser lost the claim while running: the winner may be running the charge now.
			if halt.Cause != HaltContended || halt.Op.Kind != OpTool {
				t.Fatalf("lost-claim halt = %+v, want an OpTool halt with Cause %q", halt, HaltContended)
			}
			// The winner may still be running the charge, so its halt cannot be resolved blind.
			if rerr := ResolveHaltRef(ctx, store, halt.Ref(), Outcome{Result: "ok"}); !errors.Is(rerr, ErrConfig) {
				t.Fatalf("ResolveHaltRef on a contended halt without WithMinHaltAge = %v, want ErrConfig", rerr)
			}
			halts++
		default:
			t.Fatalf("unexpected driver error: %v", err)
		}
	}
	if oks != 1 || halts != 1 {
		t.Fatalf("drivers: %d completed, %d halted; want 1 and 1 (errs=%v)", oks, halts, errs)
	}

	// Once the winner recorded the result, re-driving (the halted driver retrying) completes
	// without running the charge again.
	store.meet = nil
	if err := drive(); err != nil {
		t.Fatalf("re-drive after the winner finished: %v", err)
	}
	if n := fired.Load(); n != 1 {
		t.Fatalf("charge ran %d times after the re-drive, want 1", n)
	}
}

// A single driver always wins its own claim, so an uncontended run is unaffected and its
// attempt marker carries the claim.
func TestClaimAttempt_SingleDriverWins(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	won, got, err := ClaimAttempt(ctx, store, "r1", "attempt:c1", Record{Kind: StepAttempt, ToolUseID: "c1"})
	if err != nil || !won || got.claim == "" {
		t.Fatalf("first claim: won=%v claim=%q err=%v, want won with a claim id", won, got.claim, err)
	}
	again, got2, err := ClaimAttempt(ctx, store, "r1", "attempt:c1", Record{Kind: StepAttempt, ToolUseID: "c1"})
	if err != nil || again || got2.claim != got.claim {
		t.Fatalf("second claim: won=%v claim=%q err=%v, want lost to the first claim %q", again, got2.claim, err, got.claim)
	}
}
