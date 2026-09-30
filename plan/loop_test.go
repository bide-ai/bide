package plan

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// loopState is the single loop-carried value: a countdown a bounded loop decrements
// until it reaches zero. Because the value routed on the back-edge re-enters the loop
// head, its type MUST equal the head's input type (loopState here); LoopBack enforces
// that at the call site through M unification.
type loopState struct {
	N     int
	Trace string // accumulates a marker per iteration so we can assert the body ran each pass
}

// buildCountdownLoop builds the canonical bounded loop:
//
//	seed (entry) -> refine (head) -> check (switch)
//	                   ^                 |
//	                   |  LoopBack(max)  |  When N>0
//	                   +-----------------+
//	                                     |  Else
//	                                     v
//	                                    done (terminal)
//
// refine decrements N (and appends to the trace); check is the loop Switch; the
// LoopBack arm re-enters refine while N>0, the Else exits to done. bump counts refine
// executions so a test can assert the body ran the expected number of times. opts are
// applied to the refine (head) node so a test can vary its Safety for the crash sweep.
func buildCountdownLoop(max int, bump *int, opts ...NodeOption) (*Flow[int, string], error) {
	b := New[int, string]("countdown")
	seed := b.Step("seed", func(_ context.Context, n int) (loopState, error) {
		return loopState{N: n, Trace: "seed"}, nil
	})
	refine := b.Step("refine", func(_ context.Context, s loopState) (loopState, error) {
		if bump != nil {
			*bump++
		}
		return loopState{N: s.N - 1, Trace: s.Trace + "|refine"}, nil
	}, opts...)
	check := b.Step("check", func(_ context.Context, s loopState) (loopState, error) { return s, nil })
	done := b.Step("done", func(_ context.Context, s loopState) (string, error) {
		return fmt.Sprintf("done N=%d trace=%s", s.N, s.Trace), nil
	})
	b.Edge(seed, refine)
	b.Edge(refine, check)
	b.Switch(check,
		LoopBack(max, func(s loopState) bool { return s.N > 0 }, refine).Named("again"),
		Else(done),
	)
	return b.Build()
}

// TestLoopIteratesThenExits runs a bounded loop that iterates a few times and exits.
// With input 3: refine decrements 3 -> 2 -> 1 -> 0; the loop-back arm (N>0) is taken
// while N is 2, 1; the Else exits when N reaches 0. refine therefore runs 3 times and
// the terminal reports N=0.
func TestLoopIteratesThenExits(t *testing.T) {
	var bump int
	flow, err := buildCountdownLoop(10, &bump)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	out, err := flow.Run(context.Background(), agent.NewMemStore(), "loop-run", 3)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if bump != 3 {
		t.Fatalf("refine ran %d times, want 3 (3 -> 2 -> 1 -> 0)", bump)
	}
	if !strings.HasPrefix(out, "done N=0 ") {
		t.Fatalf("terminal output = %q, want it to report N=0", out)
	}
	if want := "seed|refine|refine|refine"; !strings.Contains(out, want) {
		t.Fatalf("terminal trace does not show three body passes: %q (want it to contain %q)", out, want)
	}
}

// TestLoopRespectsMaxAndErrors asserts the runaway guard: a loop whose predicate would
// never let it exit within max iterations returns a clear error naming the bound
// rather than looping forever. Input 100 with max 4 cannot count down to zero in four
// passes, so Run must error.
func TestLoopRespectsMaxAndErrors(t *testing.T) {
	var bump int
	flow, err := buildCountdownLoop(4, &bump)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	_, err = flow.Run(context.Background(), agent.NewMemStore(), "loop-runaway", 100)
	if err == nil {
		t.Fatal("Run accepted a loop that never exits within its bound")
	}
	if !strings.Contains(err.Error(), "runaway") || !strings.Contains(err.Error(), "4") {
		t.Fatalf("runaway error does not name the bound: %v", err)
	}
	// The body ran exactly max times before the guard tripped (iterations 0..3).
	if bump != 4 {
		t.Fatalf("refine ran %d times before the guard tripped, want 4 (the bound)", bump)
	}
}

