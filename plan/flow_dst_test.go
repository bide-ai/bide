package plan

import (
	"context"
	"errors"
	"iter"
	"sync"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// Deterministic Simulation Testing of the substrate invariant as it reaches the
// plan surface: Flow.Run runs every node as an agent.Step, so a NON-IDEMPOTENT side
// effect written as a plain step body fires AT MOST ONCE across a crash and resume,
// with NO per-step opt-in. The step here is a bare increment; the guard is the Step's
// attempt claim, not the step body's. This mirrors the core dst_test crash-sweep,
// proving plan inherits at-most-once and halt-on-ambiguity by lowering onto Step
// rather than by adding an executor.

// errCrash is the DST sentinel a crashFlowStore returns instead of persisting the
// crash-point write, simulating a process crash between a step's side effect and
// the durable record of it.
var errCrash = errors.New("simulated crash")

// crashFlowStore is a store wrapper that fails to persist the crashAt-th write
// (0 = never) of a new entry, the journal header aside, simulating a crash at that
// write: the entry is not stored and the run unwinds with errCrash. A step's write
// comes after its fn ran, so a crash on a result's write is a crash between the
// step's side effect and the durable record of it. A resume drives the same runID
// against the same underlying store with crashAt = 0 so the run can finish. Each
// wrapper is its own process: it does not Unwrap, so a Journal over it shares no
// in-process state (flights, remembered claims) with a Journal over another. It
// mirrors the core dst_test crashStore.
type crashFlowStore struct {
	inner   *agent.Journal // the journal whose store the crash wraps, shared across a crash and its resume
	mu      sync.Mutex
	writes  int
	crashAt int
}

func (c *crashFlowStore) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	s := c.inner.Store()
	if name != "@journal" { // the journal header (see agent.Journal)
		if _, ok, err := s.Get(ctx, runID, name); err != nil || ok {
			return s.Insert(ctx, runID, name, data) // stored already: not a write
		}
		c.mu.Lock()
		c.writes++
		w := c.writes
		c.mu.Unlock()
		if c.crashAt > 0 && w == c.crashAt {
			return agent.Entry{}, false, errCrash // crash: the entry is NOT persisted
		}
	}
	return s.Insert(ctx, runID, name, data)
}

func (c *crashFlowStore) Get(ctx context.Context, runID, name string) (agent.Entry, bool, error) {
	return c.inner.Store().Get(ctx, runID, name)
}

func (c *crashFlowStore) Load(ctx context.Context, runID string, after int64) iter.Seq2[agent.Entry, error] {
	return c.inner.Store().Load(ctx, runID, after)
}

// bumpCounter is a plain, NON-IDEMPOTENT step body: it increments a shared counter
// and returns the new value. It declares no Safety and wraps no durable primitive
// of its own; the at-most-once guarantee comes entirely from Flow.Run's automatic
// attempt/result guard. If the guard were absent, a crash between the increment
// and its recorded result would re-fire the increment on resume.
func bumpCounter(count *int) func(context.Context, int) (int, error) {
	return func(context.Context, int) (int, error) {
		*count++ // the real-world side effect, fired inside a plain step body
		return *count, nil
	}
}

// buildCounterFlow builds a flow whose entry performs the plain non-idempotent
// increment, then routes through a Switch to one of two terminals producing the
// string result. The Switch choice is journaled as its own step, so a resume
// replays the recorded branch. No step declares any Safety; Run provides the guard.
func buildCounterFlow(count *int) (*Flow[int, string], error) {
	b := New[int, string]("charge-flow")
	entry := b.Step("entry", bumpCounter(count))
	hi := b.Step("high", func(context.Context, int) (string, error) { return "high", nil })
	lo := b.Step("low", func(context.Context, int) (string, error) { return "low", nil })
	b.Switch(entry,
		When(func(n int) bool { return n >= 1 }, hi),
		Else(lo),
	)
	return b.Build()
}

