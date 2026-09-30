package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// TestSignAbsenceRoot_OneCall confirms the one-step signer produces a tree head that
// ProveAbsentBundle accepts, and the resulting proof verifies offline.
func TestSignAbsenceRoot_OneCall(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	const runID = "run1"

	res, _ := json.Marshal(map[string]any{"event": "e", "applied": true, "policy_digest": "aaaa"})
	store.Do(ctx, runID, "call1", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepToolResult, ToolUseID: "call1", Result: res}, nil
	})
	recs, _ := store.History(ctx, runID)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}

	journal, err := NewTreeHead(ctx, store, runID, 1)
	if err != nil {
		t.Fatal(err)
	}
	// One call to commit and sign the policy-used key set.
	sth, err := SignAbsenceRoot(recs, PolicyUsedKeys, journal, edS(priv), 1)
	if err != nil {
		t.Fatal(err)
	}

	// Prove a disallowed policy is absent against that signed root.
	b, err := ProveAbsentBundle(recs, PolicyUsedKeys, PolicyUsedKeyFor("dddd"), sth)
	if err != nil {
		t.Fatalf("ProveAbsentBundle: %v", err)
	}
	if err := b.Verify(edV(pub), PolicyUsedKeys); err != nil {
		t.Fatalf("absence bundle did not verify: %v", err)
	}

	// ToolUseKeyFor mirrors PolicyUsedKeyFor: prove a tool id that never happened is absent.
	sthTool, err := SignAbsenceRoot(recs, ToolUseKeys, journal, edS(priv), 1)
	if err != nil {
		t.Fatal(err)
	}
	bt, err := ProveAbsentBundle(recs, ToolUseKeys, ToolUseKeyFor("nope"), sthTool)
	if err != nil {
		t.Fatalf("ProveAbsentBundle (tool): %v", err)
	}
	if err := bt.Verify(edV(pub), ToolUseKeys); err != nil {
		t.Fatalf("tool absence bundle did not verify: %v", err)
	}
}
