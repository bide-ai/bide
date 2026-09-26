package plan

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	agent "github.com/dayna/go-agents"
)

// Deterministic Simulation Testing of the substrate invariant as it reaches the
// plan surface: a flow lowers to journaled steps, so a NON-IDEMPOTENT side effect
// journaled as its own durable step fires AT MOST ONCE across a crash and resume,
// and a plain re-run with the same runID reuses the journaled steps rather than
// re-executing them. This mirrors the core dst_test crash-sweep, proving plan
// inherits at-most-once by lowering to store.Do rather than by adding an executor.

// errCrash is the DST sentinel a crashFlowStore returns instead of persisting the
// crash-point write, simulating a process crash between a step's side effect and
// the durable record of it.
var errCrash = errors.New("simulated crash")

// crashFlowStore fails to persist the crashAt-th write (0 = never), simulating a
// crash at that write: the record is not recorded and the run unwinds with
// errCrash. A resume drives the same runID against the same underlying journal
// with crashAt = 0 so the run can finish. It mirrors the core dst_test crashStore.
type crashFlowStore struct {
	inner   agent.Durable
	mu      sync.Mutex
	writes  int
	crashAt int
}

func (c *crashFlowStore) Do(ctx context.Context, runID, name string, fn func(context.Context) (agent.Record, error)) (agent.Record, error) {
	return c.inner.Do(ctx, runID, name, func(ctx context.Context) (agent.Record, error) {
		rec, err := fn(ctx) // the real work (incl. any side effect) runs here
		if err != nil {
			return rec, err
		}
		c.mu.Lock()
		c.writes++
		w := c.writes
		c.mu.Unlock()
		if c.crashAt > 0 && w == c.crashAt {
			return agent.Record{}, errCrash // crash: record is NOT persisted
		}
		return rec, nil
	})
}

func (c *crashFlowStore) History(ctx context.Context, runID string) ([]agent.Record, error) {
	return c.inner.History(ctx, runID)
}

// errChargeHalt is returned when the guarded effect finds a prior attempt whose
// outcome is unknown (a crash between the effect and the record of it). Like the
// core's ResumeHalt, the effect stops rather than risk a double fire; the run
// ends halted, not re-fired.
var errChargeHalt = errors.New("charge halted: prior attempt outcome unknown")

// guardedCounter is a non-idempotent side effect (an increment) journaled with
// the same two-phase attempt/result protocol the core loop uses for a
// non-idempotent tool (store.go / agent.go): record an attempt marker BEFORE the
// effect, perform the effect, then record the result. On resume, if the attempt
// marker is present but the result is not, the outcome is unknown and the effect
// HALTS rather than re-firing. This is the documented pattern for a non-idempotent
// effect inside a plan Step (declare its own durable steps; see the Builder.Step
// doc comment and docs/design/expression-surfaces.md). It uses only the existing
// substrate primitives (store.Do, History): plan adds no new durability model.
type guardedCounter struct {
	store agent.Durable
	runID string
	count *int
}

// bump increments the counter at most once for runID, or halts if a prior attempt
// crashed with an unknown outcome. The Builder.Step body captures this so the flow
// lowers the effect to the same journal it runs on.
func (g guardedCounter) bump(int) (int, error) {
	ctx := context.Background()

	// Resume gate: an attempt recorded with no result means the effect may have
	// fired but its outcome was lost to a crash. Halt rather than re-fire.
	recs, err := g.store.History(ctx, g.runID)
	if err != nil {
		return 0, err
	}
	var attempted, resulted bool
	var resultVal int
	for _, r := range recs {
		switch r.Name {
		case "charge:attempt":
			attempted = true
		case "charge:result":
			resulted = true
			if len(r.Result) > 0 {
				_ = json.Unmarshal(r.Result, &resultVal)
			}
		}
	}
	if resulted {
		return resultVal, nil // already done: replay the recorded result
	}
	if attempted {
		return 0, errChargeHalt // effect started, outcome unknown → halt
	}

	// Record the attempt marker BEFORE the effect, so a crash between the effect
	// and its result is detectable on resume (the marker persists, the result does
	// not) and triggers the halt above rather than a silent double fire.
	if _, err := g.store.Do(ctx, g.runID, "charge:attempt", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue}, nil
	}); err != nil {
		return 0, err
	}

	// Perform the effect, then record its result. store.Do memoizes the result by
	// name, so once persisted a resume replays it (the resulted branch above).
	return agent.Step(ctx, g.store, g.runID, "charge:result", func(context.Context) (int, error) {
		*g.count++ // the real-world side effect
		return *g.count, nil
	})
}