// runFlowOnce drives the flow once against a crashFlowStore wrapping mem, crashing
// at the crashAt-th write. mem is the durable journal shared across a crash and its
// resume, so the automatic guard's attempt markers and results survive the crash
// wrapper.
func runFlowOnce(mem *agent.Journal, count *int, crashAt int) error {
	store := agenttest.MustJournal(&crashFlowStore{inner: mem, crashAt: crashAt})
	flow, err := buildCounterFlow(count)
	if err != nil {
		return err
	}
	_, err = flow.Run(context.Background(), store, "dst", 0)
	return err
}

// TestDST_Flow_NoDoubleFire_CrashSweep sweeps a crash at every write point of the
// lowered flow. The plain non-idempotent increment must fire AT MOST ONCE across
// crash+resume purely because Run guards it, and the run must end completed (the
// effect fired once and the flow produced Out) or HALTED (the crash landed between
// the effect and its result, so Run returns *agent.OutcomeUnknown rather than re-firing).
// At least one crash point must land on the effect's own result write so the halt
// path is exercised and the sweep is not vacuous.
func TestDST_Flow_NoDoubleFire_CrashSweep(t *testing.T) {
	crashed := false
	haltSeen := false
	for crashAt := 1; crashAt <= 32; crashAt++ {
		var count int
		mem := agenttest.MemJournal()

		err := runFlowOnce(mem, &count, crashAt)
		didCrash := errors.Is(err, errCrash)
		if didCrash {
			crashed = true
		}
		for errors.Is(err, errCrash) { // resume without further crashes
			err = runFlowOnce(mem, &count, 0)
		}

		if count > 1 {
			t.Fatalf("crashAt=%d: guarded effect fired %d times, want at most once (DOUBLE FIRE)", crashAt, count)
		}
		var halt *agent.OutcomeUnknown
		switch {
		case err == nil:
			// Completed: the effect fired exactly once and the flow produced Out.
			if count != 1 {
				t.Fatalf("crashAt=%d: completed run fired the effect %d times, want exactly 1", crashAt, count)
			}
		case errors.As(err, &halt):
			// Halted: Run detected an attempt with no result and refused to re-fire.
			// The effect fired at most once (asserted above); the run halted rather
			// than double-firing. The guard is automatic for EVERY node, so the halt
			// can name any node whose result write was lost to the crash; it must name
			// some declared node, as a Step.
			haltSeen = true
			if n, ok := nodeOfKey(halt.Op.ID); !ok || n == "" || halt.Op.Kind != agent.OpStep || halt.RootRunID != "dst" {
				t.Fatalf("crashAt=%d: halt names %+v in run %q, want a node's Step in run dst", crashAt, halt.Op, halt.RootRunID)
			}
		default:
			t.Fatalf("crashAt=%d: unexpected terminal error: %v", crashAt, err)
		}

		if !didCrash { // crashAt exceeded the clean-run write count → sweep complete
			break
		}
	}
	if !crashed {
		t.Fatal("no crash point was exercised: the sweep was vacuous")
	}
	if !haltSeen {
		t.Fatal("no crash point exercised the halt path: the effect-vs-result crash was never tested")
	}
}

// TestDST_Flow_ResumeReusesJournaledSteps asserts a plain re-run with the same
// runID (no crash) reuses the journaled steps rather than re-executing them: the
// non-idempotent effect fires exactly once across two full runs, and the second
// run returns the same output by replay. The step body carries no guard of its
// own; Run's memoized attempt/result records provide the reuse.
func TestDST_Flow_ResumeReusesJournaledSteps(t *testing.T) {
	var count int
	mem := agenttest.MemJournal()
	flow, err := buildCounterFlow(&count)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	first, err := flow.Run(context.Background(), mem, "reuse", 0)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if count != 1 {
		t.Fatalf("after first run effect fired %d times, want 1", count)
	}

	second, err := flow.Run(context.Background(), mem, "reuse", 0)
	if err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	if count != 1 {
		t.Fatalf("resume re-ran the effect: fired %d times, want 1 (steps must be reused)", count)
	}
	if first != second {
		t.Fatalf("resume returned %q, want the replayed %q", second, first)
	}
	if first != "high" {
		t.Fatalf("output = %q, want %q (input 0 -> count 1 -> When n>=1 -> high)", first, "high")
	}
}

