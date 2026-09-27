package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"

	agent "github.com/blackwell-systems/bide"
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

	// One call to commit and sign the policy-used key set.
	sth := SignAbsenceRoot(recs, PolicyUsedKey, priv, 1)

	// Prove a disallowed policy is absent against that signed root.
	b, err := ProveAbsentBundle(recs, PolicyUsedKey, PolicyUsedKeyFor("dddd"), runID, sth)
	if err != nil {
		t.Fatalf("ProveAbsentBundle: %v", err)
	}
	if ok, err := b.Verify(pub); err != nil || !ok {
		t.Fatalf("absence bundle did not verify: ok=%v err=%v", ok, err)
	}

	// ToolUseKeyFor mirrors PolicyUsedKeyFor: prove a tool id that never happened is absent.
	sthTool := SignAbsenceRoot(recs, ToolUseKey, priv, 1)
	bt, err := ProveAbsentBundle(recs, ToolUseKey, ToolUseKeyFor("nope"), runID, sthTool)
	if err != nil {
		t.Fatalf("ProveAbsentBundle (tool): %v", err)
	}
	if ok, err := bt.Verify(pub); err != nil || !ok {
		t.Fatalf("tool absence bundle did not verify: ok=%v err=%v", ok, err)
	}
}
