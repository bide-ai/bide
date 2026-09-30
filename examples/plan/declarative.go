package main

// This file adds the declarative (declarative config) demonstration to the plan example.
// declarative config expresses a flow's TOPOLOGY as data (nodes, edges, switch arms) while BEHAVIOR
// stays as registered Go blocks referenced by name. A config loads into the same Go
// builder and produces the same *plan.Flow, so it inherits RenderMermaid, Conform, and
// the topology Digest unchanged. See
// ../../docs/guides/flows.md ("Declarative config").
//
// The headline this demonstration proves is CONFIG-CONFORMANCE: the config below and the
// code-built triage flow in buildFlow describe the same topology, so their Digest() values
// are EQUAL. Because a config-loaded flow inherits flow:digest and the audit inclusion
// proof, the same offline-verifiable "the run followed this topology" story holds for a
// flow authored as data.

import (
	"context"
	"fmt"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/plan"
)

// declarativeConfig is the order-triage flow expressed as a declarative JSON config. It restates the
// topology of buildFlow's code-built flow (classify entry, a Switch routing rush orders to
// reserve-and-finalize and everything else to decline) as pure data: nodes reference
// registered blocks by name, wiring is an ordered list of edges and switches. Types are
// NOT restated here; they flow from the registered blocks at load time. The optional
// in/out fields are documentation the loader cross-checks against Load's In/Out.
const declarativeConfig = `{
  "flow": "order-triage",
  "in": "main.Order",
  "out": "main.Receipt",
  "entry": "classify",
  "nodes": [
    {"name": "classify", "block": "classify"},
    {"name": "reserve",  "block": "reserve"},
    {"name": "finalize", "block": "finalize"},
    {"name": "decline",  "block": "decline"}
  ],
  "wiring": [
    {"switch": "classify", "when": [{"pred": "rush", "to": "reserve"}], "else": "decline"},
    {"edge": ["reserve", "finalize"]}
  ]
}`

// buildDeclarativeRegistry registers the SAME blocks the code-built flow uses, under the SAME
// journal-key names (classify/reserve/finalize/decline) plus the rush predicate. The block
// bodies are the clean, side-effect-free demo variants (the declarative demo never injects a
// crash), which is sound because the topology Digest commits to node names, kinds, and I/O
// types, not to node bodies: the config and the code describe the same SHAPE regardless of
// what each body does inside. Every registration is checked; a duplicate would surface at
// Load, but this fixed set has none.
func buildDeclarativeRegistry() (*plan.Registry, error) {
	reg := plan.NewRegistry()

	// classify: Order -> Assessment, the entry step the Switch routes on. Identical to the
	// code-built classify body.
	if err := plan.RegisterStep(reg, "classify", func(_ context.Context, o Order) (Assessment, error) {
		return Assessment{OrderID: o.ID, Amount: o.Amount, Rush: o.Amount > 100}, nil
	}); err != nil {
		return nil, err
	}

	// reserve: Assessment -> Reservation, the rush-arm step. In the code-built flow this is
	// the one non-idempotent effect; the declarative demo uses the clean variant with no witness
	// append or crash injection, since it only needs to run to completion.
	if err := plan.RegisterStep(reg, "reserve", func(_ context.Context, a Assessment) (Reservation, error) {
		return Reservation{OrderID: a.OrderID, Ref: "hold-" + a.OrderID}, nil
	}); err != nil {
		return nil, err
	}

	// finalize: Reservation -> Receipt, the rush-arm terminal.
	if err := plan.RegisterStep(reg, "finalize", func(_ context.Context, r Reservation) (Receipt, error) {
		return Receipt{OrderID: r.OrderID, Outcome: "reserved", Detail: r.Ref, Reserved: true}, nil
	}); err != nil {
		return nil, err
	}

	// decline: Assessment -> Receipt, the Else-arm terminal.
	if err := plan.RegisterStep(reg, "decline", func(_ context.Context, a Assessment) (Receipt, error) {
		return Receipt{OrderID: a.OrderID, Outcome: "declined", Detail: "below rush threshold"}, nil
	}); err != nil {
		return nil, err
	}

	// rush: the Switch predicate over Assessment. Load checks its M (Assessment) equals the
	// switched node's output type, a strict improvement over the Go builder.
	if err := plan.RegisterPredicate(reg, "rush", func(a Assessment) bool { return a.Rush }); err != nil {
		return nil, err
	}

	return reg, nil
}

