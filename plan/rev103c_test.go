package plan

// Adversarial review of PR #103's fixes after bde345a (F2, F4, F5, F7). Each test states the
// property the fix claims and fails while the hole is open.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// F5: a terminal node resolved through Flow.ResolveHalt with an output holding '<', '>' or '&'.
// agent.ResolveHaltRef records the Result unescaped (marshalJournal), Run replays those bytes as
// the terminal's result, and json.Marshal(completion{...}) HTML-escapes them inside Output. The
// completion is the terminal's output, but sameJSON compares bytes after json.Compact (which does
// not undo escapes), so Conform reports a divergence on a run Run itself produced.
func TestRev103c_F5_ResolvedTerminalWithHTMLCharsConforms(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	var fired int
	b := New[int, string]("html")
	b.Step("issue", func(_ context.Context, n int) (string, error) {
		fired++
		if fired == 1 {
			return "", errors.New("connection reset after the effect")
		}
		return "unused", nil
	})
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
	if err := flow.ResolveHalt(ctx, mem, halt.Ref(), agent.Outcome{Result: "a<b & c>d"}); err != nil {
		t.Fatalf("ResolveHalt: %v", err)
	}
	out, err := flow.Run(ctx, mem, "r", 1)
	if err != nil || out != "a<b & c>d" {
		t.Fatalf("after the resolution: %q, %v", out, err)
	}
	if ok, diffs, err := flow.Conform(ctx, mem, "r"); err != nil || !ok {
		recs, _ := mem.History(ctx, "r")
		for _, r := range recs {
			t.Logf("%s %s", r.Name, r.Result)
		}
		t.Fatalf("a run Run completed: Conform = %v, %q, %v; want it to conform", ok, diffs, err)
	}
}

