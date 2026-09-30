package plan

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// buildDiamond builds the canonical fan-out-then-fan-in diamond: an entry split
// fans out to two producers y and z (a producer with more than one outgoing edge),
// and a Join2 merges y and z into the terminal output. It exercises fan-out, fan-in,
// and the topological barrier (the join runs only after both y and z are journaled).
//
//	 split
//	 /    \
//	y      z
//	 \    /
//	 merge (Join2)
//
// The optional opts are applied to the join node (for example ReadOnly to opt it out
// of halt-on-ambiguous-crash), so a caller can vary the join's Safety.
func buildDiamond(opts ...NodeOption) (*Flow[int, string], error) {
	b := New[int, string]("diamond")
	split := b.Step("split", func(_ context.Context, n int) (int, error) { return n * 2, nil })
	y := b.Step("y", func(_ context.Context, n int) (int, error) { return n + 1, nil })
	z := b.Step("z", func(_ context.Context, n int) (string, error) { return fmt.Sprintf("z%d", n), nil })
	b.Edge(split, y)
	b.Edge(split, z)
	b.Join2("merge", y, z, func(_ context.Context, a int, s string) (string, error) {
		return fmt.Sprintf("%s+%d", s, a), nil
	}, opts...)
	return b.Build()
}

// TestJoinDiamondRunsSequentially runs the diamond to its merged output. With input
// 3: split -> 6; y -> 7; z -> "z6"; merge -> "z6+7". This proves fan-out and fan-in
// execute sequentially in topological order to the correct merged result.
func TestJoinDiamondRunsSequentially(t *testing.T) {
	flow, err := buildDiamond()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	mem := agent.NewMemStore()
	out, err := flow.Run(context.Background(), mem, "diamond-run", 3)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "z6+7" {
		t.Fatalf("merged output = %q, want %q", out, "z6+7")
	}

	// The journal must carry a result and attempt marker for every node, including
	// both fan-out branches and the join. No arm/switch record exists (no Switch).
	recs, err := mem.History(context.Background(), "diamond-run")
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	names := map[string]bool{}
	for _, r := range recs {
		names[r.Name] = true
	}
	for _, want := range []string{"node:split", "node:y", "node:z", "node:merge", "attempt:step:node:split", "attempt:step:node:y", "attempt:step:node:z", "attempt:step:node:merge"} {
		if !names[want] {
			t.Errorf("journal missing expected record %q", want)
		}
	}
}

// TestJoinDiamondConforms asserts a diamond run conforms: every journaled record
// maps to a declared node (a join's result and attempt marker map to the join), so
// there are no divergences.
func TestJoinDiamondConforms(t *testing.T) {
	flow, err := buildDiamond()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	mem := agent.NewMemStore()
	ctx := context.Background()
	if _, err := flow.Run(ctx, mem, "conform-run", 3); err != nil {
		t.Fatalf("Run: %v", err)
	}
	ok, diffs, err := flow.Conform(ctx, mem, "conform-run")
	if err != nil {
		t.Fatalf("Conform: %v", err)
	}
	if !ok {
		t.Errorf("Conform flagged a valid diamond run as divergent: %v", diffs)
	}
}

// TestJoinDigestStableAndShapeSensitive asserts the diamond's Digest is stable
// across independent builds of the same shape, and changes when the shape changes (a
// different join input order, or a differently-typed merge). This proves the join's
// ordered inputs and port types are part of the topology fingerprint.
func TestJoinDigestStableAndShapeSensitive(t *testing.T) {
	a, err := buildDiamond()
	if err != nil {
		t.Fatalf("build a: %v", err)
	}
	b, err := buildDiamond()
	if err != nil {
		t.Fatalf("build b: %v", err)
	}
	if a.Digest() != b.Digest() {
		t.Errorf("diamond digest not stable across builds:\n a=%s\n b=%s", a.Digest(), b.Digest())
	}
	base := a.Digest()

	// Swapped join input order (z, y) instead of (y, z): a distinct shape, so a
	// distinct digest, even though the same nodes and edges exist.
	swap := New[int, string]("diamond")
	sp := swap.Step("split", func(_ context.Context, n int) (int, error) { return n * 2, nil })
	sy := swap.Step("y", func(_ context.Context, n int) (int, error) { return n + 1, nil })
	sz := swap.Step("z", func(_ context.Context, n int) (string, error) { return fmt.Sprintf("z%d", n), nil })
	swap.Edge(sp, sy)
	swap.Edge(sp, sz)
	swap.Join2("merge", sz, sy, func(_ context.Context, s string, i int) (string, error) { // inputs swapped
		return fmt.Sprintf("%s+%d", s, i), nil
	})
	swapFlow, err := swap.Build()
	if err != nil {
		t.Fatalf("build swapped: %v", err)
	}
	if swapFlow.Digest() == base {
		t.Errorf("digest ignored the join input order (both %s)", base)
	}
}

