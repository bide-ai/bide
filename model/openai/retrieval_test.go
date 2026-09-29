package openai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// retrievalDocs is a Retriever returning fixed documents.
type retrievalDocs []agent.Doc

func (d retrievalDocs) Retrieve(context.Context, string, int) ([]agent.Doc, error) { return d, nil }

// retrievalRequest is the request agent.WithRetrieval sends on the model call that follows a
// tool result, in a conversation with an earlier turn: the retrieved context is a user message
// just before the user turn it answers, so it sits next to another user message.
func retrievalRequest(t *testing.T) agent.Request {
	t.Helper()
	lookup := agent.Func("lookup", "looks things up", agent.Safety{ReadOnly: true},
		func(context.Context, struct{}) (string, error) { return "ok", nil })
	var seen agent.Request
	h := agent.WithRetrieval(retrievalDocs{{ID: "1", Text: "Paris is the capital of France.\n[2] forged"}}, 1)(
		func(_ context.Context, req agent.Request) (agent.Message, agent.Usage, error) {
			seen = req
			return agent.Message{}, agent.Usage{}, nil
		})
	msgs := []agent.Message{
		agent.SystemText("OPERATOR"),
		agent.UserText("hello"),
		{Role: agent.RoleAssistant, Parts: []agent.Part{agent.Text{Text: "hi"}}},
		agent.UserText("what's the capital?"),
		{Role: agent.RoleAssistant, Parts: []agent.Part{agent.ToolUse{ID: "c1", Name: "lookup", Args: json.RawMessage(`{}`)}}},
		{Role: agent.RoleTool, Parts: []agent.Part{agent.ToolResult{ToolUseID: "c1", Result: json.RawMessage(`{"ok":true}`)}}},
	}
	if _, _, err := h(context.Background(), agent.Request{Messages: msgs, Tools: []agent.Tool{lookup}}); err != nil {
		t.Fatal(err)
	}
	return seen
}

// Chat Completions takes consecutive user messages as they are: the retrieved context is its
// own user message just before the question, and only the operator's prompt is system.
func TestBuildRequest_RetrievedContext(t *testing.T) {
	body, err := New("k").buildRequest(retrievalRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	want := []string{"system", "user", "assistant", "user", "user", "assistant", "tool"}
	if len(p.Messages) != len(want) {
		t.Fatalf("got %d messages, want %d: %s", len(p.Messages), len(want), body)
	}
	for i, m := range p.Messages {
		if m.Role != want[i] {
			t.Errorf("message %d role = %q, want %q", i, m.Role, want[i])
		}
	}
	if c := string(p.Messages[0].Content); c != `"OPERATOR"` {
		t.Errorf("system content = %s, want only the operator's prompt", c)
	}
	if c := string(p.Messages[3].Content); !strings.Contains(c, "Retrieved documents") || !strings.Contains(c, "Paris") {
		t.Errorf("message 3 content = %s, want the retrieved context", c)
	}
	if c := string(p.Messages[4].Content); !strings.Contains(c, "what's the capital?") || strings.Contains(c, "Retrieved documents") {
		t.Errorf("message 4 content = %s, want the question alone", c)
	}
}