// TestLoopBuildRequiresExitArm asserts a Switch whose only arm is a LoopBack (no When
// or Else exit) fails Build: the loop could never terminate. The error names the
// offending Switch.
func TestLoopBuildRequiresExitArm(t *testing.T) {
	b := New[int, string]("no-exit")
	seed := b.Step("seed", func(_ context.Context, n int) (loopState, error) { return loopState{N: n}, nil })
	refine := b.Step("refine", func(_ context.Context, s loopState) (loopState, error) { return s, nil })
	b.Edge(seed, refine)
	b.Switch(refine, LoopBack(5, func(s loopState) bool { return true }, refine))
	_, err := b.Build()
	if err == nil {
		t.Fatal("Build accepted a loop with no exit arm")
	}
	if !strings.Contains(err.Error(), "exit") {
		t.Fatalf("Build error does not mention the missing exit arm: %v", err)
	}
}

// TestLoopBuildRequiresPositiveBound asserts a LoopBack with a non-positive max fails
// Build: a zero or negative bound cannot terminate cleanly.
func TestLoopBuildRequiresPositiveBound(t *testing.T) {
	b := New[int, string]("bad-bound")
	seed := b.Step("seed", func(_ context.Context, n int) (loopState, error) { return loopState{N: n}, nil })
	refine := b.Step("refine", func(_ context.Context, s loopState) (loopState, error) { return s, nil })
	done := b.Step("done", func(_ context.Context, s loopState) (string, error) { return "done", nil })
	b.Edge(seed, refine)
	b.Switch(refine,
		LoopBack(0, func(s loopState) bool { return s.N > 0 }, refine),
		Else(done),
	)
	_, err := b.Build()
	if err == nil {
		t.Fatal("Build accepted a loop with a non-positive bound")
	}
	if !strings.Contains(err.Error(), "positive") {
		t.Fatalf("Build error does not mention the bound: %v", err)
	}
}

// TestLoopBuildRejectsNonAncestorHead asserts a LoopBack whose target is NOT an
// ancestor of the Switch (so the back-edge is not a real cycle) fails Build. Here the
// "head" is a node the Switch does not reach in the forward graph.
func TestLoopBuildRejectsNonAncestorHead(t *testing.T) {
	b := New[int, string]("not-a-loop")
	seed := b.Step("seed", func(_ context.Context, n int) (loopState, error) { return loopState{N: n}, nil })
	refine := b.Step("refine", func(_ context.Context, s loopState) (loopState, error) { return s, nil })
	sidecar := b.Step("sidecar", func(_ context.Context, s loopState) (loopState, error) { return s, nil })
	done := b.Step("done", func(_ context.Context, s loopState) (string, error) { return "done", nil })
	b.Edge(seed, refine)
	// sidecar is reachable only as the Else target; it is not an ancestor of refine.
	b.Switch(refine,
		LoopBack(5, func(s loopState) bool { return s.N > 0 }, sidecar), // back to a non-ancestor
		Else(done),
	)
	_ = sidecar
	_, err := b.Build()
	if err == nil {
		t.Fatal("Build accepted a LoopBack to a non-ancestor head")
	}
	if !strings.Contains(err.Error(), "ancestor") {
		t.Fatalf("Build error does not mention the ancestor requirement: %v", err)
	}
}

