package gemini

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
		func(_ context.Context, call agent.ModelCall) (agent.ModelResponse, error) {
			seen = call.Request
			return agent.ModelResponse{}, nil
		})
	msgs := []agent.Message{
		agent.SystemText("OPERATOR"),
		agent.UserText("hello"),
		{Role: agent.RoleAssistant, Parts: []agent.Part{agent.Text{Text: "hi"}}},
		agent.UserText("what's the capital?"),
		{Role: agent.RoleAssistant, Parts: []agent.Part{agent.ToolUse{ID: "c1", Name: "lookup", Args: json.RawMessage(`{}`)}}},
		{Role: agent.RoleTool, Parts: []agent.Part{agent.ToolResult{ToolUseID: "c1", Result: json.RawMessage(`{"ok":true}`)}}},
	}
	if _, err := h(context.Background(), agent.ModelCall{Request: agent.Request{Messages: msgs, Tools: []agent.Tool{lookup}}}); err != nil {
		t.Fatal(err)
	}
	return seen
}

// Gemini alternates user and model turns: the retrieved-context message folds into the user
// turn it precedes, ahead of the question, and only the operator's prompt is system.
func TestBuildRequest_RetrievedContext(t *testing.T) {
	body, err := New("k").buildRequest(retrievalRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		SystemInstruction struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"systemInstruction"`
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text             string          `json:"text"`
				FunctionResponse json.RawMessage `json:"functionResponse"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	if sys := p.SystemInstruction.Parts; len(sys) != 1 || sys[0].Text != "OPERATOR" {
		t.Errorf("systemInstruction = %+v, want only the operator's prompt", sys)
	}
	want := []string{"user", "model", "user", "model", "user"}
	if len(p.Contents) != len(want) {
		t.Fatalf("got %d contents, want %d: %s", len(p.Contents), len(want), body)
	}
	for i, c := range p.Contents {
		if c.Role != want[i] {
			t.Errorf("content %d role = %q, want %q (turns must alternate)", i, c.Role, want[i])
		}
	}
	turn := p.Contents[2].Parts
	if len(turn) != 2 || !strings.HasPrefix(turn[0].Text, "Retrieved documents") || !strings.Contains(turn[0].Text, "Paris") || turn[1].Text != "what's the capital?" {
		t.Errorf("user turn = %+v, want the retrieved context then the question", turn)
	}
	if last := p.Contents[4].Parts; len(last) == 0 || last[0].FunctionResponse == nil {
		t.Errorf("last turn = %+v, want the function response", last)
	}
}
