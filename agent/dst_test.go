package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
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

// crashStore fails the crashAt-th Insert that would store a new entry (0 = never), simulating a
// process crash at that point: the entry is not stored, and the process is dead from then on, so
// every later call fails too and the run unwinds. It works at the storage port, under a Journal
// (see crashJournal), so a crash can land between any two store round trips the engine makes: the
// journal header, a claim, a not-started record, a result. On resume a fresh journal over the
// same store (a new process) lets the run finish.
type crashStore struct {
	inner   Store
	mu      sync.Mutex
	writes  int
	crashAt int
	crashed bool
}

func (c *crashStore) dead() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.crashed
}

func (c *crashStore) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	if c.dead() {
		return Entry{}, false, errCrash
	}
	if e, ok, err := c.inner.Get(ctx, runID, name); err != nil || ok {
		return e, false, err // stores nothing: not a write
	}
	c.mu.Lock()
	c.writes++
	crash := c.crashAt > 0 && c.writes == c.crashAt
	if crash {
		c.crashed = true
	}
	c.mu.Unlock()
	if crash {
		return Entry{}, false, errCrash // crash: the entry is NOT stored
	}
	return c.inner.Insert(ctx, runID, name, data)
}

func (c *crashStore) Get(ctx context.Context, runID, name string) (Entry, bool, error) {
	if c.dead() {
		return Entry{}, false, errCrash
	}
	return c.inner.Get(ctx, runID, name)
}

func (c *crashStore) Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error] {
	if c.dead() {
		return func(yield func(Entry, error) bool) { yield(Entry{}, errCrash) }
	}
	return c.inner.Load(ctx, runID, after)
}

// crashJournal returns a Journal, a new process, over mem's store behind a crashStore that crashes
// at its crashAt-th write.
func crashJournal(mem Durable, crashAt int) Durable {
	return newJournal(&crashStore{inner: mem.(Store), crashAt: crashAt})
}

// dstModel: call charge until there's a tool result in the conversation, then answer.
// Deterministic on the messages → re-calling after a crash returns the same turn.
//
// Because it answers the same way twice, a model call made after the final answer was
// recorded would go unnoticed by the side-effect count alone; afterAnswer counts those calls
// (a request that already holds an assistant turn with no tool calls), which a real model
// could answer differently or with new tool calls.
type dstModel struct{ afterAnswer *int }

func (m dstModel) Stream(_ context.Context, req Request) (*Stream, error) {
	answered := false
	for _, msg := range req.Messages {
		if msg.Role == RoleTool {
			answered = true
		}
		if msg.Role == RoleAssistant && len(msg.toolUses()) == 0 {
			*m.afterAnswer++
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

func (chargeTool) Name() string                { return "charge" }
func (chargeTool) Description() string         { return "" }
func (chargeTool) Safety() Safety              { return Safety{} }
func (chargeTool) ArgsSchema() json.RawMessage { return nil }
func (t chargeTool) Call(context.Context, json.RawMessage) (json.RawMessage, error) {
	*t.count++ // the real-world side effect (the "charge")
	return json.RawMessage(`{"charged":true}`), nil
}

func runOnce(mem Durable, tool chargeTool, afterAnswer *int, crashAt int) error {
	a := New(dstModel{afterAnswer: afterAnswer}, crashJournal(mem, crashAt), tool).SetMaxConcurrency(1)
	out, err := a.Run(context.Background(), "dst", "charge me")
	if err == nil && textOf(out) != "done" {
		return fmt.Errorf("completed run answered %q, want done", textOf(out))
	}
	return err
}

// wantFinished checks the invariants of a run that ended: no model call after its final
// answer was recorded, and a completed run is marked complete.
func wantFinished(t *testing.T, mem Durable, err error, afterAnswer int, schedule string) {
	t.Helper()
	if afterAnswer != 0 {
		t.Fatalf("%s: the model was called %d times after the final answer was recorded", schedule, afterAnswer)
	}
	if err == nil {
		if done, ierr := IsComplete(context.Background(), mem, "dst"); ierr != nil || !done {
			t.Fatalf("%s: completed run not marked complete (%v, %v)", schedule, done, ierr)
		}
	}
}

// Sweep a crash at every write point; the charge must fire at most once each time, and
// the run must end completed or in ResumeHalt. At least one point must exercise the halt
// path (crash while persisting the tool result), or the test would be vacuous.
func TestDST_NoDoubleFire_CrashSweep(t *testing.T) {
	haltSeen := false
	for crashAt := 1; crashAt <= 32; crashAt++ {
		var count, afterAnswer int
		mem := NewMemStore()
		tool := chargeTool{count: &count}

		err := runOnce(mem, tool, &afterAnswer, crashAt)
		crashed := errors.Is(err, errCrash)
		for errors.Is(err, errCrash) { // resume without further crashes
			err = runOnce(mem, tool, &afterAnswer, 0)
		}
		wantFinished(t, mem, err, afterAnswer, fmt.Sprintf("crashAt=%d", crashAt))

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
		var count, afterAnswer int
		mem := NewMemStore()
		tool := chargeTool{count: &count}

		var err error
		for attempt := 0; attempt < 50; attempt++ {
			err = runOnce(mem, tool, &afterAnswer, rng.IntN(8)+1) // crash at a random write (or beyond → no crash)
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
		if !errors.Is(err, errCrash) {
			wantFinished(t, mem, err, afterAnswer, fmt.Sprintf("seed=%d", seed))
		}
	}
}
