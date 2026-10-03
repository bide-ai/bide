package agent

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// xprocStore is a store shared by separate processes. Each process drives it through a Journal
// of its own (proc), over a view of the store with an identity of its own, so two processes share
// no in-process state (in-flight steps, remembered claims): two drivers can both run fn for the
// same name, and the store's atomic insert decides which record stands (as SQLite and Postgres do
// with a primary key). The store does not lease runs: it holds its MemStore as a field, so neither
// it nor a view exposes the MemStore's Leaser or Lister. If meet is set, every Insert of an
// attempt marker waits there first, forcing two drivers into the same window.
type xprocStore struct {
	mem  *MemStore
	meet *rendezvous
}

func newXprocStore() *xprocStore { return &xprocStore{mem: NewMemStore()} }

// proc returns a new process's Journal over the store.
func (s *xprocStore) proc() *Journal { return mustJournal(&xprocView{s}) }

// xprocView is one process's view of an xprocStore. It does not implement Unwrap, so it is its
// own identity.
type xprocView struct{ s *xprocStore }

func (v *xprocView) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	if v.s.meet != nil && strings.HasPrefix(name, "attempt:") {
		v.s.meet.arrive()
	}
	return v.s.mem.Insert(ctx, runID, name, data)
}

func (v *xprocView) Get(ctx context.Context, runID, name string) (Entry, bool, error) {
	return v.s.mem.Get(ctx, runID, name)
}

func (v *xprocView) Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error] {
	return v.s.mem.Load(ctx, runID, after)
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
	if _, err := store.proc().do(ctx, "r1", "@llm/0", func(context.Context) (Record, error) {
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
		_, err := mustNew(&greedyModel{script: [][]Emit{textTurn("done")}}, store.proc(), WithTools(charge)).Run(ctx, "r1", UserText("pay"))
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
			if rerr := ResolveHaltRef(ctx, store.proc(), halt.Ref(), Outcome{Result: "ok"}); !errors.Is(rerr, ErrConfig) {
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
	store := memJournal()
	won, got, err := ClaimAttempt(ctx, store, "r1", "attempt:c1", Record{Kind: StepAttempt, ToolUseID: "c1"})
	if err != nil || !won || got.claim == "" {
		t.Fatalf("first claim: won=%v claim=%q err=%v, want won with a claim id", won, got.claim, err)
	}
	again, got2, err := ClaimAttempt(ctx, store, "r1", "attempt:c1", Record{Kind: StepAttempt, ToolUseID: "c1"})
	if err != nil || again || got2.claim != got.claim {
		t.Fatalf("second claim: won=%v claim=%q err=%v, want lost to the first claim %q", again, got2.claim, err, got.claim)
	}
}
