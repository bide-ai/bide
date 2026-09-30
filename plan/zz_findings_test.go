package plan_test

// Findings of the review of PR #103 (plan lowering). Each test asserts the behaviour the PR's
// promises imply; a failing test is a finding.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/plan"
)

func twoNode(t *testing.T, name string, fired *atomic.Int64, extra bool) *plan.Flow[int, int] {
	t.Helper()
	b := plan.New[int, int](name)
	a := b.Step("a", func(_ context.Context, n int) (int, error) { fired.Add(1); return n + 1, nil })
	c := b.Step("c", func(_ context.Context, n int) (int, error) { return n * 10, nil }, plan.ReadOnly())
	b.Edge(a, c)
	if extra {
		d := b.Step("d", func(_ context.Context, n int) (int, error) { return n, nil }, plan.ReadOnly())
		b.Edge(c, d)
	}
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// F1: a completed flow run holds no terminal marker, so IsComplete says it is not complete and
// every Recover pass (every RecoverLoop tick) drives it again, forever. After a deploy that
// changes the flow, every completed run of it fails every pass with ErrConfig.
func TestF1_CompletedFlowIsRecoveredEveryPass(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	var fired atomic.Int64
	flow := twoNode(t, "two", &fired, false)
	if out, err := flow.Run(ctx, mem, "r", 1); err != nil || out != 20 {
		t.Fatalf("Run: %d, %v", out, err)
	}
	done, err := agent.IsComplete(ctx, mem, "r")
	if err != nil {
		t.Fatal(err)
	}
	drives := 0
	resume := func(ctx context.Context, runID string) error {
		drives++
		_, err := flow.Run(ctx, mem, runID, 1)
		return err
	}
	for range 3 {
		if _, err := agent.Recover(ctx, mem, resume); err != nil {
			t.Fatal(err)
		}
	}
	// The same flow redeployed with one more node: every pass now fails on the finished run.
	changed := twoNode(t, "two", &fired, true)
	var failures int
	for range 3 {
		_, err := agent.Recover(ctx, mem, func(ctx context.Context, runID string) error {
			_, err := changed.Run(ctx, mem, runID, 1)
			return err
		})
		if errors.Is(err, agent.ErrConfig) {
			failures++
		}
	}
	if !done || drives != 0 || failures != 0 {
		t.Fatalf("a finished flow run: IsComplete=%v, re-driven %d times by 3 Recover passes, and %d of 3 passes after a redeploy failed with ErrConfig; want true, 0, 0", done, drives, failures)
	}
}

// F2: a flow's run:start holds the input as the JSON text json.Marshal made; the documented
// recovery path (RecordedStart, decode Input, Run) re-encodes the decoded value, and for an input
// whose JSON does not round-trip byte for byte the resume is refused with ErrConfig forever.
func TestF2_RecordedStartInputDoesNotRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   any
	}{
		{"integer above 2^53 in any", map[string]any{"id": int64(9007199254740993)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			mem := agent.NewMemStore()
			var calls atomic.Int64
			b := plan.New[any, string]("rt")
			b.Step("a", func(_ context.Context, v any) (string, error) {
				if calls.Add(1) == 1 {
					return "", errors.New("connection reset after the effect")
				}
				return fmt.Sprint(v), nil
			})
			flow, err := b.Build()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := flow.Run(ctx, mem, "r", tc.in); err == nil {
				t.Fatal("first drive: want the node's error")
			}
			start, ok, err := agent.RecordedStart(ctx, mem, "r")
			if err != nil || !ok || start.Kind != agent.RunKindFlow {
				t.Fatalf("RecordedStart: %+v %v %v", start, ok, err)
			}
			var in any
			if err := json.Unmarshal([]byte(start.Input), &in); err != nil {
				t.Fatal(err)
			}
			_, err = flow.Run(ctx, mem, "r", in)
			if errors.Is(err, agent.ErrConfig) {
				t.Fatalf("resume with the recorded input (%s) decoded: %v; want the run's halt, not ErrConfig", start.Input, err)
			}
		})
	}
}

// F3: ResolveHaltRef accepts any well-formed node key and records it whether or not that node
// ever attempted anything: a key for a node that never halted pre-records its output, so its body
// (a side effect) never runs; a key the flow never reads ("node:iter:01:x") succeeds and does
// nothing.
func TestF3_ResolveHaltRefOnANodeThatNeverHalted(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	var fired atomic.Int64
	flow := twoNode(t, "two", &fired, false)
	ref := agent.HaltRef{RunID: "r", Op: agent.OpRef{Kind: agent.OpStep, ID: "node:a"}, Cause: agent.HaltCrashed}
	err := agent.ResolveHaltRef(ctx, mem, ref, agent.Outcome{Result: 7})
	out, rerr := flow.Run(ctx, mem, "r", 1)
	t.Logf("resolve of an unattempted node: %v; Run -> %d, %v; body ran %d times", err, out, rerr, fired.Load())
	if err == nil {
		t.Errorf("ResolveHaltRef recorded an output for node:a, which never attempted anything: want a refusal")
	}
	ref.Op.ID = "node:iter:01:a"
	if err := agent.ResolveHaltRef(ctx, agent.NewMemStore(), ref, agent.Outcome{Result: 7}); err == nil {
		t.Errorf("ResolveHaltRef accepted node:iter:01:a, a key Run never writes (it writes node:iter:1:a)")
	}
}

