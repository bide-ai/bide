package main

// In-process test of the rung-2 (declarative config) path: load the triage flow from the
// JSON config against a registry of the same blocks, run it, conform the run, and assert
// the config-loaded flow's topology Digest EQUALS the code-built flow's. The digest
// equality is the config-conformance headline: the config and the Go describe the same
// topology, so a config-loaded run is cryptographically conformable to its config exactly
// as the code-built run is to its diagram.

import (
	"context"
	"testing"

	agent "github.com/dayna/go-agents"
	"github.com/dayna/go-agents/plan"
)

// TestRung2ConfigMatchesCodeBuilt loads the rung-2 config, runs and conforms it, and
// asserts its Digest equals the code-built flow's Digest.
func TestRung2ConfigMatchesCodeBuilt(t *testing.T) {
	ctx := context.Background()

	codeBuilt, err := buildFlow(config{})
	if err != nil {
		t.Fatalf("buildFlow: %v", err)
	}

	reg, err := buildRung2Registry()
	if err != nil {
		t.Fatalf("buildRung2Registry: %v", err)
	}
	loaded, err := plan.Load[Order, Receipt]([]byte(rung2Config), reg)
	if err != nil {
		t.Fatalf("Load rung-2 config: %v", err)
	}

	// The config-loaded flow runs to the expected typed Receipt.
	store := agent.NewMemStore()
	const runID = "rung2-run"
	out, err := loaded.Run(ctx, store, runID, Order{ID: runID, Amount: 500})
	if err != nil {
		t.Fatalf("Run config-loaded flow: %v", err)
	}
	if out.Outcome != "reserved" || !out.Reserved {
		t.Fatalf("config-loaded run output = %+v, want a reserved receipt", out)
	}

	// The config-loaded run conforms to its declared graph.
	ok, diffs, err := loaded.Conform(ctx, store, runID)
	if err != nil {
		t.Fatalf("Conform config-loaded run: %v", err)
	}
	if !ok {
		t.Fatalf("config-loaded run diverged from the declared graph: %v", diffs)
	}

	// The headline: the config and the Go describe the same topology.
	if loaded.Digest() != codeBuilt.Digest() {
		t.Fatalf("config Digest() %q != code-built Digest() %q: the config describes a different topology",
			loaded.Digest(), codeBuilt.Digest())
	}
}

// TestRung2ValidateCatchesDrift asserts Validate rejects a config that references an
// unregistered block, which is the CI drift guard rung 2 adds over rung 1.
func TestRung2ValidateCatchesDrift(t *testing.T) {
	reg, err := buildRung2Registry()
	if err != nil {
		t.Fatalf("buildRung2Registry: %v", err)
	}
	// A config naming a block the registry does not have must fail Validate.
	const drifted = `{
	  "flow": "order-triage",
	  "entry": "classify",
	  "nodes": [
	    {"name": "classify", "block": "classify"},
	    {"name": "reserve",  "block": "reserve"},
	    {"name": "finalize", "block": "finalize"},
	    {"name": "decline",  "block": "declineX"}
	  ],
	  "wiring": [
	    {"switch": "classify", "when": [{"pred": "rush", "to": "reserve"}], "else": "decline"},
	    {"edge": ["reserve", "finalize"]}
	  ]
	}`
	if err := plan.Validate([]byte(drifted), reg); err == nil {
		t.Fatal("Validate accepted a config referencing an unregistered block; want a drift error")
	}
}
