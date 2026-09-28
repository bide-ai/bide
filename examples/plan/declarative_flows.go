package main

// This file extends the declarative (declarative config) demonstration to the three config
// features beyond the linear triage flow: fan-in (a join), a bounded loop (a switch When
// arm with a loopMax back-edge), and per-node safety. Each is authored as data that
// references registered Go blocks by name, loaded into the Go builder, run, conformed,
// and asserted to share the code-built flow's topology Digest (config == code). See
// ../../docs/guides/flows.md ("Declarative config").
//
// As with demoDeclarative, these run only on the clean demo path against their own in-memory
// stores, so they never perturb the crash/resume e2e that drives the sqlite journal.

import (
	"context"
	"fmt"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/plan"
)

// LoopState is the single loop-carried value the bounded-loop demo threads through its
// back-edge: a countdown N the refine step decrements, plus a Trace that records one marker
// per body pass so the terminal output can show how many times the loop ran. Because the
// value routed on the back-edge re-enters the loop head, its type equals the head's input
// type (LoopState), which LoopBack enforces.
type LoopState struct {
	N     int    `json:"n"`
	Trace string `json:"trace"`
}

// diamondConfig is the canonical fan-out-then-fan-in diamond authored as data: an entry
// "split" fans out to two producers "y" and "z" (two edges out of one node), and a "join"
// named "merge" fans them back in through the registered Join2 merge block "mergeBlock".
// The node order (split, y, z), the two fan-out edges, then the join (which appends the
// merge node and its y->merge, z->merge edges) mirror buildDiamondByHand exactly, so the
// loaded flow's Digest equals the code-built one. The join carries safety "readonly" to
// illustrate the field: a fan-in over pure producers takes no external effect, so it opts
// out of halt-on-ambiguous-crash. Types are not restated; they flow from the blocks.
const diamondConfig = `{
  "flow": "diamond",
  "in": "int",
  "out": "string",
  "entry": "split",
  "nodes": [
    {"name": "split", "block": "split"},
    {"name": "y",     "block": "y"},
    {"name": "z",     "block": "z"}
  ],
  "wiring": [
    {"edge": ["split", "y"]},
    {"edge": ["split", "z"]},
    {"join": "merge", "inputs": ["y", "z"], "merge": "mergeBlock", "safety": "readonly"}
  ]
}`

// loopConfig is the canonical bounded countdown loop authored as data: seed -> refine
// (the loop head) -> check (the loop switch). The switch's When arm carries a loopMax of
// 10, a bounded back-edge that routes to refine while N>0; the Else arm exits to done. The
// node order, the two forward edges, and the arm order/bound mirror buildCountdownLoopByHand
// exactly, so the loaded flow's Digest equals the code-built one. loopMax bounds the
// back-edge so the graph stays finite; the predicate ("again") stays registered Go.
const loopConfig = `{
  "flow": "countdown",
  "in": "int",
  "out": "string",
  "entry": "seed",
  "nodes": [
    {"name": "seed",   "block": "seed"},
    {"name": "refine", "block": "refine"},
    {"name": "check",  "block": "check"},
    {"name": "done",   "block": "done"}
  ],
  "wiring": [
    {"edge": ["seed", "refine"]},
    {"edge": ["refine", "check"]},
    {"switch": "check", "when": [{"pred": "again", "to": "refine", "loopMax": 10}], "else": "done"}
  ]
}`

// buildDiamondRegistry registers the split/y/z steps and the two-input merge block the
// diamond config references by name. The bodies are identical to buildDiamondByHand, so the
// config and the code describe the same diamond and run to the same merged output. Every
// registration is checked; this fixed set has no duplicates.
func buildDiamondRegistry() (*plan.Registry, error) {
	reg := plan.NewRegistry()

	// split: int -> int, the entry that fans out to y and z.
	if err := plan.RegisterStep(reg, "split", func(n int) (int, error) { return n * 2, nil }); err != nil {
		return nil, err
	}
	// y: int -> int, one fan-out arm.
	if err := plan.RegisterStep(reg, "y", func(n int) (int, error) { return n + 1, nil }); err != nil {
		return nil, err
	}
	// z: int -> string, the other fan-out arm.
	if err := plan.RegisterStep(reg, "z", func(n int) (string, error) { return fmt.Sprintf("z%d", n), nil }); err != nil {
		return nil, err
	}
	// mergeBlock: the Join2 merge fanning y (int) and z (string) back into one string. Load
	// checks its arity (2) against the join's declared input count and its input types
	// against y's and z's outputs.
	if err := plan.RegisterJoin2(reg, "mergeBlock", func(a int, s string) (string, error) {
		return fmt.Sprintf("%s+%d", s, a), nil
	}); err != nil {
		return nil, err
	}
	return reg, nil
}

