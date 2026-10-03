package anthropic

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
		if _, err := s.Send(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	return seen
}

// Anthropic wants strict user/assistant alternation: the retrieved-context message folds into
// the user turn it precedes, ahead of the question, and only the operator's prompt is system.
func TestBuildRequest_RetrievedContext(t *testing.T) {
	body, err := New("k").buildRequest(retrievalRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		System   string `json:"system"`
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	if p.System != "OPERATOR" {
		t.Errorf("system = %q, want only the operator's prompt", p.System)
	}
	want := []string{"user", "assistant", "user", "assistant", "user"}
	if len(p.Messages) != len(want) {
		t.Fatalf("got %d messages, want %d: %s", len(p.Messages), len(want), body)
	}
	for i, m := range p.Messages {
		if m.Role != want[i] {
			t.Errorf("message %d role = %q, want %q (turns must alternate)", i, m.Role, want[i])
		}
	}
	turn := p.Messages[2].Content
	if len(turn) != 2 || !strings.HasPrefix(turn[0].Text, "Retrieved documents") || !strings.Contains(turn[0].Text, "Paris") || turn[1].Text != "what's the capital?" {
		t.Errorf("user turn = %+v, want the retrieved context then the question", turn)
	}
	if last := p.Messages[4].Content; len(last) == 0 || last[0].Type != "tool_result" {
		t.Errorf("last turn = %+v, want the tool result", last)
	}
}
