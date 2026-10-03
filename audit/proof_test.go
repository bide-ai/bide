package audit_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
)

// buildRun journals a small tool-using run and returns the store + a signed STH over it.
func buildRun(t *testing.T) (*agent.Journal, string, ed25519.PublicKey, audit.SignedTreeHead) {
	t.Helper()
	ctx := context.Background()
	store := agent.NewMemStore()
	j := agenttest.MustJournal(store)
	tool := agent.Func("lookup", "", agent.Safety{ReadOnly: true},
		func(_ context.Context, _ struct{}) (string, error) { return "ok", nil })
	if _, err := agenttest.MustNew(&twoTurnModel{}, j, agent.WithTools(tool)).Run(ctx, "run", agent.UserText("hi")); err != nil {
		t.Fatalf("run: %v", err)
	}
	pub, priv, _ := ed25519.GenerateKey(nil)
	th, err := audit.NewTreeHead(ctx, j, "run", 1000)
	if err != nil {
		t.Fatalf("NewTreeHead: %v", err)
	}
	return j, "run", pub, signTH(t, th, priv)
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
	if err := bundle.Verify(edV(pub)); err != nil {
		t.Fatalf("valid bundle failed to verify (err=%v)", err)
	}

	// Wrong key: reject.
	otherPub, _, _ := ed25519.GenerateKey(nil)
	if err := bundle.Verify(edV(otherPub)); err == nil {
		t.Fatal("bundle verified under the wrong public key")
	}

	// Tampered record: reject (inclusion no longer holds).
	forged := bundle
	editRec(t, &forged, func(r *agent.Record) { *r = agent.Record{Name: r.Name, Kind: agent.StepModel} })
	if err := forged.Verify(edV(pub)); err == nil {
		t.Fatal("bundle verified with a forged record")
	}

	// Tampered STH size (proof no longer bound to the signed tree): reject.
	badSize := bundle
	badSize.Inclusion.Size = badSize.STH.Size + 1
	if err := badSize.Verify(edV(pub)); err == nil {
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
	if err := back.Verify(edV(pub)); err != nil {
		t.Fatalf("round-tripped bundle failed to verify (err=%v)", err)
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
	if recOf(t, bundle).Kind != agent.StepToolResult || recOf(t, bundle).ToolUseID != "c1" {
		t.Fatalf("resolved the wrong record: %+v", recOf(t, bundle))
	}
	if err := bundle.Verify(edV(pub)); err != nil {
		t.Fatalf("tool-call proof failed to verify (err=%v)", err)
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
	j := agenttest.MustJournal(otherStore)
	for _, v := range []string{"x", "y", "z"} {
		vv := v
		if _, err := agent.Step(ctx, j, "other", vv, func(context.Context) (string, error) { return vv, nil }, agent.WithSafety(agent.Safety{ReadOnly: true})); err != nil {
			t.Fatalf("other journal: %v", err)
		}
	}
	_, priv, _ := ed25519.GenerateKey(nil)
	otherTH, _ := audit.NewTreeHead(ctx, j, "other", 1000)
	otherSTH := signTH(t, otherTH, priv)

	if _, err := audit.ProveRecord(ctx, store, runID, 0, otherSTH); err == nil {
		t.Fatal("ProveRecord accepted an STH whose root does not match this run")
	}
}
