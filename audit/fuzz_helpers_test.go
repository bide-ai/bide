package audit

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// fuzzPriv is a fixed signing key so fuzz seeds signed with it stay valid across runs.
var fuzzPriv = ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
var fuzzPub = fuzzPriv.Public().(ed25519.PublicKey)

// fuzzRun journals a small fixed run (a model turn with text and a tool call, its tool result, and
// a final answer) and returns the store, its records, and a signed journal head over it.
func fuzzRun(tb testing.TB) (agent.Durable, []agent.Record, SignedTreeHead) {
	tb.Helper()
	ctx := context.Background()
	store := agent.NewMemStore()
	recs := []agent.Record{
		{Name: "@llm/0", Kind: agent.StepModel, Message: &agent.Message{Role: agent.RoleAssistant, Parts: []agent.Part{
			agent.Text{Text: "refund $10"},
			agent.ToolUse{ID: "c1", Name: "refund", Args: json.RawMessage(`{"amount":10}`)},
		}}, Usage: &agent.Usage{InputTokens: 3, OutputTokens: 4}},
		{Name: "c1", Kind: agent.StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`{"ok":true}`)},
		{Name: "@llm/1", Kind: agent.StepModel, Message: &agent.Message{Role: agent.RoleAssistant, Parts: []agent.Part{agent.Text{Text: "done"}}}},
	}
	for _, r := range recs {
		if _, err := store.Do(ctx, "run", r.Name, func(context.Context) (agent.Record, error) { return r, nil }); err != nil {
			tb.Fatal(err)
		}
	}
	th, err := NewTreeHead(ctx, store, "run", 1000)
	if err != nil {
		tb.Fatal(err)
	}
	got, _ := store.History(ctx, "run")
	return store, got, SignTreeHead(th, fuzzPriv)
}
