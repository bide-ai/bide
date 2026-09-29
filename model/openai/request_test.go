package openai

import (
	"encoding/json"
	"testing"

	"github.com/bide-ai/bide/agent"
)

func payload(t *testing.T, m *Model, req agent.Request) map[string]any {
	t.Helper()
	body, err := m.buildRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// A tool call with no arguments goes back as "{}": "arguments" must be a JSON object, and ""
// is not one.
func TestBuildRequest_ToolCallWithoutArgs(t *testing.T) {
	p := payload(t, New("k"), agent.Request{Messages: []agent.Message{
		{Role: agent.RoleAssistant, Parts: []agent.Part{agent.ToolUse{ID: "c1", Name: "x"}}},
		{Role: agent.RoleTool, Parts: []agent.Part{agent.ToolResult{ToolUseID: "c1", Result: json.RawMessage(`{}`)}}},
	}})
	fn := p["messages"].([]any)[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["arguments"] != "{}" {
		t.Fatalf("arguments = %q, want {}", fn["arguments"])
	}
}

// A system message made of several text parts reads as separate paragraphs, not run together.
func TestBuildRequest_SystemPartsJoined(t *testing.T) {
	p := payload(t, New("k"), agent.Request{Messages: []agent.Message{
		{Role: agent.RoleSystem, Parts: []agent.Part{agent.Text{Text: "B"}, agent.Text{Text: ""}, agent.Text{Text: "C"}}},
	}})
	if c := p["messages"].([]any)[0].(map[string]any)["content"]; c != "B\n\nC" {
		t.Fatalf("system content = %q, want B, a blank line, C", c)
	}
}

// OpenAI's own endpoint takes max_completion_tokens (max_tokens is deprecated there, and the
// reasoning models reject it); other OpenAI-compatible servers get max_tokens, which is what
// they implement, unless the model is an OpenAI reasoning model.
func TestBuildRequest_MaxTokensFieldByEndpoint(t *testing.T) {
	for name, tc := range map[string]struct {
		m    *Model
		want string
	}{
		"openai":                 {New("k", WithMaxTokens(100)), "max_completion_tokens"},
		"openai with path":       {New("k", WithMaxTokens(100), WithBaseURL("https://api.openai.com/v1/")), "max_completion_tokens"},
		"compatible":             {New("k", WithMaxTokens(100), WithBaseURL("http://localhost:11434/v1")), "max_tokens"},
		"compatible o-series":    {New("k", WithMaxTokens(100), WithBaseURL("https://proxy.example/v1"), WithModel("o3-mini")), "max_completion_tokens"},
		"compatible gpt-5":       {New("k", WithMaxTokens(100), WithBaseURL("https://proxy.example/v1"), WithModel("openai/gpt-5.1")), "max_completion_tokens"},
		"compatible other model": {New("k", WithMaxTokens(100), WithBaseURL("https://proxy.example/v1"), WithModel("llama-3.3-70b")), "max_tokens"},
		"not an o-series id":     {New("k", WithMaxTokens(100), WithBaseURL("https://proxy.example/v1"), WithModel("o1x")), "max_tokens"},
		"forced on":              {New("k", WithMaxTokens(100), WithBaseURL("http://localhost:8000/v1"), WithMaxCompletionTokens(true)), "max_completion_tokens"},
		"forced off":             {New("k", WithMaxTokens(100), WithMaxCompletionTokens(false)), "max_tokens"},
	} {
		p := payload(t, tc.m, agent.Request{})
		other := map[string]string{"max_tokens": "max_completion_tokens", "max_completion_tokens": "max_tokens"}[tc.want]
		if p[tc.want] != float64(100) || p[other] != nil {
			t.Errorf("%s: payload = %v, want only %s", name, p, tc.want)
		}
	}
}
