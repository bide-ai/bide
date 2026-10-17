package plan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strconv"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/journalhook"
	"github.com/bide-ai/bide/internal/journaltest"
)

// TestConformCleanRun builds a small switched flow, runs it against a MemStore to
// produce a real journal, then asserts Conform reports the run followed the
// declared topology: ok is true and diffs is empty. This is the positive
// accountability case: the journal names only declared nodes and the recorded
// Switch choice picked a declared arm.
func TestConformCleanRun(t *testing.T) {
	b := New[int, string]("conform-clean")
	entry := b.Step("entry", func(_ context.Context, n int) (int, error) { return n + 1, nil })
	high := b.Step("high", func(context.Context, int) (string, error) { return "high", nil })
	low := b.Step("low", func(context.Context, int) (string, error) { return "low", nil })
	b.Switch(entry,
		When(func(n int) bool { return n > 0 }, high),
		Else(low),
	)

	flow, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	mem := agenttest.MemJournal()
	ctx := context.Background()
	if _, err := flow.Run(ctx, mem, "clean", 1); err != nil {
		t.Fatalf("Run: %v", err)
	}

	ok, diffs, err := flow.Conform(ctx, mem, "clean")
	if err != nil {
		t.Fatalf("Conform: %v", err)
	}
	if !ok {
		t.Errorf("Conform reported divergence on a clean run: %v", diffs)
	}
	if len(diffs) != 0 {
		t.Errorf("Conform returned diffs %v on a clean run, want none", diffs)
	}
}

// TestConformUnexpectedStep hand-crafts a journal (via store.Do) that contains a
// step name NOT in the declared topology, alongside the run's real records, and
// asserts Conform reports the intruder as a divergence: ok is false and diffs
// names the unexpected step. This is the negative accountability case.
func TestConformUnexpectedStep(t *testing.T) {
	b := New[int, string]("conform-intruder")
	entry := b.Step("entry", func(_ context.Context, n int) (int, error) { return n + 1, nil })
	high := b.Step("high", func(context.Context, int) (string, error) { return "high", nil })
	low := b.Step("low", func(context.Context, int) (string, error) { return "low", nil })
	b.Switch(entry,
		When(func(n int) bool { return n > 0 }, high),
		Else(low),
	)

	flow, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	mem := agenttest.MemJournal()
	ctx := context.Background()
	if _, err := flow.Run(ctx, mem, "intruder", 1); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Inject a step name that no declared node owns. store.Do memoizes it into the
	// same run's journal, so History will surface it to Conform.
	if _, err := journaltest.Do(ctx, mem, "intruder", "ghost", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`"boo"`)}, nil
	}); err != nil {
		t.Fatalf("inject ghost step: %v", err)
	}

	ok, diffs, err := flow.Conform(ctx, mem, "intruder")
	if err != nil {
		t.Fatalf("Conform: %v", err)
	}
	if ok {
		t.Fatal("Conform reported ok on a journal with an undeclared step")
	}
	found := false
	for _, d := range diffs {
		if contains(d, "ghost") {
			found = true
		}
	}
	if !found {
		t.Errorf("Conform diffs %v do not name the undeclared step %q", diffs, "ghost")
	}
}

