package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// ledger tracks side effects: which resources are currently "active" (side effect
// applied and not undone), how many times each was undone, and the undo order. The core
// saga invariant is: after an abort, NOTHING is active, and each undo ran at most once.
type ledger struct {
	mu        sync.Mutex
	active    map[string]bool
	undoCount map[string]int
	undoOrder []string
}

func newLedger() *ledger {
	return &ledger{active: map[string]bool{}, undoCount: map[string]int{}}
}

// write returns a compensated tool that marks `res` active on do and clears it on undo.
func (l *ledger) write(res string) Tool {
	return CompensatedFunc(res, "", Safety{},
		func(context.Context, struct{}) (struct{}, error) {
			l.mu.Lock()
			l.active[res] = true
			l.mu.Unlock()
			return struct{}{}, nil
		},
		func(context.Context, struct{}, struct{}) error {
			l.mu.Lock()
			l.active[res] = false
			l.undoCount[res]++
			l.undoOrder = append(l.undoOrder, res)
			l.mu.Unlock()
			return nil
		})
}

func (l *ledger) activeSet() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for k, v := range l.active {
		if v {
			out = append(out, k)
		}
	}
	return out
}

// assertClean is the invariant: nothing left active, and no compensator ran more than once.
func (l *ledger) assertClean(t *testing.T) {
	t.Helper()
	if a := l.activeSet(); len(a) != 0 {
		t.Fatalf("side effects still active after abort: %v", a)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for res, n := range l.undoCount {
		if n > 1 {
			t.Fatalf("compensator for %q ran %d times, want at most 1", res, n)
		}
	}
}

func failTool(name string) Tool {
	return Func(name, "", Safety{}, func(context.Context, struct{}) (struct{}, error) {
		return struct{}{}, errors.New(name + " failed")
	})
}

// Depth-3 tree: the deepest sub-agent fails → every level's writes reverse.
func TestSaga_DeepTreeFailureReverses(t *testing.T) {
	l := newLedger()
	store := NewMemStore()

	l3 := New(&scriptModel{turns: [][]Emit{toolTurn("d1", "C", `{}`), toolTurn("d2", "boom3", `{}`)}},
		store, l.write("C"), failTool("boom3"))
	l2 := New(&scriptModel{turns: [][]Emit{toolTurn("b1", "B", `{}`), toolTurn("b2", "l3", `{"task":"x"}`)}},
		store, l.write("B"), SubAgent("l3", "", l3))
	l1 := New(&scriptModel{turns: [][]Emit{toolTurn("a1", "A", `{}`), toolTurn("a2", "l2", `{"task":"x"}`)}},
		store, l.write("A"), SubAgent("l2", "", l2))

	_, err := l1.RunSaga(context.Background(), "root", "go")
	var ab *SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("err = %v, want *SagaAborted", err)
	}
	l.assertClean(t)
	for _, r := range []string{"A", "B", "C"} {
		if l.undoCount[r] != 1 {
			t.Errorf("undo %q = %d, want 1", r, l.undoCount[r])
		}
	}
}

// Sibling sub-agents: A succeeds, B fails → A's writes (via parent recursion) and B's
// writes (self-rollback) and the parent's write all reverse, each once.
func TestSaga_SiblingSubAgentsReverse(t *testing.T) {
	l := newLedger()
	store := NewMemStore()

	subA := New(&scriptModel{turns: [][]Emit{toolTurn("x1", "X", `{}`), textTurn("done")}}, store, l.write("X"))
	subB := New(&scriptModel{turns: [][]Emit{toolTurn("y1", "Y", `{}`), toolTurn("y2", "boomB", `{}`)}},
		store, l.write("Y"), failTool("boomB"))

	parent := New(&scriptModel{turns: [][]Emit{
		toolTurn("p0", "P", `{}`),
		toolTurn("p1", "subA", `{"task":"x"}`),
		toolTurn("p2", "subB", `{"task":"x"}`),
	}}, store, l.write("P"), SubAgent("subA", "", subA), SubAgent("subB", "", subB))

	_, err := parent.RunSaga(context.Background(), "root", "go")
	var ab *SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("err = %v, want *SagaAborted", err)
	}
	l.assertClean(t)
	for _, r := range []string{"P", "X", "Y"} {
		if l.undoCount[r] != 1 {
			t.Errorf("undo %q = %d, want 1", r, l.undoCount[r])
		}
	}
}

// flakyStore fails the first N compensation writes to simulate a crash DURING rollback.
type flakyStore struct {
	*MemStore
	mu                sync.Mutex
	failCompensations int
	seen              int
}

func (f *flakyStore) Do(ctx context.Context, runID, name string, fn func(context.Context) (Record, error)) (Record, error) {
	if strings.HasPrefix(name, "@saga/compensate/") {
		f.mu.Lock()
		f.seen++
		fail := f.seen <= f.failCompensations
		f.mu.Unlock()
		if fail {
			return Record{}, errors.New("crash during rollback")
		}
	}
	return f.MemStore.Do(ctx, runID, name, fn)
}