// F4: a resolution whose Result does not decode as the node's output type is accepted, and the
// run can then never continue: the next node's input decode fails on every drive, and a
// corrected resolution is refused as HaltAlreadyResolved.
func TestF4_ResolutionOfTheWrongTypeStrandsTheRun(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	var calls atomic.Int64
	b := plan.New[int, int]("typed")
	a := b.Step("a", func(_ context.Context, n int) (int, error) {
		if calls.Add(1) == 1 {
			return 0, errors.New("connection reset after the effect")
		}
		return n + 1, nil
	})
	c := b.Step("c", func(_ context.Context, n int) (int, error) { return n * 10, nil }, plan.ReadOnly())
	b.Edge(a, c)
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = flow.Run(ctx, mem, "r", 1)
	_, err = flow.Run(ctx, mem, "r", 1)
	halt, ok := errors.AsType[*agent.OutcomeUnknown](err)
	if !ok {
		t.Fatalf("want a halt: %v", err)
	}
	rerr := agent.ResolveHaltRef(ctx, mem, halt.Ref(), agent.Outcome{Result: "charged"})
	_, err1 := flow.Run(ctx, mem, "r", 1)
	fix := agent.ResolveHaltRef(ctx, mem, halt.Ref(), agent.Outcome{Result: 2})
	_, err2 := flow.Run(ctx, mem, "r", 1)
	t.Logf("wrong-type resolve: %v; drive: %v; corrected resolve: %v; drive: %v", rerr, err1, fix, err2)
	if rerr == nil && err2 != nil {
		t.Fatalf("a resolution of the wrong type was accepted and strands the run (drive: %v, correction: %v)", err2, fix)
	}
}

// F5: Conform reports a Step that a node's body runs (which #103's reserved prefixes exist to
// make safe) as an unexpected step, so a conforming run of such a flow never conforms.
func TestF5_ConformFlagsAStepInsideANodeBody(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	b := plan.New[int, int]("nested")
	b.Step("a", func(ctx context.Context, n int) (int, error) {
		return agent.Step(ctx, mem, "r", "fetch", func(context.Context) (int, error) { return n + 1, nil })
	})
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := flow.Run(ctx, mem, "r", 1); err != nil || out != 2 {
		t.Fatalf("Run: %d %v", out, err)
	}
	if ok, diffs, err := flow.Conform(ctx, mem, "r"); err != nil || !ok {
		t.Fatalf("Conform of a clean run = %v, %q, %v; want it to conform", ok, diffs, err)
	}
}

// F5b: Conform checks record membership only: a journal holding the result of a node on the arm
// its Switch did not take, or an iteration key for a node outside any loop, conforms.
func TestF5b_ConformMissesOffPathRecords(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	b := plan.New[int, string]("sw")
	a := b.Step("a", func(_ context.Context, n int) (int, error) { return n, nil }, plan.ReadOnly())
	yes := b.Step("yes", func(_ context.Context, n int) (string, error) { return "yes", nil }, plan.ReadOnly())
	no := b.Step("no", func(_ context.Context, n int) (string, error) { return "no", nil }, plan.ReadOnly())
	b.Switch(a, plan.When(func(v int) bool { return v > 0 }, yes), plan.Else(no))
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := flow.Run(ctx, mem, "r", 1); err != nil || out != "yes" {
		t.Fatalf("Run: %q %v", out, err)
	}
	for _, name := range []string{"node:no", "node:iter:3:a"} {
		if _, err := mem.Do(ctx, "r", name, func(context.Context) (agent.Record, error) {
			return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`"x"`)}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if ok, diffs, _ := flow.Conform(ctx, mem, "r"); ok {
		t.Errorf("Conform = ok (%q) with node:no (the untaken arm) and node:iter:3:a (no loop) in the journal", diffs)
	}
}

// F6: Flow.Run does not check its run ID as agent.Run does: an empty ID, and an ID in the
// sub-agent form (which Recover skips and ResolveHaltRef leases by its root) are accepted.
func TestF6_FlowRunAcceptsRunIDsAgentRefuses(t *testing.T) {
	ctx := context.Background()
	var fired atomic.Int64
	flow := twoNode(t, "two", &fired, false)
	for _, id := range []string{"", "parent>call"} {
		if _, err := flow.Run(ctx, agent.NewMemStore(), id, 1); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("Flow.Run with run ID %q: err = %v, want ErrConfig as agent.Run gives", id, err)
		}
	}
}