// buildDiamondByHand builds the same fan-out-then-fan-in diamond with the Go builder,
// so its Digest can be compared against a Load of diamondConfig, proving config == code for
// a fan-in flow. The node order, edges, join, and ReadOnly on the join all match the config.
func buildDiamondByHand() (*plan.Flow[int, string], error) {
	b := plan.New[int, string]("diamond")
	split := b.Step("split", func(n int) (int, error) { return n * 2, nil })
	y := b.Step("y", func(n int) (int, error) { return n + 1, nil })
	z := b.Step("z", func(n int) (string, error) { return fmt.Sprintf("z%d", n), nil })
	b.Edge(split, y)
	b.Edge(split, z)
	b.Join2("merge", y, z, func(a int, s string) (string, error) {
		return fmt.Sprintf("%s+%d", s, a), nil
	}, plan.ReadOnly())
	flow, err := b.Build()
	if err != nil {
		return nil, fmt.Errorf("build diamond by hand: %w", err)
	}
	return flow, nil
}

// buildLoopRegistry registers the seed/refine/check/done steps and the loop predicate the
// loop config references by name. The bodies are identical to buildCountdownLoopByHand, so
// the config and the code describe the same bounded loop and run to the same output.
func buildLoopRegistry() (*plan.Registry, error) {
	reg := plan.NewRegistry()

	// seed: int -> LoopState, the entry that primes the countdown.
	if err := plan.RegisterStep(reg, "seed", func(n int) (LoopState, error) {
		return LoopState{N: n, Trace: "seed"}, nil
	}); err != nil {
		return nil, err
	}
	// refine: LoopState -> LoopState, the loop head. It decrements N and appends a marker
	// so the terminal output shows one entry per body pass.
	if err := plan.RegisterStep(reg, "refine", func(s LoopState) (LoopState, error) {
		return LoopState{N: s.N - 1, Trace: s.Trace + "|refine"}, nil
	}); err != nil {
		return nil, err
	}
	// check: LoopState -> LoopState, the loop switch's switched node (a pass-through).
	if err := plan.RegisterStep(reg, "check", func(s LoopState) (LoopState, error) { return s, nil }); err != nil {
		return nil, err
	}
	// done: LoopState -> string, the loop exit terminal.
	if err := plan.RegisterStep(reg, "done", func(s LoopState) (string, error) {
		return fmt.Sprintf("done N=%d trace=%s", s.N, s.Trace), nil
	}); err != nil {
		return nil, err
	}
	// again: the loop-back predicate over LoopState. The When arm loops back while it holds.
	if err := plan.RegisterPredicate(reg, "again", func(s LoopState) bool { return s.N > 0 }); err != nil {
		return nil, err
	}
	return reg, nil
}

// buildCountdownLoopByHand builds the same bounded loop with the Go builder, so its
// Digest can be compared against a Load of loopConfig, proving config == code for a bounded
// loop. The node order, the two forward edges, and the LoopBack(10)/Else arm order match the
// config's loopMax of 10.
func buildCountdownLoopByHand() (*plan.Flow[int, string], error) {
	b := plan.New[int, string]("countdown")
	seed := b.Step("seed", func(n int) (LoopState, error) {
		return LoopState{N: n, Trace: "seed"}, nil
	})
	refine := b.Step("refine", func(s LoopState) (LoopState, error) {
		return LoopState{N: s.N - 1, Trace: s.Trace + "|refine"}, nil
	})
	check := b.Step("check", func(s LoopState) (LoopState, error) { return s, nil })
	done := b.Step("done", func(s LoopState) (string, error) {
		return fmt.Sprintf("done N=%d trace=%s", s.N, s.Trace), nil
	})
	b.Edge(seed, refine)
	b.Edge(refine, check)
	b.Switch(check,
		plan.LoopBack(10, func(s LoopState) bool { return s.N > 0 }, refine),
		plan.Else(done),
	)
	flow, err := b.Build()
	if err != nil {
		return nil, fmt.Errorf("build countdown loop by hand: %w", err)
	}
	return flow, nil
}