// TestJoin3RunsSequentially builds a fan-out to three producers merged by Join3 and
// runs it to the merged output, proving arity-3 fan-in works end to end.
func TestJoin3RunsSequentially(t *testing.T) {
	b := New[int, string]("diamond3")
	split := b.Step("split", func(_ context.Context, n int) (int, error) { return n, nil })
	p := b.Step("p", func(_ context.Context, n int) (int, error) { return n + 1, nil })
	q := b.Step("q", func(_ context.Context, n int) (int, error) { return n + 2, nil })
	r := b.Step("r", func(_ context.Context, n int) (string, error) { return fmt.Sprintf("r%d", n), nil })
	b.Edge(split, p)
	b.Edge(split, q)
	b.Edge(split, r)
	b.Join3("merge", p, q, r, func(_ context.Context, a, bb int, s string) (string, error) {
		return fmt.Sprintf("%s|%d|%d", s, a, bb), nil
	})
	flow, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	out, err := flow.Run(context.Background(), agent.NewMemStore(), "d3", 5)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "r5|6|7" {
		t.Fatalf("Join3 output = %q, want %q", out, "r5|6|7")
	}
}

// TestJoinTypeMismatchIsBuildError asserts a join whose input producer's output type
// does not match the corresponding merge parameter type fails Build, naming the join
// and the offending input. The mismatch is constructed by hand-assembling the spec
// (the Go call site would otherwise reject it at compile time), mirroring how a
// hand-built or loaded spec could smuggle a mismatch past the compiler.
func TestJoinTypeMismatchIsBuildError(t *testing.T) {
	b := New[int, string]("mismatch")
	// split fans out to y (int) and z (string).
	split := b.Step("split", func(_ context.Context, n int) (int, error) { return n, nil })
	_ = b.Step("y", func(_ context.Context, n int) (int, error) { return n, nil })
	z := b.Step("z", func(_ context.Context, n int) (string, error) { return "z", nil })
	b.Edge(split, z)
	// Hand-append the fan-out edge and a join node whose FIRST port type (string)
	// disagrees with input y's output type (int): a type-mismatched join input.
	b.core.edges = append(b.core.edges, edge{from: "split", to: "y"})
	b.core.register(&node{
		name:       "merge",
		kind:       kindJoin,
		outType:    typeOf[string](),
		joinInputs: []string{"y", "z"},
		// Port 0 declared as string, but input y produces int: a type-mismatched input.
		joinInTypes: []reflect.Type{reflect.TypeFor[string](), reflect.TypeFor[string]()},
		merge: func(_ context.Context, _ []any) (any, error) {
			return "", nil
		},
	})
	b.core.edges = append(b.core.edges, edge{from: "y", to: "merge"}, edge{from: "z", to: "merge"})

	_, err := b.Build()
	if err == nil {
		t.Fatal("Build accepted a type-mismatched join input")
	}
	if !strings.Contains(err.Error(), "merge") || !strings.Contains(err.Error(), "y") {
		t.Errorf("Build error does not name the join and offending input: %v", err)
	}
}