// TestLoopConforms asserts a looped run conforms: every iteration-scoped journal key
// (iter:<n>:<node>, attempt:iter:<n>:<node>, switch:iter:<n>:<over>) maps back to a
// declared node, so there are no divergences.
func TestLoopConforms(t *testing.T) {
	flow, err := buildCountdownLoop(10, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	mem := agent.NewMemStore()
	ctx := context.Background()
	if _, err := flow.Run(ctx, mem, "loop-conform", 3); err != nil {
		t.Fatalf("Run: %v", err)
	}
	ok, diffs, err := flow.Conform(ctx, mem, "loop-conform")
	if err != nil {
		t.Fatalf("Conform: %v", err)
	}
	if !ok {
		t.Fatalf("Conform flagged a valid looped run as divergent: %v", diffs)
	}
}

// TestLoopConformFlagsUndeclaredStep asserts conformance still catches an undeclared
// step in a looped run: an iteration-scoped record for a node the flow never declared
// is a divergence (the iter:<n>: prefix is stripped, and the remainder does not name a
// declared node).
func TestLoopConformFlagsUndeclaredStep(t *testing.T) {
	flow, err := buildCountdownLoop(10, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	mem := agent.NewMemStore()
	ctx := context.Background()
	if _, err := flow.Run(ctx, mem, "loop-diverge", 3); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Inject an iteration-scoped record for a node that was never declared.
	if _, err := mem.Do(ctx, "loop-diverge", "iter:0:ghost", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: []byte("null")}, nil
	}); err != nil {
		t.Fatalf("inject: %v", err)
	}
	ok, diffs, err := flow.Conform(ctx, mem, "loop-diverge")
	if err != nil {
		t.Fatalf("Conform: %v", err)
	}
	if ok {
		t.Fatal("Conform reported ok despite an undeclared iteration-scoped step")
	}
	found := false
	for _, d := range diffs {
		if strings.Contains(d, "ghost") {
			found = true
		}
	}
	if !found {
		t.Fatalf("Conform diffs do not name the undeclared step: %v", diffs)
	}
}

// TestLoopDigestStableAndShapeSensitive asserts the looped flow's Digest is stable
// across builds of the same shape, differs from the SAME flow without the loop
// (acyclic), and differs when the max bound changes (the bound is part of the
// structure), while never depending on the runtime iteration count.
func TestLoopDigestStableAndShapeSensitive(t *testing.T) {
	a, err := buildCountdownLoop(10, nil)
	if err != nil {
		t.Fatalf("build a: %v", err)
	}
	bb, err := buildCountdownLoop(10, nil)
	if err != nil {
		t.Fatalf("build b: %v", err)
	}
	if a.Digest() != bb.Digest() {
		t.Fatalf("looped digest not stable across builds:\n a=%s\n b=%s", a.Digest(), bb.Digest())
	}

	// Same flow but the arm is a forward Switch (no loop): distinct shape, distinct digest.
	acyclic := New[int, string]("countdown")
	s := acyclic.Step("seed", func(_ context.Context, n int) (loopState, error) { return loopState{N: n}, nil })
	r := acyclic.Step("refine", func(_ context.Context, st loopState) (loopState, error) { return st, nil })
	ch := acyclic.Step("check", func(_ context.Context, st loopState) (loopState, error) { return st, nil })
	again := acyclic.Step("again", func(_ context.Context, st loopState) (string, error) { return "again", nil })
	dn := acyclic.Step("done", func(_ context.Context, st loopState) (string, error) { return "done", nil })
	acyclic.Edge(s, r)
	acyclic.Edge(r, ch)
	acyclic.Switch(ch,
		When(func(st loopState) bool { return st.N > 0 }, again), // forward, not a back-edge
		Else(dn),
	)
	acyclicFlow, err := acyclic.Build()
	if err != nil {
		t.Fatalf("build acyclic: %v", err)
	}
	if acyclicFlow.Digest() == a.Digest() {
		t.Fatal("looped flow digest equals the acyclic flow digest; the loop structure is not committed")
	}

	// Different max bound: distinct digest (the bound is part of the loop structure).
	diffMax, err := buildCountdownLoop(20, nil)
	if err != nil {
		t.Fatalf("build diffMax: %v", err)
	}
	if diffMax.Digest() == a.Digest() {
		t.Fatal("changing the loop max did not change the digest; the bound is not committed")
	}

	// The runtime iteration count must NOT affect the digest: two runs of the SAME flow
	// with different inputs (hence different iteration counts) record one digest, and the
	// flow's Digest is unchanged by having run.
	before := a.Digest()
	recorded := map[string]string{}
	for _, n := range []int{1, 5} {
		store := agent.NewMemStore()
		runID := fmt.Sprintf("iter-%d", n)
		if _, err := a.Run(context.Background(), store, runID, n); err != nil {
			t.Fatalf("run with %d iterations: %v", n, err)
		}
		recs, _ := store.History(context.Background(), runID)
		for _, r := range recs {
			if r.Name == flowDigestStep {
				recorded[runID] = string(r.Result)
			}
		}
	}
	if len(recorded) != 2 || recorded["iter-1"] != recorded["iter-5"] {
		t.Fatalf("runs with different iteration counts recorded different digests: %v", recorded)
	}
	if a.Digest() != before {
		t.Fatal("running the flow changed its Digest")
	}
}