// buildCounterFlow builds a flow whose entry performs the guarded non-idempotent
// increment, then routes through a Switch to one of two terminals producing the
// string result. The Switch choice is journaled as its own step, so a resume
// replays the recorded branch.
func buildCounterFlow(g guardedCounter) (*Flow[int, string], error) {
	b := New[int, string]("charge-flow")
	entry := b.Step("entry", g.bump)
	hi := b.Step("high", func(int) (string, error) { return "high", nil })
	lo := b.Step("low", func(int) (string, error) { return "low", nil })
	b.Switch(entry,
		When(func(n int) bool { return n >= 1 }, hi),
		Else(lo),
	)
	return b.Build()
}

// runFlowOnce drives the flow once against a crashFlowStore wrapping mem, crashing
// at the crashAt-th write. The guardedCounter is bound to mem (the durable
// journal) directly so its guard step survives the crash wrapper exactly as the
// flow's own steps do.
func runFlowOnce(mem agent.Durable, count *int, crashAt int) error {
	store := &crashFlowStore{inner: mem, crashAt: crashAt}
	g := guardedCounter{store: store, runID: "dst", count: count}
	flow, err := buildCounterFlow(g)
	if err != nil {
		return err
	}
	_, err = flow.Run(context.Background(), store, "dst", 0)
	return err
}

// TestDST_Flow_NoDoubleFire_CrashSweep sweeps a crash at every write point of the
// lowered flow. The guarded increment must fire AT MOST ONCE across crash+resume,
// and the run must end completed (or unwinding with the crash sentinel until a
// clean resume finishes it). At least one crash point must land on the effect's
// own write so the sweep is not vacuous.
func TestDST_Flow_NoDoubleFire_CrashSweep(t *testing.T) {
	crashed := false
	haltSeen := false
	for crashAt := 1; crashAt <= 32; crashAt++ {
		var count int
		mem := agent.NewMemStore()

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
		switch {
		case err == nil:
			// Completed: the effect fired exactly once and the flow produced Out.
			if count != 1 {
				t.Fatalf("crashAt=%d: completed run fired the effect %d times, want exactly 1", crashAt, count)
			}
		case errors.Is(err, errChargeHalt):
			// Halted: the crash landed between the effect and its record, so the
			// outcome is unknown and the guard halts rather than re-firing. The effect
			// fired at most once (asserted above); the run ends halted, not double-fired.
			haltSeen = true
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
		t.Fatal("no crash point exercised the halt path: the effect-vs-record crash was never tested")
	}
}

// TestDST_Flow_ResumeReusesJournaledSteps asserts a plain re-run with the same
// runID (no crash) reuses the journaled steps rather than re-executing them: the
// guarded effect fires exactly once across two full runs, and the second run
// returns the same output by replay.
func TestDST_Flow_ResumeReusesJournaledSteps(t *testing.T) {
	var count int
	mem := agent.NewMemStore()
	g := guardedCounter{store: mem, runID: "reuse", count: &count}
	flow, err := buildCounterFlow(g)
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
		t.Fatalf("output = %q, want %q (input 0 -> guarded count 1 -> When n>=1 -> high)", first, "high")
	}
}

// TestDST_Flow_SwitchChoiceReplayed asserts the Switch choice is journaled as its
// own step and replayed on resume: the recorded choice ("switch:entry") is present
// after a run, so a resumed run replays the recorded branch rather than
// re-evaluating the predicate.
func TestDST_Flow_SwitchChoiceReplayed(t *testing.T) {
	var count int
	mem := agent.NewMemStore()
	g := guardedCounter{store: mem, runID: "switch", count: &count}
	flow, err := buildCounterFlow(g)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := flow.Run(context.Background(), mem, "switch", 0); err != nil {
		t.Fatalf("Run: %v", err)
	}

	recs, err := mem.History(context.Background(), "switch")
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	var sawChoice, sawTaken bool
	for _, r := range recs {
		if r.Name == "switch:entry" {
			sawChoice = true
		}
		if r.Name == "high" { // the taken arm (count 1 satisfies n>=1)
			sawTaken = true
		}
		if r.Name == "low" {
			t.Errorf("the untaken arm %q was executed: only the taken arm should run", r.Name)
		}
	}
	if !sawChoice {
		t.Error("no journaled Switch choice step \"switch:entry\": the branch choice must be its own durable step")
	}
	if !sawTaken {
		t.Error("the taken arm was not journaled")
	}
}