// TestConformUnreachableArm hand-crafts a "switch:<over>" record whose recorded
// chosen target is not a declared arm of that Switch, and asserts Conform flags
// the unreachable arm as a divergence. It uses a runID with no prior records so
// the only record present is the crafted choice.
func TestConformUnreachableArm(t *testing.T) {
	b := New[int, string]("conform-arm")
	entry := b.Step("entry", func(_ context.Context, n int) (int, error) { return n + 1, nil })
	high := b.Step("high", func(context.Context, int) (string, error) { return "high", nil })
	low := b.Step("low", func(context.Context, int) (string, error) { return "low", nil })
	b.Switch(entry,
		When(func(n int) bool { return n > 0 }, high),
		Else(low),
	)

	flow, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	mem := agenttest.MemJournal()
	ctx := context.Background()

	// A Switch choice record whose target is not a declared arm of the entry Switch.
	if _, err := journaltest.Do(ctx, mem, "arm", "switch:entry", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`"nowhere"`)}, nil
	}); err != nil {
		t.Fatalf("inject switch choice: %v", err)
	}

	ok, diffs, err := flow.Conform(ctx, mem, "arm")
	if err != nil {
		t.Fatalf("Conform: %v", err)
	}
	if ok {
		t.Fatal("Conform reported ok on a Switch choice to an undeclared arm")
	}
	found := false
	for _, d := range diffs {
		if contains(d, "switch:entry") && contains(d, "nowhere") {
			found = true
		}
	}
	if !found {
		t.Errorf("Conform diffs %v do not flag the unreachable arm choice", diffs)
	}
}