// F5: "Conform requires that run:complete carries the recorded output of the last terminal node
// the run reached." In a fan-out a -> {x, y}, the run reaches both terminals and Run completes with
// y's output (y is last in topological order). A journal with y's record removed and the
// completion rewritten to x's output is not one Run can produce (a completed run recorded every
// live node), but lastTerminal skips a live terminal with no record, so Conform accepts it.
func TestRev103c_F5_CompletionSkippingAReachedTerminalConforms(t *testing.T) {
	ctx := context.Background()
	build := func() *Flow[int, string] {
		b := New[int, string]("fan")
		a := b.Step("a", func(_ context.Context, n int) (int, error) { return n, nil }, ReadOnly())
		x := b.Step("x", func(_ context.Context, n int) (string, error) { return "x", nil }, ReadOnly())
		y := b.Step("y", func(_ context.Context, n int) (string, error) { return "y", nil }, ReadOnly())
		b.Edge(a, x)
		b.Edge(a, y)
		f, err := b.Build()
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	flow := build()
	src := agent.NewMemStore()
	out, err := flow.Run(ctx, src, "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := src.History(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	lastTerm := "node:y"
	if out == "x" {
		lastTerm = "node:x"
	}
	other := `"y"`
	if out == "y" {
		other = `"x"`
	}
	forged := agent.NewMemStore()
	for _, r := range recs {
		switch r.Name {
		case lastTerm:
			continue
		case "run:complete":
			done, _ := json.Marshal(completion{Flow: "fan", Output: json.RawMessage(other)})
			r.Result = done
		}
		if _, err := forged.Do(ctx, "r", r.Name, func(context.Context) (agent.Record, error) { return r, nil }); err != nil {
			t.Fatal(err)
		}
	}
	if ok, diffs, err := flow.Conform(ctx, forged, "r"); err != nil || ok {
		t.Fatalf("a completion of %s while the reached terminal %s has no record: Conform = %v, %q, %v; want a divergence", other, lastTerm, ok, diffs, err)
	}
}

// F5, the same hole in a linear flow: a -> b, completed, with node:a's record removed. Run records
// every node before its completion; Conform accepts the journal.
func TestRev103c_F5_CompletedRunMissingAReachedNodeConforms(t *testing.T) {
	ctx := context.Background()
	b := New[int, int]("lin")
	a := b.Step("a", func(_ context.Context, n int) (int, error) { return n + 1, nil }, ReadOnly())
	z := b.Step("z", func(_ context.Context, n int) (int, error) { return n + 1, nil }, ReadOnly())
	b.Edge(a, z)
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	src := agent.NewMemStore()
	if _, err := flow.Run(ctx, src, "r", 1); err != nil {
		t.Fatal(err)
	}
	recs, _ := src.History(ctx, "r")
	forged := agent.NewMemStore()
	for _, r := range recs {
		if r.Name == "node:a" {
			continue
		}
		if _, err := forged.Do(ctx, "r", r.Name, func(context.Context) (agent.Record, error) { return r, nil }); err != nil {
			t.Fatal(err)
		}
	}
	if ok, diffs, err := flow.Conform(ctx, forged, "r"); err != nil || ok {
		t.Fatalf("a completed run with no record of the reached node a: Conform = %v, %q, %v; want a divergence", ok, diffs, err)
	}
}

// F5 probe: a loop body node with an edge out of the loop (a terminal fed from inside the loop,
// reached once per iteration). Build must refuse it, or Run and Conform must agree on it.
func TestRev103c_F5_TerminalFedFromInsideALoop(t *testing.T) {
	ctx := context.Background()
	b := New[int, string]("side")
	seed := b.Step("seed", func(_ context.Context, n int) (loopState, error) { return loopState{N: n}, nil })
	refine := b.Step("refine", func(_ context.Context, s loopState) (loopState, error) { return loopState{N: s.N - 1}, nil })
	check := b.Step("check", func(_ context.Context, s loopState) (loopState, error) { return s, nil })
	side := b.Step("side", func(_ context.Context, s loopState) (string, error) { return "side", nil })
	done := b.Step("done", func(_ context.Context, s loopState) (string, error) { return "done", nil })
	b.Edge(seed, refine)
	b.Edge(refine, check)
	b.Edge(refine, side)
	b.Switch(check, LoopBack(5, func(s loopState) bool { return s.N > 0 }, refine), Else(done))
	flow, err := b.Build()
	if err != nil {
		t.Logf("Build refuses an edge out of a loop body: %v", err)
		return
	}
	mem := agent.NewMemStore()
	out, err := flow.Run(ctx, mem, "r", 3)
	t.Logf("Run = %q, %v", out, err)
	if err == nil {
		if ok, diffs, err := flow.Conform(ctx, mem, "r"); err != nil || !ok {
			t.Fatalf("Conform of a run Run completed = %v, %q, %v", ok, diffs, err)
		}
	}
}

// F5 probe: a Switch with several terminal arms, each a terminal; the run completes on the chosen
// one and conforms, for either choice.
func TestRev103c_F5_SwitchOfTerminals(t *testing.T) {
	ctx := context.Background()
	b := New[int, string]("sw3")
	a := b.Step("a", func(_ context.Context, n int) (int, error) { return n, nil }, ReadOnly())
	p := b.Step("p", func(_ context.Context, n int) (string, error) { return "p", nil }, ReadOnly())
	q := b.Step("q", func(_ context.Context, n int) (string, error) { return "q", nil }, ReadOnly())
	r := b.Step("r", func(_ context.Context, n int) (string, error) { return "r", nil }, ReadOnly())
	b.Switch(a, When(func(v int) bool { return v == 1 }, p), When(func(v int) bool { return v == 2 }, q), Else(r))
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	for in, want := range map[int]string{1: "p", 2: "q", 3: "r"} {
		mem := agent.NewMemStore()
		if out, err := flow.Run(ctx, mem, "r", in); err != nil || out != want {
			t.Fatalf("Run(%d) = %q, %v", in, out, err)
		}
		if ok, diffs, err := flow.Conform(ctx, mem, "r"); err != nil || !ok {
			t.Fatalf("Conform(%d) = %v, %q, %v", in, ok, diffs, err)
		}
	}
}

// F4 probe: a halted run whose start or digest is missing, corrupt or not a flow's is refused, and
// nothing is recorded.
func TestRev103c_F4_CorruptStartOrDigest(t *testing.T) {
	ctx := context.Background()
	var fired int
	flow := effectFlow(t, &fired)
	digest, _ := json.Marshal(flow.Digest())
	for name, recs := range map[string][][2]string{
		"corrupt start":  {{"run:start", `"not a start"`}, {flowDigestStep, string(digest)}},
		"agent start":    {{"run:start", `{"input":"5"}`}, {flowDigestStep, string(digest)}},
		"no digest":      {{"run:start", `{"input":"5","kind":"flow","flow":{"name":"charge-flow"}}`}},
		"corrupt digest": {{"run:start", `{"input":"5","kind":"flow","flow":{"name":"charge-flow"}}`}, {flowDigestStep, `7`}},
	} {
		mem := agent.NewMemStore()
		for _, w := range recs {
			if _, err := mem.Do(ctx, "r", w[0], func(context.Context) (agent.Record, error) {
				return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(w[1])}, nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := mem.Do(ctx, "r", "attempt:step:node:charge", func(context.Context) (agent.Record, error) {
			return agent.Record{Kind: agent.StepAttempt, ToolUseID: "node:charge", AttemptedAt: 1}, nil
		}); err != nil {
			t.Fatal(err)
		}
		ref := agent.HaltRef{RunID: "r", Op: agent.OpRef{Kind: agent.OpStep, ID: "node:charge"}, Cause: agent.HaltCrashed}
		if err := flow.ResolveHalt(ctx, mem, ref, agent.Outcome{Result: 7}, agent.WithoutLiveDriverCheck()); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("%s: ResolveHalt = %v, want ErrConfig", name, err)
		}
		if got, _ := mem.History(ctx, "r"); hasRecord(got, "node:charge") {
			t.Errorf("%s: a refused resolution recorded the node", name)
		}
	}
}
