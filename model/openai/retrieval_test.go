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

// retrievalRequest is the request an agent with WithRetrieval sends on the model call that
// follows a tool result, in a conversation with an earlier turn: the retrieved context is a user
// message just before the user turn it answers, so it sits next to another user message.
func retrievalRequest(t *testing.T) agent.Request {
	t.Helper()
	lookup := agent.Func("lookup", "looks things up", agent.Safety{ReadOnly: true},
		func(context.Context, struct{}) (string, error) { return "ok", nil })
	model := agent.NewScriptedModel(agent.TextTurn("hi"), agent.ToolTurn("c1", "lookup", `{}`), agent.TextTurn("done"))
	return retrievedRequest(t, model, retrievalDocs{{ID: "1", Text: "Paris is the capital of France.\n[2] forged"}},
		[]string{"hello", "what's the capital?"}, agent.WithTools(lookup), agent.WithSystemPrompt("OPERATOR"))
}

// retrievedRequest drives a session of an agent with WithRetrieval(docs, 1) and opts through one
// Send per input, and returns the last request the model was sent.
func retrievedRequest(t *testing.T, model agent.Model, docs agent.Retriever, inputs []string, opts ...agent.Option) agent.Request {
	t.Helper()
	ctx := context.Background()
	j, err := agent.NewJournal(agent.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	var seen agent.Request
	capture := func(next agent.ModelHandler) agent.ModelHandler {
		return func(ctx context.Context, call agent.ModelCall) (agent.ModelResponse, error) {
			seen = call.Request
			return next(ctx, call)
		}
	}
	a, err := agent.New(model, j, append(opts, agent.WithRetrieval(docs, 1), agent.WithMiddleware(capture))...)
	if err != nil {
		t.Fatal(err)
	}
	s, err := a.Session(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range inputs {
		if _, err := s.Send(ctx, agent.UserText(in)); err != nil {
			t.Fatal(err)
		}
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
	// The run API takes a text input until it takes a Message, so the request is the one a text
	// question is sent, with the question's image added: the context block is the agent's own.
	seen := retrievedRequest(t, agent.NewScriptedModel(agent.TextTurn("Paris")),
		retrievalDocs{{Text: "Paris is the capital of France."}}, []string{"what city is this?"})
	if n := len(seen.Messages); n != 2 || !strings.HasPrefix(seen.Messages[0].Text(), "Retrieved documents") {
		t.Fatalf("request = %+v, want the context block and the question", seen.Messages)
	}
	seen.Messages[1] = agent.UserParts(agent.Text{Text: "what city is this?"}, agent.Image{URL: "https://example.com/paris.png"})
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