// crashCountdownLoop is buildCountdownLoop for the crash sweep: it builds the same
// bounded loop against a fresh set of call counters so each run can assert at-most-once
// per node across a crash mid-loop. refine defaults to the conservative
// halt-on-ambiguous-crash (no Safety opt-in), so the guard is exercised per iteration.
func crashCountdownLoop(refineCalls *int) (*Flow[int, string], error) {
	return buildCountdownLoop(10, refineCalls)
}

// TestLoopCrashSweepAtMostOncePerIteration sweeps a crash at every write point of a
// looped run and asserts the loop-body effect fires AT MOST ONCE PER ITERATION across
// crash and resume, resuming into the correct iteration. refine runs once per
// iteration (input 3 -> three iterations), so across a clean completion it fires
// exactly three times; a crash mid-loop must not double-fire any iteration's refine.
// Each run ends completed (the terminal output) or halted (*HaltAmbiguous naming a
// declared step, possibly an iteration-scoped key). It reuses the crashFlowStore DST
// harness from flow_dst_test.go.
func TestLoopCrashSweepAtMostOncePerIteration(t *testing.T) {
	crashed := false
	haltSeen := false
	for crashAt := 1; crashAt <= 64; crashAt++ {
		var refineCalls int
		mem := agent.NewMemStore()

		run := func(crashPoint int) error {
			store := &crashFlowStore{inner: mem, crashAt: crashPoint}
			flow, buildErr := crashCountdownLoop(&refineCalls)
			if buildErr != nil {
				return buildErr
			}
			_, runErr := flow.Run(context.Background(), store, "loop-sweep", 3)
			return runErr
		}

		err := run(crashAt)
		if errors.Is(err, errCrash) {
			crashed = true
		}
		for errors.Is(err, errCrash) {
			err = run(0) // resume without further crashes
		}

		// At-most-once per iteration: input 3 drives exactly three refine passes across
		// the whole crash+resume lifecycle. A double-fire on any iteration pushes this
		// above three.
		if refineCalls > 3 {
			t.Fatalf("crashAt=%d: refine fired %d times, want at most 3 (one per iteration; DOUBLE FIRE)", crashAt, refineCalls)
		}

		var halt *HaltAmbiguous
		switch {
		case err == nil:
			if refineCalls != 3 {
				t.Fatalf("crashAt=%d: completed run fired refine %d times, want exactly 3", crashAt, refineCalls)
			}
		case errors.As(err, &halt):
			haltSeen = true
			if halt.Step == "" {
				t.Fatalf("crashAt=%d: halt named no step", crashAt)
			}
		default:
			t.Fatalf("crashAt=%d: unexpected terminal error: %v", crashAt, err)
		}

		if !errors.Is(err, errCrash) && !crashed && crashAt > 40 {
			break
		}
	}
	if !crashed {
		t.Fatal("no crash point was exercised: the sweep was vacuous")
	}
	if !haltSeen {
		t.Fatal("no crash point exercised the halt path inside the loop")
	}
}

// TestLoopResumeReplaysCompletedIterations asserts a plain resume (no crash) replays
// completed iterations rather than re-running them: the loop-body effect fires exactly
// three times across two full runs of the same runID, and the second run returns the
// same output by replay. The iteration-scoped journal keys memoize each iteration.
func TestLoopResumeReplaysCompletedIterations(t *testing.T) {
	var refineCalls int
	mem := agent.NewMemStore()
	flow, err := buildCountdownLoop(10, &refineCalls)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	first, err := flow.Run(context.Background(), mem, "loop-reuse", 3)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if refineCalls != 3 {
		t.Fatalf("after first run refine fired %d times, want 3", refineCalls)
	}
	second, err := flow.Run(context.Background(), mem, "loop-reuse", 3)
	if err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	if refineCalls != 3 {
		t.Fatalf("resume re-ran the loop body: refine fired %d times, want 3 (iterations must replay)", refineCalls)
	}
	if first != second {
		t.Fatalf("resume returned %q, want the replayed %q", second, first)
	}
}
