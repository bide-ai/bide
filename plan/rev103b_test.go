package plan

// Adversarial re-review of PR #103's review-fix commits (5e2c1c8..55ec5c0), merged over #92 head
// 2c8d2db. Copy into plan/ to run. Every test here states the property the fix claims and fails
// while the hole is open.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// F2: "a later drive ... with an input that differs as canonical JSON ... is ErrConfig". An int64
// flow input beyond 2^53 is a different input, and the flow body sees a different value, but the
// canonical form rounds both to one double, so the second drive is held to the first run and
// handed its output.
func TestRev103b_F2_Int64InputsBeyond2p53AreOneInput(t *testing.T) {
	ctx := context.Background()
	b := New[int64, int64]("ids")
	b.Step("echo", func(_ context.Context, n int64) (int64, error) { return n, nil }, ReadOnly())
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	mem := agent.NewMemStore()
	const first, second int64 = 9007199254740993, 9007199254740992 // 2^53+1, 2^53
	if out, err := flow.Run(ctx, mem, "r", first); err != nil || out != first {
		t.Fatalf("first drive: %d, %v", out, err)
	}
	out, err := flow.Run(ctx, mem, "r", second)
	if !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("a drive with input %d of a run started with %d: got output %d, err %v; want ErrConfig", second, first, out, err)
	}
}

// F4: "ref must name a node of this flow ... so the next Run can feed it downstream". It checks
// the node against the flow it is called on, never that the run is a run of that flow: another
// flow with a node of the same name records a value of its own node's type, and the halted flow
// then cannot decode it on any drive.
func TestRev103b_F4_ResolveHaltThroughAnotherFlow(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	var fired int
	flow := effectFlow(t, &fired) // "charge-flow": charge is int -> int
	_, _ = flow.Run(ctx, mem, "r", 5)
	_, err := flow.Run(ctx, mem, "r", 5)
	halt, ok := errors.AsType[*agent.OutcomeUnknown](err)
	if !ok {
		t.Fatalf("want a halt: %v", err)
	}
	ob := New[int, string]("other")
	ob.Step("charge", func(_ context.Context, n int) (string, error) { return "", nil })
	other, err := ob.Build()
	if err != nil {
		t.Fatal(err)
	}
	rerr := other.ResolveHalt(ctx, mem, halt.Ref(), agent.Outcome{Result: "a receipt string"})
	_, runErr := flow.Run(ctx, mem, "r", 5)
	if !errors.Is(rerr, agent.ErrConfig) {
		t.Fatalf("flow %q resolved a halt of a run of flow %q: err %v; the halted flow's next drive: %v", "other", "charge-flow", rerr, runErr)
	}
}

// F7: a node's body may run agent.Step with any name agent.Step accepts outside a flow, "" among
// them. Inside a node it was recorded as "node:<node>:step:", which neither ResolveHaltRef nor
// Flow.ResolveHalt nor Conform accepts: a completed run of the declared graph did not conform.
// agent.Step now refuses an empty name everywhere (ErrConfig), so the drive fails before the
// Step's body runs and records no such key, and the journal conforms.
func TestRev103b_F7_EmptyStepNameInsideNodeDoesNotConform(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	b := New[int, int]("nested-empty")
	b.Step("a", func(ctx context.Context, n int) (int, error) {
		return agent.Step(ctx, mem, "r", "", func(context.Context) (int, error) { return n + 1, nil })
	}, ReadOnly())
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := flow.Run(ctx, mem, "r", 1); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("drive: %v, want the empty Step name refused with ErrConfig", err)
	}
	if ok, diffs, err := flow.Conform(ctx, mem, "r"); err != nil || !ok {
		t.Fatalf("a completed run of the declared graph: Conform = %v, %q, %v", ok, diffs, err)
	}
}

// F7: the same empty-named Step halting inside a node left a halt nothing could resolve; it is now
// refused before its body runs, so it never halts and its effect never fires.
func TestRev103b_F7_EmptyStepNameHaltIsUnresolvable(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	var charges int
	b := New[int, int]("nested-empty-halt")
	b.Step("a", func(ctx context.Context, n int) (int, error) {
		return agent.Step(ctx, mem, "r", "", func(context.Context) (int, error) {
			charges++
			if charges == 1 {
				return 0, errors.New("connection reset after the charge")
			}
			return n + 1, nil
		})
	}, ReadOnly())
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := flow.Run(ctx, mem, "r", 1); !errors.Is(err, agent.ErrConfig) {
			t.Fatalf("drive: %v, want the empty Step name refused with ErrConfig", err)
		}
	}
	if charges != 0 {
		t.Fatalf("the empty-named Step's body ran %d times, want 0", charges)
	}
}

// F5: Conform "prove[s] the run followed the declared graph". A journal that records the run's
// completion by this flow while its terminal node was never reached (here: the run halted on the
// first node) is reported as conforming.
func TestRev103b_F5_CompletionWithoutTerminalConforms(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	var fired int
	flow := effectFlow(t, &fired)
	_, _ = flow.Run(ctx, mem, "r", 5)
	if _, err := flow.Run(ctx, mem, "r", 5); err == nil {
		t.Fatal("want a halt")
	}
	done, _ := json.Marshal(completion{Flow: "charge-flow", Output: json.RawMessage(`"charged 5"`)})
	if _, err := mem.Do(ctx, "r", "run:complete", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: done}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if ok, diffs, err := flow.Conform(ctx, mem, "r"); err != nil || ok {
		t.Fatalf("a completion with no terminal node recorded: Conform = %v, %q, %v; want a divergence", ok, diffs, err)
	}
}
