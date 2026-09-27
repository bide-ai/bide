package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"

	agent "github.com/blackwell-systems/bide"
)

// TestPolicyLeaf_AnchorsAndCrossLinks commits a policy as a journal leaf, records a governed
// action whose result embeds the same policy digest, and proves both against one signed tree
// head. It confirms an auditor can show, from public artifacts alone, that the action ran under
// a policy anchored in the same committed tree: the two ProofBundles verify under the same key,
// share the same tree, and their digests link.
func TestPolicyLeaf_AnchorsAndCrossLinks(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	const runID = "run1"

	policy := []byte("(doms 4 4)\n(inv (le (var 0) (lit 3)) (do (set 0 (lit 3))))\n(ev (do (set 0 (add (var 0) (lit 1)))))\n")
	const digest = "308ddefa98275ee4289f74461c3c7c3b29c2dc307e2f4d3f403133eb77fb62c3"

	// Anchor the policy as a dedicated leaf.
	if _, err := RecordPolicy(ctx, store, runID, policy, digest); err != nil {
		t.Fatalf("RecordPolicy: %v", err)
	}

	// Record a governed action whose result embeds the policy digest (as AttestedEventTool does).
	actionResult, _ := json.Marshal(map[string]any{"event": "inc_a", "applied": true, "policy_digest": digest})
	if _, err := store.Do(ctx, runID, "call1", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepToolResult, ToolUseID: "call1", Result: actionResult}, nil
	}); err != nil {
		t.Fatalf("record action: %v", err)
	}

	// Commit and sign a tree head over the whole journal.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	th, err := NewTreeHead(ctx, store, runID, 1)
	if err != nil {
		t.Fatalf("NewTreeHead: %v", err)
	}
	sth := SignTreeHead(th, priv)

	// Prove the policy leaf.
	pb, err := ProvePolicy(ctx, store, runID, digest, sth)
	if err != nil {
		t.Fatalf("ProvePolicy: %v", err)
	}
	if ok, err := pb.Verify(pub); err != nil || !ok {
		t.Fatalf("policy bundle did not verify: ok=%v err=%v", ok, err)
	}
	var pc PolicyContent
	if err := json.Unmarshal(pb.Record.Result, &pc); err != nil {
		t.Fatalf("policy content: %v", err)
	}
	if pc.Digest != digest || pc.Policy != string(policy) {
		t.Fatalf("policy leaf content mismatch: %+v", pc)
	}

	// Prove the action.
	ab, err := ProveToolCall(ctx, store, runID, "call1", sth)
	if err != nil {
		t.Fatalf("ProveToolCall: %v", err)
	}
	if ok, err := ab.Verify(pub); err != nil || !ok {
		t.Fatalf("action bundle did not verify: ok=%v err=%v", ok, err)
	}

	// Cross-link: same committed tree, and the action's embedded digest matches the anchored
	// policy leaf's digest.
	if pb.STH.Size != ab.STH.Size || string(pb.STH.Root) != string(ab.STH.Root) {
		t.Fatalf("bundles are not against the same tree")
	}
	var actionPayload map[string]any
	if err := json.Unmarshal(ab.Record.Result, &actionPayload); err != nil {
		t.Fatalf("action payload: %v", err)
	}
	if actionPayload["policy_digest"] != pc.Digest {
		t.Fatalf("action digest %v does not link to anchored policy digest %s", actionPayload["policy_digest"], pc.Digest)
	}
}

// TestRecordPolicy_Idempotent confirms recording the same policy twice yields one leaf.
func TestRecordPolicy_Idempotent(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	const runID = "run1"
	policy := []byte("(doms 2)\n")
	const digest = "abc123"

	if _, err := RecordPolicy(ctx, store, runID, policy, digest); err != nil {
		t.Fatalf("RecordPolicy 1: %v", err)
	}
	if _, err := RecordPolicy(ctx, store, runID, policy, digest); err != nil {
		t.Fatalf("RecordPolicy 2: %v", err)
	}
	recs, err := store.History(ctx, runID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	n := 0
	for _, r := range recs {
		if r.Kind == agent.StepValue && r.Name == policyLeafName(digest) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly one policy leaf, got %d", n)
	}
}