// TestConformHaltedRunIsObservable asserts a partial journal (an attempt marker
// present without its result, as a *agent.OutcomeUnknown run leaves behind) is NOT a
// divergence: the attempt maps to its declared node, and a missing result is an
// in-flight state, not an unexpected step. Conform must not panic and must report
// ok on such a journal.
func TestConformHaltedRunIsObservable(t *testing.T) {
	b := New[int, string]("conform-halt")
	entry := b.Step("entry", func(_ context.Context, n int) (int, error) { return n + 1, nil })
	high := b.Step("high", func(context.Context, int) (string, error) { return "high", nil })
	low := b.Step("low", func(context.Context, int) (string, error) { return "low", nil })
	b.Switch(entry,
		When(func(n int) bool { return n > 0 }, high),
		Else(low),
	)

	flow, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	mem := agenttest.MemJournal()
	ctx := context.Background()

	// Simulate a run halted mid-node: the run's start and digest are recorded, and the attempt
	// marker for the entry node, but its result never was.
	for _, w := range []struct {
		name string
		rec  agent.Record
	}{
		{"run:start", agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`{"input":"1","kind":"flow","flow":{"name":"conform-halt"}}`)}},
		{flowDigestStep, agent.Record{Kind: agent.StepValue, Result: json.RawMessage(strconv.Quote(flow.Digest()))}},
		{"attempt:step:node:entry", agent.Record{Kind: agent.StepAttempt, ToolUseID: "node:entry"}},
	} {
		if _, err := journaltest.Do(ctx, mem, "halt", w.name, func(context.Context) (agent.Record, error) { return w.rec, nil }); err != nil {
			t.Fatalf("inject %s: %v", w.name, err)
		}
	}

	ok, diffs, err := flow.Conform(ctx, mem, "halt")
	if err != nil {
		t.Fatalf("Conform: %v", err)
	}
	if !ok {
		t.Errorf("Conform flagged a halted in-flight journal as divergent: %v", diffs)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// A claim's bookkeeping (a not-started record, written when a node's claim failed before its body
// ran) says nothing about the flow's topology: Conform does not report it as a step, or as an
// attempt for an undeclared one.
func TestConformIgnoresNotStartedRecords(t *testing.T) {
	b := New[int, int]("conform-claims")
	b.Step("entry", func(_ context.Context, n int) (int, error) { return n + 1, nil })
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	mem := agenttest.MemJournal()
	ctx := context.Background()
	if _, err := flow.Run(ctx, mem, "r", 1); err != nil {
		t.Fatal(err)
	}
	for name, kind := range map[string]agent.StepKind{
		"attempt:not-started:0123abcd:attempt:step:node:entry": agent.StepNotStarted,
	} {
		rec := journalhook.WithClaim(agent.Record{Kind: kind}, name[len("attempt:not-started:"):len("attempt:not-started:")+8]).(agent.Record)
		if _, err := journaltest.Do(ctx, mem, "r", name, func(context.Context) (agent.Record, error) { return rec, nil }); err != nil {
			t.Fatal(err)
		}
	}
	ok, diffs, err := flow.Conform(ctx, mem, "r")
	if err != nil || !ok || len(diffs) != 0 {
		t.Fatalf("Conform = %v, %v, %v; want the run to conform", ok, diffs, err)
	}
}

// Conformance and resume properties of plan lowering: flow input identity (F2), halts resolved
// through another flow (F4) and reserved step names (F7). Each test states the property.

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
	mem := agenttest.MemJournal()
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
	mem := agenttest.MemJournal()
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

// F7: a node's body may run agent.Journal.Step with any name agent.Journal.Step accepts outside a flow, "" among
// them. Inside a node it was recorded as "node:<node>:step:", which neither ResolveHalt nor
// Flow.ResolveHalt nor Conform accepts: a completed run of the declared graph did not conform.
// agent.Journal.Step now refuses an empty name everywhere (ErrConfig), so the drive fails before the
// Step's body runs and records no such key, and the journal conforms.
func TestRev103b_F7_EmptyStepNameInsideNodeDoesNotConform(t *testing.T) {
	ctx := context.Background()
	mem := agenttest.MemJournal()
	b := New[int, int]("nested-empty")
	b.Step("a", func(ctx context.Context, n int) (int, error) {
		return mem.Step(ctx, "r", "", func(context.Context) (int, error) { return n + 1, nil })
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
	mem := agenttest.MemJournal()
	var charges int
	b := New[int, int]("nested-empty-halt")
	b.Step("a", func(ctx context.Context, n int) (int, error) {
		return mem.Step(ctx, "r", "", func(context.Context) (int, error) {
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
	mem := agenttest.MemJournal()
	var fired int
	flow := effectFlow(t, &fired)
	_, _ = flow.Run(ctx, mem, "r", 5)
	if _, err := flow.Run(ctx, mem, "r", 5); err == nil {
		t.Fatal("want a halt")
	}
	done, _ := json.Marshal(completion{Flow: "charge-flow", Output: json.RawMessage(`"charged 5"`)})
	if _, err := journaltest.Do(ctx, mem, "r", "run:complete", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: done}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if ok, diffs, err := flow.Conform(ctx, mem, "r"); err != nil || ok {
		t.Fatalf("a completion with no terminal node recorded: Conform = %v, %q, %v; want a divergence", ok, diffs, err)
	}
}

// Conformance of resolved terminals and completed runs (F5), with the F2, F4 and F7 cases that
// share their setup. Each test states the property.

// F5: a terminal node resolved through Flow.ResolveHalt with an output holding '<', '>' or '&'.
// agent.ResolveHalt records the Result unescaped (marshalJournal), Run replays those bytes as
// the terminal's result, and json.Marshal(completion{...}) HTML-escapes them inside Output. The
// completion is the terminal's output, but sameJSON compares bytes after json.Compact (which does
// not undo escapes), so Conform reports a divergence on a run Run itself produced.
func TestRev103c_F5_ResolvedTerminalWithHTMLCharsConforms(t *testing.T) {
	ctx := context.Background()
	mem := agenttest.MemJournal()
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
	src := agenttest.MemJournal()
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
	forged := agenttest.MemJournal()
	for _, r := range recs {
		switch r.Name {
		case lastTerm:
			continue
		case "run:complete":
			done, _ := json.Marshal(completion{Flow: "fan", Output: json.RawMessage(other)})
			r.Result = done
		}
		if _, err := journaltest.Do(ctx, forged, "r", r.Name, func(context.Context) (agent.Record, error) { return r, nil }); err != nil {
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
	src := agenttest.MemJournal()
	if _, err := flow.Run(ctx, src, "r", 1); err != nil {
		t.Fatal(err)
	}
	recs, _ := src.History(ctx, "r")
	forged := agenttest.MemJournal()
	for _, r := range recs {
		if r.Name == "node:a" {
			continue
		}
		if _, err := journaltest.Do(ctx, forged, "r", r.Name, func(context.Context) (agent.Record, error) { return r, nil }); err != nil {
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
	mem := agenttest.MemJournal()
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
		mem := agenttest.MemJournal()
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
		mem := agenttest.MemJournal()
		for _, w := range recs {
			if _, err := journaltest.Do(ctx, mem, "r", w[0], func(context.Context) (agent.Record, error) {
				return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(w[1])}, nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := journaltest.Do(ctx, mem, "r", "attempt:step:node:charge", func(context.Context) (agent.Record, error) {
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

// A completed run whose nodes ran Parallel tasks and nested Steps conforms.
func TestRev103d_ConformParallelAndNestedSteps(t *testing.T) {
	ctx := context.Background()
	mem := agenttest.MemJournal()
	b := New[string, string]("par")
	a := b.Step("a", func(ctx context.Context, in string) (string, error) {
		res, err := mem.Parallel(ctx, "r", []agent.Task[string]{
			{Name: "x", Fn: func(context.Context) (string, error) { return "<x>", nil }},
			{Name: "y:z", Fn: func(context.Context) (string, error) { return "&y", nil }},
		}, agent.WithMaxConcurrency(2))
		if err != nil {
			return "", err
		}
		outer, err := mem.Step(ctx, "r", "outer", func(ctx context.Context) (string, error) {
			return mem.Step(ctx, "r", "inner", func(context.Context) (string, error) { return "in", nil })
		})
		return in + res[0] + res[1] + outer, err
	})
	c := b.Step("c", func(ctx context.Context, in string) (string, error) { return in + "!", nil })
	b.Edge(a, c)
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	out, err := flow.Run(ctx, mem, "r", "go<")
	if err != nil {
		t.Fatal(err)
	}
	recs, _ := mem.History(ctx, "r")
	for _, r := range recs {
		t.Logf("%s %s %s", r.Kind, r.Name, r.Result)
	}
	if ok, diffs, err := flow.Conform(ctx, mem, "r"); err != nil || !ok {
		t.Fatalf("Conform of a completed run with Parallel and nested Steps = %v, %q, %v (out %q)", ok, diffs, err, out)
	}
}

// A run that halted mid-node (the terminal, and a Step inside a node), resolved and resumed, conforms.
func TestRev103d_ConformAfterHaltResolved(t *testing.T) {
	ctx := context.Background()
	mem := agenttest.MemJournal()
	var fails = map[string]bool{"inside": true, "last": true}
	b := New[int, string]("halts")
	a := b.Step("a", func(ctx context.Context, n int) (int, error) {
		return mem.Step(ctx, "r", "inside", func(context.Context) (int, error) {
			if fails["inside"] {
				return 0, errors.New("crash inside")
			}
			return n + 1, nil
		})
	})
	last := b.Step("last", func(_ context.Context, n int) (string, error) {
		if fails["last"] {
			return "", errors.New("crash at the terminal")
		}
		return fmt.Sprint("<", n, ">"), nil
	})
	b.Edge(a, last)
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		_, err = flow.Run(ctx, mem, "r", 1)
		if err == nil {
			break
		}
		halt, ok := errors.AsType[*agent.OutcomeUnknown](err)
		if !ok {
			continue // the first failed attempt; the next drive halts
		}
		var res any = 41
		if halt.Op.ID == "node:last" {
			res = "<resolved & done>"
		}
		if rerr := flow.ResolveHalt(ctx, mem, halt.Ref(), agent.Outcome{Result: res}); rerr != nil {
			t.Fatalf("ResolveHalt %s: %v", halt.Op.ID, rerr)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	out, _ := flow.Run(ctx, mem, "r", 1)
	if out != "<resolved & done>" {
		t.Fatalf("out %q", out)
	}
	if ok, diffs, err := flow.Conform(ctx, mem, "r"); err != nil || !ok {
		recs, _ := mem.History(ctx, "r")
		for _, r := range recs {
			t.Logf("%s %s %s", r.Kind, r.Name, r.Result)
		}
		t.Fatalf("Conform after resolved halts = %v, %q, %v", ok, diffs, err)
	}
}

// A run begun before the escaping change (input, digest and a node result written by json.Marshal,
// with HTML escapes) resumes after it, completes, and conforms.
func TestRev103d_ResumeAcrossTheEscapingChange(t *testing.T) {
	ctx := context.Background()
	b := New[map[string]string, string]("mix")
	a := b.Step("a", func(_ context.Context, in map[string]string) (string, error) { return in["k"] + "<&>", nil })
	c := b.Step("c", func(_ context.Context, s string) (string, error) { return s + "</end>", nil })
	b.Edge(a, c)
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	in := map[string]string{"k": "<in>", "z<": "&"}
	src := agenttest.MemJournal()
	want, err := flow.Run(ctx, src, "r", in)
	if err != nil {
		t.Fatal(err)
	}
	recs, _ := src.History(ctx, "r")
	old := agenttest.MemJournal()
	for _, r := range recs {
		if r.Kind == agent.StepHeader || r.Name == "node:c" || r.Name == "attempt:step:node:c" || r.Name == "run:complete" {
			continue
		}
		switch r.Name {
		case "run:start":
			var st agent.RunStart
			_ = json.Unmarshal(r.Result, &st)
			inb, _ := json.Marshal(in)
			st.Input = agent.UserText(string(inb))
			r.Result, _ = json.Marshal(st)
		case "node:a":
			var s string
			_ = json.Unmarshal(r.Result, &s)
			r.Result, _ = json.Marshal(s)
		}
		if _, err := journaltest.Do(ctx, old, "r", r.Name, func(context.Context) (agent.Record, error) { return r, nil }); err != nil {
			t.Fatal(err)
		}
	}
	got, err := flow.Run(ctx, old, "r", in)
	if err != nil || got != want {
		t.Fatalf("resume across the change = %q, %v; want %q", got, err, want)
	}
	if ok, diffs, err := flow.Conform(ctx, old, "r"); err != nil || !ok {
		t.Fatalf("Conform = %v, %q, %v", ok, diffs, err)
	}
	if again, err := flow.Run(ctx, old, "r", in); err != nil || again != want {
		t.Fatalf("drive of the completed run = %q, %v", again, err)
	}
}

func conformFlow(t *testing.T) *Flow[int, string] {
	t.Helper()
	b := New[int, string]("conform-cov")
	entry := b.Step("entry", func(_ context.Context, n int) (int, error) { return n + 1, nil })
	high := b.Step("high", func(context.Context, int) (string, error) { return "high", nil })
	low := b.Step("low", func(context.Context, int) (string, error) { return "low", nil })
	b.Switch(entry, When(func(n int) bool { return n > 0 }, high), Else(low))
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	return flow
}

// injected journals recs in a fresh run and returns Conform's verdict on it.
func injected(t *testing.T, flow *Flow[int, string], recs ...agent.Record) (bool, []string) {
	t.Helper()
	ctx := context.Background()
	j := agenttest.MemJournal()
	for _, r := range recs {
		if _, err := journaltest.Do(ctx, j, "r", r.Name, func(context.Context) (agent.Record, error) { return r, nil }); err != nil {
			t.Fatal(err)
		}
	}
	ok, diffs, err := flow.Conform(ctx, j, "r")
	if err != nil {
		t.Fatal(err)
	}
	return ok, diffs
}

func valueRec(name, result string) agent.Record {
	return agent.Record{Name: name, Kind: agent.StepValue, Result: json.RawMessage(result)}
}

// Conform reports a record it cannot read, or one that names a gate or step the flow does not
// have, as a divergence naming the record, and never panics on it.
func TestConform_DivergenceFromUnreadableOrForeignRecords(t *testing.T) {
	flow := conformFlow(t)
	req, _ := json.Marshal(agent.ApprovalRequest{ToolUseID: "node:ghost"})
	for name, tc := range map[string]struct {
		rec  agent.Record
		want string
	}{
		"run start that is not a run start":    {valueRec(runStartStep, `"x"`), "run:start (unreadable run start)"},
		"run start of another flow":            {valueRec(runStartStep, `{"kind":"flow","flow":{"name":"other"}}`), "run:start (not a run of this flow)"},
		"topology digest that is not a string": {valueRec(flowDigestStep, `1`), "flow:digest (unreadable topology digest)"},
		"topology digest of another flow":      {valueRec(flowDigestStep, `"deadbeef"`), "flow:digest (ran against a different topology)"},
		"topology record that is not one":      {valueRec(TopologyStep, `"x"`), "flow:topology (unreadable topology)"},
		"topology record of another flow":      {valueRec(TopologyStep, `{"flow":"other"}`), "flow:topology (not this flow's topology)"},
		"switch choice that is not a string":   {valueRec("switch:entry", `1`), "switch:entry (unreadable switch choice)"},
		"switch choice of an undeclared arm":   {valueRec("switch:entry", `"ghost"`), "switch:entry -> ghost (unreachable arm taken)"},
		"switch over an undeclared node":       {valueRec("switch:ghost", `"high"`), "switch:ghost (switch over undeclared node)"},
		"completion by another flow":           {valueRec(runCompleteStep, `{"flow":"other"}`), "run:complete (not completed by this flow)"},
		"approval request that does not read":  {valueRec("approval-request:zz", `{}`), "approval-request:zz (unreadable approval request)"},
		"approval record of an unreached gate": {valueRec("approval-pending:zz", `{}`), "approval-pending:zz (approval record of a gate the run never reached)"},
		"approval tally of an unreached gate":  {valueRec("approval-tally:zz", `{}`), "approval-tally:zz (approval record of a gate the run never reached)"},
		"request of an undeclared step":        {valueRec(agent.ApprovalRequestStep("node:ghost"), string(req)), "(approval gate of an undeclared or ungated step)"},
		"decision on an ungated step":          {agent.Record{Name: "approval:x", Kind: agent.StepApproval, ToolUseID: "node:entry", Result: json.RawMessage(`{}`)}, "approval:x (decision on an undeclared or ungated step)"},
		"attempt for an undeclared step":       {agent.Record{Name: "attempt:step:node:ghost", Kind: agent.StepAttempt}, "(attempt for undeclared step)"},
		"attempt that is not an attempt":       {valueRec("attempt:step:node:entry", `{}`), "(unexpected step)"},
		"a result of an undeclared node":       {valueRec("node:ghost", `1`), "node:ghost (unexpected step)"},
	} {
		ok, diffs := injected(t, flow, tc.rec)
		if ok || !strings.Contains(strings.Join(diffs, "\n"), tc.want) {
			t.Errorf("%s: ok=%v diffs=%q, want a divergence containing %q", name, ok, diffs, tc.want)
		}
	}
	// A node result with no run start or topology is progress without a beginning.
	ok, diffs := injected(t, flow, valueRec("node:entry", `2`))
	joined := strings.Join(diffs, "\n")
	if ok || !strings.Contains(joined, "run:start (missing") || !strings.Contains(joined, "flow:digest (missing") {
		t.Errorf("a node record alone: ok=%v diffs=%q", ok, diffs)
	}
}

type unreadableStore struct{ agent.Store }

func (unreadableStore) Load(context.Context, string, int64) iter.Seq2[agent.Entry, error] {
	return func(yield func(agent.Entry, error) bool) { yield(agent.Entry{}, errors.New("journal unreadable")) }
}

func TestConform_JournalReadFailureIsAnErrorNotAVerdict(t *testing.T) {
	flow := conformFlow(t)
	j := agenttest.MustJournal(unreadableStore{agent.NewMemStore()})
	ok, diffs, err := flow.Conform(context.Background(), j, "r")
	if err == nil || !strings.Contains(err.Error(), `conform run "r": load history`) || !strings.Contains(err.Error(), "journal unreadable") {
		t.Errorf("err = %v", err)
	}
	if ok || diffs != nil {
		t.Errorf("a journal that cannot be read conformed: ok=%v diffs=%v", ok, diffs)
	}
}

func TestSplitIter(t *testing.T) {
	for in, want := range map[string]struct {
		rest string
		n    int
	}{
		"node":                           {"node", -1},
		"iter:3:body":                    {"body", 3},
		"iter:0:body":                    {"body", 0},
		"iter:12:a:b":                    {"a:b", 12},
		"iter:03:body":                   {"iter:03:body", -1}, // a leading zero is not how an iteration is written
		"iter:x:body":                    {"iter:x:body", -1},
		"iter:3":                         {"iter:3", -1}, // no node after the count
		"iter::body":                     {"iter::body", -1},
		"iter:99999999999999999999:body": {"iter:99999999999999999999:body", -1}, // overflows an int
	} {
		if rest, n := splitIter(in); rest != want.rest || n != want.n {
			t.Errorf("splitIter(%q) = %q, %d; want %q, %d", in, rest, n, want.rest, want.n)
		}
	}
}

func TestIsDigitsAndDecodeSwitchChoice(t *testing.T) {
	for in, want := range map[string]bool{"": false, "0": true, "123": true, "12a": false, "-1": false, "１": false} {
		if got := isDigits(in); got != want {
			t.Errorf("isDigits(%q) = %v", in, got)
		}
	}
	if got, err := decodeSwitchChoice(nil); got != "" || err != nil {
		t.Errorf("an empty result = %q, %v; want no arm taken", got, err)
	}
	if got, err := decodeSwitchChoice(json.RawMessage(`"high"`)); got != "high" || err != nil {
		t.Errorf("decodeSwitchChoice = %q, %v", got, err)
	}
	if _, err := decodeSwitchChoice(json.RawMessage(`{"a":1}`)); err == nil {
		t.Error("an object as a choice was accepted")
	}
}

// JournaledTopology refuses a topology record it cannot read, one without its digest, and one
// whose digest is not the recorded one.
func TestJournaledTopology_Refusals(t *testing.T) {
	flow := conformFlow(t)
	topo := flow.Topology()
	raw, _ := json.Marshal(topo)
	dig, _ := json.Marshal(flow.Digest())
	other, _ := json.Marshal("0000")
	for name, tc := range map[string]struct {
		recs []agent.Record
		ok   bool
		want string
	}{
		"no topology record":     {nil, false, ""},
		"unreadable topology":    {[]agent.Record{valueRec(TopologyStep, `"x"`)}, true, "decode flow:topology"},
		"no digest record":       {[]agent.Record{valueRec(TopologyStep, string(raw))}, true, "no readable flow:digest"},
		"unreadable digest":      {[]agent.Record{valueRec(TopologyStep, string(raw)), valueRec(flowDigestStep, `1`)}, true, "no readable flow:digest"},
		"digest of another flow": {[]agent.Record{valueRec(TopologyStep, string(raw)), valueRec(flowDigestStep, string(other))}, true, "the journaled topology's digest is"},
		"consistent":             {[]agent.Record{valueRec(TopologyStep, string(raw)), valueRec(flowDigestStep, string(dig))}, true, ""},
	} {
		_, _, ok, err := JournaledTopology(tc.recs)
		if ok != tc.ok || (tc.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: ok=%v err=%v; want ok=%v err containing %q", name, ok, err, tc.ok, tc.want)
		}
	}
}
