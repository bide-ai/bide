package openai

import (
	"encoding/json"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// stubCodec marks its input so the test can tell codec output from raw JSON.
type stubCodec struct{}

func (stubCodec) EncodeToolResult(raw json.RawMessage) (string, error) {
	return "CODEC:" + string(raw), nil
}

func TestBuildRequest_ToolResultCodec(t *testing.T) {
	req := agent.Request{Messages: []agent.Message{
		{Role: agent.RoleTool, Parts: []agent.Part{
			agent.ToolResult{ToolUseID: "call_1", Result: json.RawMessage(`{"temp":68}`)},
		}},
	}}

	// Default: the tool content is the raw JSON, unchanged.
	body, err := New("k").buildRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := toolContent(t, body); got != `{"temp":68}` {
		t.Fatalf("default content = %q, want raw JSON", got)
	}

	// With a codec: the tool content is the codec's output.
	body, err = New("k", WithToolResultCodec(stubCodec{})).buildRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := toolContent(t, body); got != `CODEC:{"temp":68}` {
		t.Fatalf("codec content = %q, want codec output", got)
	}
}

func toolContent(t *testing.T, body []byte) string {
	t.Helper()
	var r struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	for _, m := range r.Messages {
		if m.Role == "tool" {
			return m.Content
		}
	}
	t.Fatal("no tool message in request body")
	return ""
}
