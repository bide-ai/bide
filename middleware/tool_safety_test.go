package middleware

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// twoCharges asks for two separate $5 charges (two tool calls with identical arguments), then answers.
func twoCharges() *agent.ScriptedModel {
	return agent.NewScriptedModel(
		agent.ToolTurn("c1", "charge", `{"cents":500}`),
		agent.ToolTurn("c2", "charge", `{"cents":500}`),
		agent.TextTurn("done"),
	)
}

// The README's tool middleware stack on a non-idempotent charge whose provider times out after
// taking the payment. The charge must be attempted once: a retry would charge the card again.
func TestToolRetry_DoesNotRetryANonIdempotentTool(t *testing.T) {
	var charged int
	charge := agent.MustFunc("charge", "charge the card", func(context.Context, struct {
		Cents int `json:"cents"`
	}) (string, error) {
		charged++ // the payment went through
		return "", errors.New("gateway timeout")
	})
	m := agent.NewScriptedModel(
		agent.ToolTurn("c1", "charge", `{"cents":500}`),
		agent.TextTurn("done"),
	)
	store := agenttest.MemJournal()
	a := agenttest.MustNew(
		m,
		store,
		agent.WithTools(charge),
		agent.WithToolMiddleware(ToolCache(), ToolRetry(3, WithBackoff(0, 0))),
	)
	if _, err := a.Run(context.Background(), "r1", agent.UserText("charge $5")); err != nil {
		t.Fatal(err)
	}
	if charged != 1 {
		t.Fatalf("charged %d times, want 1", charged)
	}
	// The model is told what happened: the gateway's error, not a refused retry.
	recs, _ := store.History(context.Background(), "r1")
	for _, r := range recs {
		if r.ToolUseID == "c1" && r.Kind == agent.StepToolResult && !strings.Contains(string(r.Result), "gateway timeout") {
			t.Fatalf("recorded result = %s, want the gateway's error", r.Result)
		}
	}
}

// Two separate charges with the same arguments are two payments. The cache must not answer the
// second from the first: that skips a charge the model asked for and reports it as done.
func TestToolCache_DoesNotCacheANonReadOnlyTool(t *testing.T) {
	var charged int
	charge := agent.MustFunc("charge", "charge the card", func(context.Context, struct {
		Cents int `json:"cents"`
	}) (string, error) {
		charged++
		return "ok", nil
	})
	a := agenttest.MustNew(
		twoCharges(),
		agenttest.MemJournal(),
		agent.WithTools(charge),
		agent.WithToolMiddleware(ToolCache(), ToolRetry(3)),
	)
	if _, err := a.Run(context.Background(), "r1", agent.UserText("charge $5 twice")); err != nil {
		t.Fatal(err)
	}
	if charged != 2 {
		t.Fatalf("charged %d times for two charge calls, want 2", charged)
	}
}