// F7 (pre-existing, not from #103): a Step a loop body runs has the same name in every
// iteration, so iteration 1 replays iteration 0's result and its effect never runs.
func TestF7_NestedStepInLoopBodyIsNotIterationScoped(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	var charges atomic.Int64
	b := plan.New[int, string]("nloop")
	seed := b.Step("seed", func(_ context.Context, n int) (fLoopState, error) { return fLoopState{n}, nil }, plan.ReadOnly())
	inc := b.Step("inc", func(ctx context.Context, s fLoopState) (fLoopState, error) {
		v, err := agent.Step(ctx, mem, "r", "charge", func(context.Context) (int, error) { return int(charges.Add(1)), nil })
		return fLoopState{s.N + 1 + 0*v}, err
	})
	check := b.Step("check", func(_ context.Context, s fLoopState) (fLoopState, error) { return s, nil }, plan.ReadOnly())
	done := b.Step("done", func(_ context.Context, s fLoopState) (string, error) { return fmt.Sprint(s.N), nil }, plan.ReadOnly())
	b.Edge(seed, inc)
	b.Edge(inc, check)
	b.Switch(check, plan.LoopBack(5, func(s fLoopState) bool { return s.N < 2 }, inc), plan.Else(done))
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := flow.Run(ctx, mem, "r", 0); err != nil || out != "2" {
		t.Fatalf("Run: %q %v", out, err)
	}
	if charges.Load() != 2 {
		t.Errorf("a two-iteration loop charged %d times, want 2", charges.Load())
	}
}

// Concurrency: two drivers of one flow run. With the same input, the effect fires at most once;
// with different inputs, exactly one start wins and the loser runs nothing.
func TestF8_ConcurrentDrivers(t *testing.T) {
	for i := range 300 {
		ctx := context.Background()
		mem := agent.NewMemStore()
		var fired atomic.Int64
		flow := twoNode(t, "two", &fired, false)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		ins := []int{1, 1}
		if i%2 == 1 {
			ins[1] = 2
		}
		for k := range 2 {
			wg.Go(func() { _, errs[k] = flow.Run(ctx, mem, "r", ins[k]) })
		}
		wg.Wait()
		if fired.Load() > 1 {
			t.Fatalf("iteration %d: the effect fired %d times (%v)", i, fired.Load(), errs)
		}
		if ins[0] != ins[1] {
			nConfig := 0
			for _, err := range errs {
				if errors.Is(err, agent.ErrConfig) {
					nConfig++
				}
			}
			if nConfig != 1 || fired.Load() != 1 {
				t.Fatalf("iteration %d: different inputs: %v, fired %d; want one ErrConfig and one fire", i, errs, fired.Load())
			}
		}
	}
}

// F9: a loop whose body is one node (the head is the switched node) is accepted by Build, but
// Run feeds the head a nil input: nodeInput takes the loop Switch's arm (the head is its
// target, and the head is live) before the edge from its forward predecessor, and reads the
// head's own result, which does not exist yet.
func TestF9_SingleNodeLoop(t *testing.T) {
	b := plan.New[int, string]("one")
	seed := b.Step("seed", func(_ context.Context, n int) (fLoopState, error) { return fLoopState{n}, nil }, plan.ReadOnly())
	inc := b.Step("inc", func(_ context.Context, s fLoopState) (fLoopState, error) { return fLoopState{s.N + 1}, nil }, plan.ReadOnly())
	done := b.Step("done", func(_ context.Context, s fLoopState) (string, error) { return fmt.Sprint(s.N), nil }, plan.ReadOnly())
	b.Edge(seed, inc)
	b.Switch(inc, plan.LoopBack(5, func(s fLoopState) bool { return s.N < 2 }, inc), plan.Else(done))
	flow, err := b.Build()
	if err != nil {
		t.Skipf("Build refuses a one-node loop: %v", err)
	}
	if out, err := flow.Run(context.Background(), agent.NewMemStore(), "r", 0); err != nil || out != "2" {
		t.Fatalf("Run: %q, %v; want \"2\"", out, err)
	}
}

// F10: the CHANGELOG says Conform "requires run:start to name the flow", but a journal with no
// run:start at all (and no flow:digest) conforms: only a present record is checked.
func TestF10_ConformWithoutRunStartOrDigest(t *testing.T) {
	ctx := context.Background()
	var fired atomic.Int64
	flow := twoNode(t, "two", &fired, false)
	mem := agent.NewMemStore()
	for _, w := range []struct{ name, val string }{{"node:a", "2"}, {"node:c", "20"}} {
		if _, err := mem.Do(ctx, "r", w.name, func(context.Context) (agent.Record, error) {
			return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(w.val)}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if ok, diffs, err := flow.Conform(ctx, mem, "r"); err != nil || ok {
		t.Fatalf("Conform = %v, %q, %v on a journal with no run:start and no flow:digest; want a divergence", ok, diffs, err)
	}
}