// demoDeclarative runs the declarative demonstration on the no-flags demo path. It loads the flow from
// the JSON config against a registry of the same blocks, prints the config-derived declared
// topology, runs it against a store to a typed Receipt, conforms the run, and prints that
// the config-loaded flow's Digest() EQUALS the code-built flow's Digest(): the config and
// the Go describe the same topology. Because a config-loaded flow inherits flow:digest, it
// then reuses proveTopologyConformance to show the config-loaded run is offline-provable too.
//
// It uses an in-memory store and its own run id so it never touches the sqlite journal the
// code-built demo drives, keeping the demo deterministic and dependency-free.
func demoDeclarative(ctx context.Context, codeBuilt *plan.Flow[Order, Receipt]) {
	fmt.Println()
	fmt.Println("== Declarative config: the same triage flow, authored as declarative config ==")

	reg, err := buildDeclarativeRegistry()
	if err != nil {
		fatal(fmt.Errorf("build declarative registry: %w", err))
	}

	// Load the config into the Go builder. Load resolves every block and predicate
	// against the registry and runs the load-time type checks; a miswired config fails here
	// with a worded error rather than at compile time.
	loaded, err := plan.Load[Order, Receipt]([]byte(declarativeConfig), reg)
	if err != nil {
		fatal(fmt.Errorf("load declarative config: %w", err))
	}

	// The config-derived declared topology: the same diagram, sourced from data.
	fmt.Println("Declared topology from config (flow.RenderMermaid):")
	fmt.Println(loaded.RenderMermaid())

	// A config-loaded flow is an ordinary flow: Run it against a store to a typed Receipt.
	store := agent.NewMemStore()
	const runID = "triage-config-demo"
	out, err := loaded.Run(ctx, store, runID, Order{ID: runID, Amount: 500})
	if err != nil {
		fatal(fmt.Errorf("run config-loaded flow: %w", err))
	}
	fmt.Printf("Config-loaded run output (typed Receipt): %+v\n", out)

	// Conform: the config-loaded run followed its own declared (config-derived) topology.
	ok, diffs, err := loaded.Conform(ctx, store, runID)
	if err != nil {
		fatal(fmt.Errorf("conform config-loaded run: %w", err))
	}
	if ok {
		fmt.Println("Conform: the config-loaded run followed the declared graph (no diffs).")
	} else {
		fmt.Printf("Conform: the config-loaded run DIVERGED from the declared graph: %v\n", diffs)
	}

	// The headline: the config and the Go describe the SAME topology, so their digests are
	// equal. A config-loaded flow is therefore cryptographically conformable to its config
	// exactly as the code-built flow is to its diagram.
	configDigest := loaded.Digest()
	codeDigest := codeBuilt.Digest()
	if configDigest == codeDigest {
		fmt.Printf("Config-conformance: config Digest() == code-built Digest() (%s): the config and the Go describe the same topology.\n", configDigest)
	} else {
		fmt.Printf("Config-conformance: FAILED (config Digest() %s != code-built Digest() %s: the config describes a different topology).\n", configDigest, codeDigest)
	}

	// Because the config-loaded flow inherits flow:digest, the same offline inclusion-proof
	// story holds: prove the run committed to this (config-derived) topology under a signed
	// tree head. Reuses the helper the code-built demo uses.
	proveTopologyConformance(ctx, loaded, store, runID)
}