// TestJoinInputGatedBySwitchIsBuildError asserts a join whose input is reachable only
// via one arm of a Switch (so the Switch can skip it while the other input runs) fails
// Build, naming the join and the gated input. This enforces the boundary: Join is for
// fan-in of branches that BOTH execute, not reconvergence of mutually-exclusive arms.
func TestJoinInputGatedBySwitchIsBuildError(t *testing.T) {
	b := New[int, string]("gated")
	// entry -> Switch{ When -> gatedY, Else -> other }. gatedY is reachable only via
	// the When arm. z is on the ungated spine (entry -> z), so a Join2(gatedY, z) has
	// one input a Switch can skip while the other runs.
	entry := b.Step("entry", func(_ context.Context, n int) (int, error) { return n, nil })
	gatedY := b.Step("gatedY", func(_ context.Context, n int) (int, error) { return n, nil })
	other := b.Step("other", func(context.Context, int) (string, error) { return "other", nil })
	z := b.Step("z", func(_ context.Context, n int) (string, error) { return "z", nil })
	b.Edge(entry, z)
	b.Switch(entry,
		When(func(n int) bool { return n > 0 }, gatedY),
		Else(other),
	)
	b.Join2("merge", gatedY, z, func(_ context.Context, a int, s string) (string, error) {
		return fmt.Sprintf("%s%d", s, a), nil
	})

	_, err := b.Build()
	if err == nil {
		t.Fatal("Build accepted a join whose input a Switch can skip")
	}
	if !strings.Contains(err.Error(), "merge") || !strings.Contains(err.Error(), "gatedY") {
		t.Errorf("Build error does not name the join and gated input: %v", err)
	}
}

// TestJoinDiamondCrashSweep sweeps a crash at every write point of the diamond and
// asserts at-most-once semantics reach the join: each node (including the join's
// merge) fires at most once across crash and resume, and each run ends either
// completed (the merged output) or halted (*agent.OutcomeUnknown naming a declared node).
// The join defaults non-idempotent, so a crash on its result write HALTS rather than
// re-running the merge. It reuses the crashFlowStore DST harness from
// flow_dst_test.go.
func TestJoinDiamondCrashSweep(t *testing.T) {
	crashed := false
	for crashAt := 1; crashAt <= 64; crashAt++ {
		var splitCalls, yCalls, zCalls, mergeCalls int
		mem := agent.NewMemStore()

		run := func(crashPoint int) error {
			store := &crashFlowStore{inner: mem, crashAt: crashPoint}
			bb := New[int, string]("diamond")
			split := bb.Step("split", func(_ context.Context, n int) (int, error) { splitCalls++; return n * 2, nil })
			y := bb.Step("y", func(_ context.Context, n int) (int, error) { yCalls++; return n + 1, nil })
			z := bb.Step("z", func(_ context.Context, n int) (string, error) { zCalls++; return fmt.Sprintf("z%d", n), nil })
			bb.Edge(split, y)
			bb.Edge(split, z)
			bb.Join2("merge", y, z, func(_ context.Context, a int, s string) (string, error) {
				mergeCalls++
				return fmt.Sprintf("%s+%d", s, a), nil
			})
			flow, buildErr := bb.Build()
			if buildErr != nil {
				return buildErr
			}
			_, runErr := flow.Run(context.Background(), store, "sweep", 3)
			return runErr
		}

		err := run(crashAt)
		if errors.Is(err, errCrash) {
			crashed = true
		}
		for errors.Is(err, errCrash) {
			err = run(0) // resume without further crashes
		}

		// At-most-once for every node body, including the join merge.
		if splitCalls > 1 || yCalls > 1 || zCalls > 1 || mergeCalls > 1 {
			t.Fatalf("crashAt=%d: a node body fired more than once (split=%d y=%d z=%d merge=%d)",
				crashAt, splitCalls, yCalls, zCalls, mergeCalls)
		}

		var halt *agent.OutcomeUnknown
		switch {
		case err == nil:
			// completed cleanly
		case errors.As(err, &halt):
			if n, ok := nodeOfKey(halt.Op.ID); !ok || n == "" || halt.Op.Kind != agent.OpStep {
				t.Fatalf("crashAt=%d: halt names %+v, want a node's Step", crashAt, halt.Op)
			}
		default:
			t.Fatalf("crashAt=%d: unexpected terminal error: %v", crashAt, err)
		}

		if !errors.Is(err, errCrash) && !crashed && crashAt > 40 {
			break // sweep exhausted the clean-run write count
		}
	}
	if !crashed {
		t.Fatal("no crash point was exercised: the sweep was vacuous")
	}
}
