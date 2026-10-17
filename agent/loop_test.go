package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Single-flight: many concurrent Do calls on the SAME (runID,name) run fn exactly once.
// Run with -race to also prove no data race. Prevents double side effects under
// concurrency (parallel tools / retries) — the journal's "at-most-once execution" contract.
func TestMemStore_SingleFlight(t *testing.T) {
	store := memJournal()
	var calls int32
	var wg sync.WaitGroup

	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = store.do(context.Background(), "run", "step", func(context.Context) (Record, error) {
				atomic.AddInt32(&calls, 1)
				time.Sleep(2 * time.Millisecond) // widen the race window
				return Record{Kind: StepValue, Result: json.RawMessage(`1`)}, nil
			})
		}()
	}
	wg.Wait()

	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("fn ran %d times under 64 concurrent same-key Do, want 1 (single-flight)", n)
	}
	h, _ := store.History(context.Background(), "run")
	if len(h) != 2 { // the journal header, then the step
		t.Fatalf("history len %d, want the header and 1", len(h))
	}
}

func multiToolTurn(calls ...[2]string) []Emit {
	var e []Emit
	for i, c := range calls {
		e = append(e, Emit{Event: ToolCallDelta{Index: i, ID: c[0], Name: c[1], ArgsFragment: json.RawMessage(`{}`)}})
	}
	return append(e, Emit{Event: Finish{Reason: "tool_use"}})
}

