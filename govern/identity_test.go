package govern_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/govern"
	gsm "github.com/blackwell-systems/gsm"
)

// buildCreditGov returns a governor over a tiny machine plus its policy digest, enough to exercise
// an attested governed action.
func buildCreditGov(t *testing.T) (govern.Applier, string) {
	t.Helper()
	r := gsm.NewRegistry("account")
	bal := r.Int("balance", 0, 10)
	r.On("credit").Does(gsm.Inc(bal)).Add()
	m, _, err := r.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	digest, err := r.PolicyDigest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	return govern.New(m, m.NewState()), digest
}

// TestEventToolAttested_StampsIdentity confirms that when the deployment binds an identity to the
// run (agent.WithIdentity), a governed action's leaf carries who acted, on whose behalf, and under
// what authority, alongside the policy and state digests.
func TestEventToolAttested_StampsIdentity(t *testing.T) {
	gov, digest := buildCreditGov(t)
	tool := govern.EventTool(gov, govern.EventToolConfig{Name: "credit", Description: "credit $1", Event: "credit", PolicyDigest: digest})

	id := agent.Identity{Actor: "exec-agent@1.4.2", OnBehalfOf: "desk-EQ-US", AuthorityRef: "grant#a1b2"}
	ctx := agent.ContextWithIdentity(context.Background(), id)

	raw, err := tool.Call(ctx, []byte(`{}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if got["actor"] != id.Actor || got["on_behalf_of"] != id.OnBehalfOf || got["authority_ref"] != id.AuthorityRef {
		t.Fatalf("identity not stamped into leaf: %+v", got)
	}
	if got["policy_digest"] != digest {
		t.Fatalf("policy digest missing/wrong: %+v", got)
	}
}

// TestEventToolAttested_NoIdentity confirms backward compatibility: with no identity bound to the
// run, the leaf omits the identity fields entirely (rather than emitting empty ones).
func TestEventToolAttested_NoIdentity(t *testing.T) {
	gov, digest := buildCreditGov(t)
	tool := govern.EventTool(gov, govern.EventToolConfig{Name: "credit", Description: "credit $1", Event: "credit", PolicyDigest: digest})

	raw, err := tool.Call(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"actor", "on_behalf_of", "authority_ref"} {
		if _, present := got[k]; present {
			t.Fatalf("expected %q absent when no identity is bound, got %+v", k, got)
		}
	}
}

// TestProof_CommitsToIdentity confirms the end-to-end guarantee: a signed inclusion proof over the
// governed action's leaf commits to the identity claim, so an auditor verifies WHO acted (and under
// what authority) from public artifacts alone, not just that the action happened.
func TestProof_CommitsToIdentity(t *testing.T) {
	ctx := context.Background()
	gov, digest := buildCreditGov(t)
	tool := govern.EventTool(gov, govern.EventToolConfig{Name: "credit", Description: "credit $1", Event: "credit", PolicyDigest: digest})

	id := agent.Identity{Actor: "exec-agent@1.4.2", OnBehalfOf: "desk-EQ-US", AuthorityRef: "grant#a1b2"}
	raw, err := tool.Call(agent.ContextWithIdentity(ctx, id), []byte(`{}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	// Journal the tool result as a governed-action leaf, as the agent loop would.
	store := agent.NewMemStore()
	const runID = "run1"
	if _, err := store.Do(ctx, runID, "call1", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepToolResult, ToolUseID: "call1", Result: raw}, nil
	}); err != nil {
		t.Fatalf("record leaf: %v", err)
	}

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	th, err := audit.NewTreeHead(ctx, store, runID, 1)
	if err != nil {
		t.Fatalf("NewTreeHead: %v", err)
	}
	sth, err := audit.SignTreeHead(th, audit.Ed25519Signer{Priv: priv})
	if err != nil {
		t.Fatal(err)
	}

	pb, err := audit.ProveToolCall(ctx, store, runID, "call1", sth)
	if err != nil {
		t.Fatalf("ProveToolCall: %v", err)
	}
	if err := pb.Verify(audit.Ed25519Verifier{Pub: pub}); err != nil {
		t.Fatalf("bundle did not verify: %v", err)
	}

	// The proven leaf commits to the identity claim.
	var payload map[string]any
	if err := json.Unmarshal(provenRecord(t, pb).Result, &payload); err != nil {
		t.Fatalf("proven payload: %v", err)
	}
	if payload["actor"] != id.Actor || payload["on_behalf_of"] != id.OnBehalfOf {
		t.Fatalf("proof does not commit to identity: %+v", payload)
	}
}

// provenRecord decodes the record a bundle proves.
func provenRecord(t *testing.T, pb audit.ProofBundle) agent.Record {
	t.Helper()
	r, err := pb.Record()
	if err != nil {
		t.Fatal(err)
	}
	return r
}
