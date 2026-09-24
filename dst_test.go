package agent

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"sync"
	"testing"
)

// Deterministic Simulation Testing of the core guarantee: a NON-IDEMPOTENT side effect
// never fires twice across a crash, under any crash schedule. This is the moat, proven
// adversarially rather than asserted.
//
// The model here is resume-safe (its response is a pure function of the conversation, so
// re-calling it after a crash returns the same turn — exactly how a replayable model
// behaves). The store can be told to "crash" (fail to persist) at the Kth write, and the
// driver resumes by re-running the same runID against the same store. The invariant:
// charge() executes at most once, and the run always ends completed or in *ResumeHalt.

// crashStore fails to persist the crashAt-th write (0 = never), simulating a process
// crash at that point: the record is not recorded and the run unwinds. On resume a fresh
// crashStore with crashAt=0 lets the run finish.
type crashStore struct {
	inner   Durable
	mu      sync.Mutex
	writes  int
	crashAt int
}

func (c *crashStore) Do(ctx context.Context, runID, name string, fn func(context.Context) (Record, error)) (Record, error) {
	return c.inner.Do(ctx, runID, name, func(ctx context.Context) (Record, error) {
		rec, err := fn(ctx) // the real work (incl. any side effect) runs here
		if err != nil {
			return rec, err
		}
		c.mu.Lock()
		c.writes++
		w := c.writes
		c.mu.Unlock()
		if c.crashAt > 0 && w == c.crashAt {
			return Record{}, errCrash // crash: record is NOT persisted
		}
		return rec, nil
	})
}

func (c *crashStore) History(ctx context.Context, runID string) ([]Record, error) {
	return c.inner.History(ctx, runID)
}

// dstModel: call charge until there's a tool result in the conversation, then answer.
// Deterministic on the messages → re-calling after a crash returns the same turn.
type dstModel struct{}

func (dstModel) Stream(_ context.Context, req Request) (*Stream, error) {
	answered := false
	for _, m := range req.Messages {
		if m.Role == RoleTool {
			answered = true
		}
	}
	emits := toolTurn("c1", "charge", `{}`)
	if answered {
		emits = textTurn("done")
	}
	ch := make(chan Emit, len(emits))
	for _, e := range emits {
		ch <- e
	}
	close(ch)
	return NewStream(ch), nil
}

// chargeTool is a NON-idempotent side effect (Safety{}): it must never run twice.
type chargeTool struct{ count *int }

func (chargeTool) Name() string                 { return "charge" }
func (chargeTool) Description() string           { return "" }
func (chargeTool) Safety() Safety                { return Safety{} }
func (chargeTool) ArgsSchema() json.RawMessage   { return nil }
func (t chargeTool) Call(context.Context, json.RawMessage) (json.RawMessage, error) {
	*t.count++ // the real-world side effect (the "charge")
	return json.RawMessage(`{"charged":true}`), nil
}

func runOnce(mem Durable, tool chargeTool, crashAt int) error {
	a := New(dstModel{}, &crashStore{inner: mem, crashAt: crashAt}, tool).SetMaxConcurrency(1)
	_, err := a.Run(context.Background(), "dst", "charge me")
	return err
}

// Sweep a crash at every write point; the charge must fire at most once each time, and
// the run must end completed or in ResumeHalt. At least one point must exercise the halt
// path (crash while persisting the tool result), or the test would be vacuous.
func TestDST_NoDoubleFire_CrashSweep(t *testing.T) {
	haltSeen := false
	for crashAt := 1; crashAt <= 32; crashAt++ {
		var count int
		mem := NewMemStore()
		tool := chargeTool{count: &count}

		err := runOnce(mem, tool, crashAt)
		crashed := errors.Is(err, errCrash)
		for errors.Is(err, errCrash) { // resume without further crashes
			err = runOnce(mem, tool, 0)
		}

		if count > 1 {
			t.Fatalf("crashAt=%d: charge fired %d times — DOUBLE FIRE", crashAt, count)
		}
		var halt *ResumeHalt
		switch {
		case err == nil:
			if count != 1 {
				t.Fatalf("crashAt=%d: completed run charged %d times, want 1", crashAt, count)
			}
		case errors.As(err, &halt):
			haltSeen = true // effect fired but outcome unknown → halted, not retried
		default:
			t.Fatalf("crashAt=%d: unexpected terminal error: %v", crashAt, err)
		}

		if !crashed { // crashAt exceeded the clean-run write count → sweep complete
			break
		}
	}
	if !haltSeen {
		t.Fatal("no crash point exercised ResumeHalt — the halt path was never tested")
	}
}

// Adversarial: throw randomized multi-crash schedules at the loop; charge still fires at
// most once across all crash+resume attempts.
func TestDST_NoDoubleFire_Randomized(t *testing.T) {
	for seed := uint64(1); seed <= 500; seed++ {
		rng := rand.New(rand.NewPCG(seed, 0x9E3779B97F4A7C15))
		var count int
		mem := NewMemStore()
		tool := chargeTool{count: &count}

		var err error
		for attempt := 0; attempt < 50; attempt++ {
			err = runOnce(mem, tool, rng.IntN(8)+1) // crash at a random write (or beyond → no crash)
			if count > 1 {
				t.Fatalf("seed=%d attempt=%d: DOUBLE FIRE (count=%d)", seed, attempt, count)
			}
			if !errors.Is(err, errCrash) {
				break // terminal
			}
		}
		var halt *ResumeHalt
		if !errors.Is(err, errCrash) && err != nil && !errors.As(err, &halt) {
			t.Fatalf("seed=%d: unexpected terminal error: %v", seed, err)
		}
	}
}