// A turn's tool calls run CONCURRENTLY: three tools rendezvous at a barrier that can only
// be crossed if all three are in flight at once. If execution were sequential, the first
// tool would block at the barrier and time out. Run with -race too.
func TestParallelTools_RunConcurrently(t *testing.T) {
	const n = 3
	var arrived int32
	barrier := make(chan struct{})
	var concurrent [n]bool

	tools := make([]Tool, n)
	for i := 0; i < n; i++ {
		i := i
		tools[i] = MustFunc(fmt.Sprintf("t%d", i), "", func(ctx context.Context, _ struct{}) (struct{}, error) {
			if atomic.AddInt32(&arrived, 1) == n {
				close(barrier) // last one in releases everyone
			}
			select {
			case <-barrier:
				concurrent[i] = true
			case <-time.After(3 * time.Second):
			}
			return struct{}{}, nil
		}, WithSafety(Safety{ReadOnly: true}))
	}

	m := &scriptModel{turns: [][]Emit{
		multiToolTurn([2]string{"c0", "t0"}, [2]string{"c1", "t1"}, [2]string{"c2", "t2"}),
		textTurn("done"),
	}}
	res, err := mustNew(m, memJournal(), WithTools(tools...)).Run(context.Background(), "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	out := res.Message
	if textOf(out) != "done" {
		t.Fatalf("answer = %q, want done", textOf(out))
	}
	for i := 0; i < n; i++ {
		if !concurrent[i] {
			t.Fatalf("tool %d never reached the barrier — tools did not run concurrently", i)
		}
	}
}

// Thread-safety: concurrent Do on DISTINCT keys (the parallel-tools shape) all record,
// with no race. Proves the store is safe for concurrent tool execution.
func TestMemStore_ConcurrentDistinctKeys(t *testing.T) {
	store := memJournal()
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = store.do(context.Background(), "run", fmt.Sprintf("s%d", i), func(context.Context) (Record, error) {
				return Record{Kind: StepValue}, nil
			})
		}()
	}
	wg.Wait()

	h, _ := store.History(context.Background(), "run")
	if len(h) != 101 { // the journal header, then the steps
		t.Fatalf("history len %d, want the header and 100 distinct steps", len(h))
	}
}

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
	charge := MustFunc("charge", "charge the card", func(context.Context, struct{}) (string, error) {
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
		var halt *OutcomeUnknown
		switch {
		case err == nil:
			oks++
		case errors.As(err, &halt) && halt.Op.ID == "c1":
			// The loser lost the claim while running: the winner may be running the charge now.
			if halt.Cause != HaltContended || halt.Op.Kind != OpTool {
				t.Fatalf("lost-claim halt = %+v, want an OpTool halt with Cause %q", halt, HaltContended)
			}
			// The winner may still be running the charge, so its halt cannot be resolved blind.
			if rerr := ResolveHalt(ctx, store.proc(), halt.Ref(), Outcome{Result: "ok"}); !errors.Is(rerr, ErrConfig) {
				t.Fatalf("ResolveHalt on a contended halt without WithMinHaltAge = %v, want ErrConfig", rerr)
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

// Concurrent Insert, Get and Load on one run: one winner per name, every reader sees the winner's
// exact bytes, Load yields a gap-free prefix in Seq order, positions follow real-time order, and
// mutating input or output buffers never reaches the store.
func TestRev138c_MemStoreHammer(t *testing.T) {
	ctx := context.Background()
	for round := 0; round < 20; round++ {
		m := NewMemStore()
		const writers, names, readers = 16, 64, 8
		var winners [names]atomic.Int32
		var winSeq [names]atomic.Int64
		var wg sync.WaitGroup
		var stop atomic.Bool
		var lastDone atomic.Int64 // largest Seq of an Insert that has returned
		lastDone.Store(-1)
		errs := make(chan string, 1024)
		report := func(f string, a ...any) {
			select {
			case errs <- fmt.Sprintf(f, a...):
			default:
			}
		}
		payload := func(w, n int) []byte { return bytes.Repeat([]byte{byte('a' + w)}, 64+n) }
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for n := 0; n < names; n++ {
					in := payload(w, n)
					floor := lastDone.Load()
					e, ins, err := m.Insert(ctx, "r", fmt.Sprint("n", n), in)
					for i := range in {
						in[i] = 'X' // the caller reuses its buffer
					}
					if err != nil {
						report("insert: %v", err)
						return
					}
					if ins {
						winners[n].Add(1)
						winSeq[n].Store(e.Seq)
						if e.Seq <= floor {
							report("A2: seq %d not after completed insert %d", e.Seq, floor)
						}
						for {
							c := lastDone.Load()
							if e.Seq <= c || lastDone.CompareAndSwap(c, e.Seq) {
								break
							}
						}
					}
					if len(e.Data) != 64+n || bytes.IndexByte(e.Data, 'X') >= 0 {
						report("insert returned torn data %q", e.Data)
					}
					for i := 1; i < len(e.Data); i++ {
						if e.Data[i] != e.Data[0] {
							report("insert returned mixed data")
							break
						}
					}
					for i := range e.Data {
						e.Data[i] = 'Y' // the caller scribbles on its result
					}
				}
			}(w)
		}
		for r := 0; r < readers; r++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for !stop.Load() {
					var prev int64 = -1
					for e, err := range m.Load(ctx, "r", -1) {
						if err != nil {
							report("load: %v", err)
							return
						}
						if e.Seq != prev+1 {
							report("load gap: %d after %d", e.Seq, prev)
						}
						prev = e.Seq
						if bytes.ContainsAny(e.Data, "XY") || len(e.Data) < 64 {
							report("load torn %q", e.Data)
						}
						g, ok, err := m.Get(ctx, "r", e.Name)
						if err != nil || !ok || g.Seq != e.Seq || !bytes.Equal(g.Data, e.Data) {
							report("get disagrees with load for %s", e.Name)
						}
						e.Data[0] = 'Y'
					}
				}
			}()
		}
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		// wait for writers only, then stop readers
		for {
			all := true
			for n := 0; n < names; n++ {
				if winners[n].Load() == 0 {
					all = false
				}
			}
			if all {
				break
			}
		}
		stop.Store(true)
		<-done
		close(errs)
		for e := range errs {
			t.Fatal(e)
		}
		for n := 0; n < names; n++ {
			if c := winners[n].Load(); c != 1 {
				t.Fatalf("name n%d won %d times", n, c)
			}
			g, ok, _ := m.Get(ctx, "r", fmt.Sprint("n", n))
			if !ok || g.Seq != winSeq[n].Load() || bytes.ContainsAny(g.Data, "XY") {
				t.Fatalf("final get n%d = %+v", n, g)
			}
		}
	}
}