// TestDST_Flow_JournalRecordScheme asserts the exact journal-record name scheme
// Run writes: the run's start, the topology digest, a Step attempt marker
// "attempt:step:node:<name>" and a result "node:<name>" per node, and a Switch choice
// "switch:<over>". Only the taken arm runs. Conformance relies on this scheme
// (attempt markers are internal records of their node, not divergences).
func TestDST_Flow_JournalRecordScheme(t *testing.T) {
	var count int
	mem := agenttest.MemJournal()
	flow, err := buildCounterFlow(&count)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := flow.Run(context.Background(), mem, "scheme", 0); err != nil {
		t.Fatalf("Run: %v", err)
	}

	recs, err := mem.History(context.Background(), "scheme")
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	names := map[string]bool{}
	for _, r := range recs {
		names[r.Name] = true
	}

	// Attempt marker + result for each executed node.
	for _, want := range []string{"run:start", "flow:digest", "attempt:step:node:entry", "node:entry", "attempt:step:node:high", "node:high"} {
		if !names[want] {
			t.Errorf("journal missing expected record %q", want)
		}
	}
	// Switch choice for the branch over "entry".
	if !names["switch:entry"] {
		t.Errorf("journal missing Switch choice record %q", "switch:entry")
	}
	// The untaken arm "low" must not have run (neither its attempt nor its result).
	if names["node:low"] || names["attempt:step:node:low"] {
		t.Errorf("untaken arm %q was executed: only the taken arm should run", "low")
	}
}

// TestDST_Flow_HaltThenResolveCompletes documents the halt-then-resolve path: after
// Run halts on an ambiguous crash, recording the missing result out of band lets a
// subsequent Run complete by replay without re-firing the effect. This shows the
// halt is a durable pause, not a dead end.
func TestDST_Flow_HaltThenResolveCompletes(t *testing.T) {
	// Find a crash point on the ENTRY result write (increment fired, result lost,
	// attempt marker persisted), so the halt names "entry" and count is 1 at halt.
	// Run records flow:digest first, then the entry attempt, then the entry result,
	// so the crash point is a few writes in; find it robustly rather than hard-coding
	// the count.
	var count int
	var mem *agent.Journal
	var halt *agent.OutcomeUnknown
	for crashAt := 1; crashAt <= 32; crashAt++ {
		count = 0
		mem = agenttest.MemJournal()
		err := runFlowOnce(mem, &count, crashAt)
		if !errors.Is(err, errCrash) {
			continue
		}
		err = runFlowOnce(mem, &count, 0) // resume with no further crash
		if errors.As(err, &halt) && halt.Op.ID == "node:entry" {
			break
		}
		halt = nil
	}
	if halt == nil {
		t.Fatal("no crash point produced a OutcomeUnknown on the entry step to resolve")
	}
	if count != 1 {
		t.Fatalf("at halt the effect fired %d times, want exactly 1", count)
	}

	// Resolve out of band with ResolveHaltRef, as an operator confirming the effect
	// landed would: it records the node's output. Then a fresh Run completes by replay
	// without re-firing the increment (count stays 1).
	if err := agent.ResolveHalt(context.Background(), mem, halt.Ref(), agent.Outcome{Result: 1}); err != nil {
		t.Fatalf("ResolveHaltRef: %v", err)
	}

	flow, err := buildCounterFlow(&count)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	out, err := flow.Run(context.Background(), mem, "dst", 0)
	if err != nil {
		t.Fatalf("post-resolve Run: %v", err)
	}
	if count != 1 {
		t.Fatalf("post-resolve run re-fired the effect: count = %d, want 1", count)
	}
	if out != "high" {
		t.Fatalf("post-resolve output = %q, want %q", out, "high")
	}
}
