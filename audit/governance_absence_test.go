package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// governedAction records a completed tool call whose result carries a policy digest, as
// govern.AttestedEventTool does in production.
func governedAction(t *testing.T, ctx context.Context, store agent.Durable, runID, id, digest string) {
	t.Helper()
	res, _ := json.Marshal(map[string]any{"event": "e", "applied": true, "policy_digest": digest})
	if _, err := store.Do(ctx, runID, id, func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepToolResult, ToolUseID: id, Result: res}, nil
	}); err != nil {
		t.Fatalf("record action: %v", err)
	}
}

// TestGovernanceAbsence_NoActionUnderDisallowedPolicy proves the negative: over a complete,
// signed run, no governed action ran under a policy outside the approved set. It records two
// governed actions under an approved digest, commits the policy-used key set, and produces an
// anchorable absence proof that a disallowed digest was never used, verified offline. Recomputing
// the set of policies used confirms it is exactly the approved one.
func TestGovernanceAbsence_NoActionUnderDisallowedPolicy(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	const runID = "run1"
	const approved = "aaaa1111"
	const disallowed = "dddd9999"

	governedAction(t, ctx, store, runID, "call1", approved)
	governedAction(t, ctx, store, runID, "call2", approved)

	recs, err := store.History(ctx, runID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}

	// The set of policies exercised is exactly the approved one.
	used := PoliciesUsed(recs)
	if len(used) != 1 || used[0] != approved {
		t.Fatalf("expected policies used = [%s], got %v", approved, used)
	}

	// Commit the policy-used key set and sign it.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	journal, err := NewTreeHead(ctx, store, runID, 1)
	if err != nil {
		t.Fatal(err)
	}
	sth, err := SignAbsenceRoot(recs, PolicyUsedKeys, journal, priv, 1)
	if err != nil {
		t.Fatal(err)
	}

	// Prove no governed action ran under the disallowed policy.
	bundle, err := ProveAbsentBundle(recs, PolicyUsedKeys, PolicyUsedKeyFor(disallowed), sth)
	if err != nil {
		t.Fatalf("ProveAbsentBundle: %v", err)
	}
	if ok, err := bundle.Verify(pub, PolicyUsedKeys); err != nil || !ok {
		t.Fatalf("absence bundle did not verify: ok=%v err=%v", ok, err)
	}

	// The negative has teeth: you cannot prove absence of a policy that WAS used.
	if _, err := ProveAbsent(recs, PolicyUsedKeys, PolicyUsedKeyFor(approved)); err == nil {
		t.Fatalf("expected error proving absence of an approved policy that was actually used")
	}
}