// demoDeclarativeJoin runs the fan-in (join) config demonstration on the clean demo path. It loads
// the diamond from JSON against a registry of the same blocks (including the RegisterJoin2
// merge), prints the config-derived topology, runs it to the merged output, conforms the
// run, and asserts the config-loaded flow's Digest() EQUALS the code-built diamond's: the
// config and the Go describe the same fan-in topology. A config-loaded join flow inherits
// Conform and the topology Digest, so it is cryptographically conformable to its config
// exactly as a code-built join is to its diagram. It uses an in-memory store and its own run
// id, so it never touches the sqlite journal the code-built demo drives.
func demoDeclarativeJoin(ctx context.Context) {
	fmt.Println()
	fmt.Println("== Declarative config: a fan-in (join) diamond, authored as declarative config ==")

	reg, err := buildDiamondRegistry()
	if err != nil {
		fatal(fmt.Errorf("build diamond registry: %w", err))
	}

	loaded, err := plan.Load[int, string]([]byte(diamondConfig), reg)
	if err != nil {
		fatal(fmt.Errorf("load diamond config: %w", err))
	}

	fmt.Println("Declared topology from config (flow.RenderMermaid):")
	fmt.Println(loaded.RenderMermaid())

	store := agent.NewMemStore()
	const runID = "diamond-config-demo"
	out, err := loaded.Run(ctx, store, runID, 3)
	if err != nil {
		fatal(fmt.Errorf("run config-loaded diamond: %w", err))
	}
	fmt.Printf("Config-loaded join run output (merged fan-in): %q (split(3)=6; y=7; z=\"z6\"; merge fans them in)\n", out)

	ok, diffs, err := loaded.Conform(ctx, store, runID)
	if err != nil {
		fatal(fmt.Errorf("conform config-loaded diamond: %w", err))
	}
	if ok {
		fmt.Println("Conform: the config-loaded join run followed the declared graph (no diffs).")
	} else {
		fmt.Printf("Conform: the config-loaded join run DIVERGED from the declared graph: %v\n", diffs)
	}

	hand, err := buildDiamondByHand()
	if err != nil {
		fatal(err)
	}
	configDigest := loaded.Digest()
	codeDigest := hand.Digest()
	if configDigest == codeDigest {
		fmt.Printf("Config-conformance (join): config Digest() == code-built Digest() (%s): the config and the Go describe the same fan-in topology.\n", configDigest)
	} else {
		fmt.Printf("Config-conformance (join): FAILED (config Digest() %s != code-built Digest() %s).\n", configDigest, codeDigest)
	}
}

// demoDeclarativeLoop runs the bounded-loop config demonstration on the clean demo path. It loads
// the countdown loop from JSON (a switch whose When arm carries a loopMax back-edge to the
// loop head, referencing a registered predicate) against a registry of the same blocks,
// prints the config-derived topology, runs it (input 3 iterates refine three times then
// exits at N=0), conforms the run, and asserts the config-loaded flow's Digest() EQUALS the
// code-built loop's: the config and the Go describe the same bounded loop. It uses an
// in-memory store and its own run id, so it never touches the sqlite journal.
func demoDeclarativeLoop(ctx context.Context) {
	fmt.Println()
	fmt.Println("== Declarative config: a bounded loop (loopMax back-edge), authored as declarative config ==")

	reg, err := buildLoopRegistry()
	if err != nil {
		fatal(fmt.Errorf("build loop registry: %w", err))
	}

	loaded, err := plan.Load[int, string]([]byte(loopConfig), reg)
	if err != nil {
		fatal(fmt.Errorf("load loop config: %w", err))
	}

	fmt.Println("Declared topology from config (flow.RenderMermaid):")
	fmt.Println(loaded.RenderMermaid())

	store := agent.NewMemStore()
	const runID = "loop-config-demo"
	out, err := loaded.Run(ctx, store, runID, 3)
	if err != nil {
		fatal(fmt.Errorf("run config-loaded loop: %w", err))
	}
	fmt.Printf("Config-loaded loop run output (iterated then exited): %q\n", out)

	ok, diffs, err := loaded.Conform(ctx, store, runID)
	if err != nil {
		fatal(fmt.Errorf("conform config-loaded loop: %w", err))
	}
	if ok {
		fmt.Println("Conform: the config-loaded loop run followed the declared graph (no diffs).")
	} else {
		fmt.Printf("Conform: the config-loaded loop run DIVERGED from the declared graph: %v\n", diffs)
	}

	hand, err := buildCountdownLoopByHand()
	if err != nil {
		fatal(err)
	}
	configDigest := loaded.Digest()
	codeDigest := hand.Digest()
	if configDigest == codeDigest {
		fmt.Printf("Config-conformance (loop): config Digest() == code-built Digest() (%s): the config and the Go describe the same bounded loop.\n", configDigest)
	} else {
		fmt.Printf("Config-conformance (loop): FAILED (config Digest() %s != code-built Digest() %s).\n", configDigest, codeDigest)
	}
}
