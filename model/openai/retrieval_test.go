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

// Some OpenAI-compatible servers (vLLM with a Mistral or Llama chat template, say) reject two
// user messages in a row, so the retrieved-context message is merged into the user message it
// precedes: one user turn, the context first, then a blank line, then the question. Only the
// operator's prompt is system, and turns alternate.
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
	want := []string{"system", "user", "assistant", "user", "assistant", "tool"}
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
	var turn string
	if err := json.Unmarshal(p.Messages[3].Content, &turn); err != nil {
		t.Fatalf("user turn content = %s, want a string: %v", p.Messages[3].Content, err)
	}
	if !strings.HasPrefix(turn, "Retrieved documents") || !strings.Contains(turn, "Paris") || !strings.HasSuffix(turn, "\n\nwhat's the capital?") {
		t.Errorf("user turn = %q, want the retrieved context, a blank line, then the question", turn)
	}
}

// When the question carries an image, the user turn is already in content-parts form: the
// context is merged as its own text part, ahead of the question's parts.
func TestBuildRequest_RetrievedContextWithImage(t *testing.T) {
	var seen agent.Request
	h := agent.WithRetrieval(retrievalDocs{{Text: "Paris is the capital of France."}}, 1)(
		func(_ context.Context, req agent.Request) (agent.Message, agent.Usage, error) {
			seen = req
			return agent.Message{}, agent.Usage{}, nil
		})
	q := agent.UserParts(agent.Text{Text: "what city is this?"}, agent.Image{URL: "https://example.com/paris.png"})
	if _, _, err := h(context.Background(), agent.Request{Messages: []agent.Message{q}}); err != nil {
		t.Fatal(err)
	}
	body, err := New("k").buildRequest(seen)
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				ImageURL struct {
					URL string `json:"url"`
				} `json:"image_url"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	if len(p.Messages) != 1 || p.Messages[0].Role != "user" {
		t.Fatalf("messages = %s, want one user turn", body)
	}
	parts := p.Messages[0].Content
	if len(parts) != 3 || !strings.HasPrefix(parts[0].Text, "Retrieved documents") ||
		parts[1].Text != "what city is this?" || parts[2].ImageURL.URL != "https://example.com/paris.png" {
		t.Errorf("user turn parts = %+v, want the context, then the question's text and image", parts)
	}
}
