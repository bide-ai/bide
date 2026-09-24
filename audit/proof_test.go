package audit_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"testing"

	agent "github.com/dayna/go-agents"
	"github.com/dayna/go-agents/audit"
)

// buildRun journals a small tool-using run and returns the store + a signed STH over it.
func buildRun(t *testing.T) (agent.Durable, string, ed25519.PublicKey, audit.SignedTreeHead) {
	t.Helper()
	ctx := context.Background()
	store := agent.NewMemStore()
	tool := agent.Func("lookup", "", agent.Safety{ReadOnly: true},
		func(_ context.Context, _ struct{}) (string, error) { return "ok", nil })
	if _, err := agent.New(&twoTurnModel{}, store, tool).Run(ctx, "run", "hi"); err != nil {
		t.Fatalf("run: %v", err)
	}
	pub, priv, _ := ed25519.GenerateKey(nil)
	th, err := audit.NewTreeHead(ctx, store, "run", 1000)
	if err != nil {
		t.Fatalf("NewTreeHead: %v", err)
	}
	return store, "run", pub, audit.SignTreeHead(th, priv)
}

// TestProofBundle_RoundTrip: a bundle proving a record verifies under the right key, and is
// rejected when the record, the STH, or the key is tampered.
func TestProofBundle_RoundTrip(t *testing.T) {
	ctx := context.Background()
	store, runID, pub, sth := buildRun(t)

	bundle, err := audit.ProveRecord(ctx, store, runID, 0, sth)
	if err != nil {
		t.Fatalf("ProveRecord: %v", err)
	}
	ok, err := bundle.Verify(pub)
	if err != nil || !ok {
		t.Fatalf("honest bundle failed to verify (ok=%v err=%v)", ok, err)
	}

	// Wrong key: reject.
	otherPub, _, _ := ed25519.GenerateKey(nil)
	if ok, _ := bundle.Verify(otherPub); ok {
		t.Fatal("bundle verified under the wrong public key")
	}

	// Tampered record: reject (inclusion no longer holds).
	forged := bundle
	forged.Record = agent.Record{Kind: agent.StepModel}
	if ok, _ := forged.Verify(pub); ok {
		t.Fatal("bundle verified with a forged record")
	}

	// Tampered STH size (proof no longer bound to the signed tree): reject.
	badSize := bundle
	badSize.Inclusion.Size = badSize.STH.Size + 1
	if ok, _ := badSize.Verify(pub); ok {
		t.Fatal("bundle verified with a proof not bound to the signed size")
	}
}

// TestProofBundle_JSONRoundTrip: a bundle survives marshal/unmarshal and still verifies (the
// serialization the CLI and any auditor handoff relies on, including the embedded STH).
func TestProofBundle_JSONRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, runID, pub, sth := buildRun(t)

	bundle, err := audit.ProveToolCall(ctx, store, runID, "c1", sth)
	if err != nil {
		t.Fatalf("ProveToolCall: %v", err)
	}
	blob, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back audit.ProofBundle
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ok, err := back.Verify(pub); err != nil || !ok {
		t.Fatalf("round-tripped bundle failed to verify (ok=%v err=%v)", ok, err)
	}
}

// TestProveToolCall_Semantic: prove a tool call by its ToolUseID without knowing the index.
func TestProveToolCall_Semantic(t *testing.T) {
	ctx := context.Background()
	store, runID, pub, sth := buildRun(t)

	bundle, err := audit.ProveToolCall(ctx, store, runID, "c1", sth)
	if err != nil {
		t.Fatalf("ProveToolCall: %v", err)
	}
	if bundle.Record.Kind != agent.StepToolResult || bundle.Record.ToolUseID != "c1" {
		t.Fatalf("resolved the wrong record: %+v", bundle.Record)
	}
	if ok, err := bundle.Verify(pub); err != nil || !ok {
		t.Fatalf("tool-call proof failed to verify (ok=%v err=%v)", ok, err)
	}

	// An unknown tool-use ID is an error, not a bad proof.
	if _, err := audit.ProveToolCall(ctx, store, runID, "nope", sth); err == nil {
		t.Fatal("expected error for an unknown tool-use ID")
	}
}

// TestProveRecord_RejectsSTHFromDifferentRun: a valid STH from a different journal must not be
// accepted as a commitment for this run (the root-match guard).
func TestProveRecord_RejectsSTHFromDifferentRun(t *testing.T) {
	ctx := context.Background()
	store, runID, _, _ := buildRun(t)

	// A signed STH over a journal with DIFFERENT content (so a different Merkle root).
	otherStore := agent.NewMemStore()
	for _, v := range []string{"x", "y", "z"} {
		vv := v
		if _, err := agent.Step(ctx, otherStore, "other", vv, func(context.Context) (string, error) { return vv, nil }); err != nil {
			t.Fatalf("other journal: %v", err)
		}
	}
	_, priv, _ := ed25519.GenerateKey(nil)
	otherTH, _ := audit.NewTreeHead(ctx, otherStore, "other", 1000)
	otherSTH := audit.SignTreeHead(otherTH, priv)

	if _, err := audit.ProveRecord(ctx, store, runID, 0, otherSTH); err == nil {
		t.Fatal("ProveRecord accepted an STH whose root does not match this run")
	}
}