// Crash during rollback → resume → rollback completes, each compensator still runs once.
func TestSaga_CrashDuringRollbackResumes(t *testing.T) {
	l := newLedger()
	store := &flakyStore{MemStore: NewMemStore(), failCompensations: 1} // crash on the first compensation

	build := func() *Agent {
		return New(&scriptModel{turns: [][]Emit{
			toolTurn("c1", "A", `{}`),
			toolTurn("c2", "Bw", `{}`),
			toolTurn("c3", "boom", `{}`),
		}}, store, l.write("A"), l.write("Bw"), failTool("boom"))
	}

	// First attempt: the failing step trips the saga, then rollback itself crashes.
	_, err := build().RunSaga(context.Background(), "root", "go")
	var ab *SagaAborted
	if !errors.As(err, &ab) || ab.CompensateErr == nil {
		t.Fatalf("first attempt err = %v, want *SagaAborted with CompensateErr", err)
	}

	// Resume: a fresh agent on the SAME store, compensation no longer failing.
	store.mu.Lock()
	store.failCompensations = 0
	store.mu.Unlock()

	_, err = build().RunSaga(context.Background(), "root", "go")
	if !errors.As(err, &ab) {
		t.Fatalf("resume err = %v, want *SagaAborted", err)
	}
	l.assertClean(t) // nothing active, and — crucially — no compensator ran twice
	for _, r := range []string{"A", "Bw"} {
		if l.undoCount[r] != 1 {
			t.Errorf("undo %q = %d, want exactly 1 across crash+resume", r, l.undoCount[r])
		}
	}
}

// deepSagaReverses builds a `depth`-level nest of transactional agents — each level
// writes then delegates deeper; the deepest fails — and asserts the WHOLE tree reverses:
// every level's write undone exactly once, nothing left active.
func deepSagaReverses(t *testing.T, depth int) {
	l := newLedger()
	store := NewMemStore()

	var child *Agent
	for i := depth; i >= 1; i-- {
		res := fmt.Sprintf("w%d", i)
		if i == depth {
			child = New(&scriptModel{turns: [][]Emit{
				toolTurn("wr", res, `{}`),
				toolTurn("bm", "boom", `{}`),
			}}, store, l.write(res), failTool("boom"))
		} else {
			sub := SubAgent(fmt.Sprintf("sub%d", i+1), "", child)
			child = New(&scriptModel{turns: [][]Emit{
				toolTurn("wr", res, `{}`),
				toolTurn("dl", sub.Name(), `{"task":"x"}`),
			}}, store, l.write(res), sub)
		}
	}

	_, err := child.RunSaga(context.Background(), "root", "go")
	var ab *SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("err = %v, want *SagaAborted", err)
	}
	l.assertClean(t)
	for i := 1; i <= depth; i++ {
		res := fmt.Sprintf("w%d", i)
		if l.undoCount[res] != 1 {
			t.Errorf("level %d (%q): undo count %d, want 1", i, res, l.undoCount[res])
		}
	}
}

func TestSaga_50LevelsDeep(t *testing.T)  { deepSagaReverses(t, 50) }
func TestSaga_100LevelsDeep(t *testing.T) { deepSagaReverses(t, 100) }

// SCALING NOTE (measured, not assumed): correctness holds at every depth that completes,
// but memory is O(depth) LIVE HEAP with a large per-level constant, because a synchronous
// agent tree keeps the whole ancestor chain alive (each parent is blocked waiting on its
// child, holding its conversation + maps). Measured: 50 <10ms; 500 ~4s; 2500 ~7.4GB.
// Running each sub-agent on its own goroutine did NOT fix this (confirmed: still 7.4GB at
// 2500) — the cost is heap live-set, not stack depth. Fine for real trees (single-digit
// depth; 100 here is far beyond realistic). A real fix would need heap profiling to shrink
// the per-level footprint; not worth it until a workload needs deep trees. See
// docs/KNOWN-LIMITATIONS.md.

// A completed write with no compensator inside the tree is surfaced, not silently lost.
func TestSaga_UncompensatedWriteSurfaced(t *testing.T) {
	l := newLedger()
	store := NewMemStore()

	// "danger" is a real write (Safety{}) with NO compensator.
	danger := Func("danger", "", Safety{}, func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil })

	a := New(&scriptModel{turns: [][]Emit{
		toolTurn("c1", "A", `{}`),
		toolTurn("c2", "danger", `{}`),
		toolTurn("c3", "boom", `{}`),
	}}, store, l.write("A"), danger, failTool("boom"))

	_, err := a.RunSaga(context.Background(), "root", "go")
	var ab *SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("err = %v, want *SagaAborted", err)
	}
	found := false
	for _, u := range ab.Uncompensated {
		if u == "danger" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Uncompensated = %v, want it to include \"danger\"", ab.Uncompensated)
	}
}
