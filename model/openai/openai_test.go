package openai

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// An OpenAI streaming response: text + a tool call whose arguments arrive fragmented
// across chunks, then a final usage-only chunk, then [DONE].
const sample = `data: {"choices":[{"delta":{"content":"Let me check. "}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"get_weather","arguments":"{\"city\":"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"SF\"}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":9}}

data: [DONE]

`

func TestStreamSSE_NormalizesToolCallAndUsage(t *testing.T) {
	ch := make(chan agent.Emit)
	go streamSSE(io.NopCloser(strings.NewReader(sample)), ch)

	msg, usage, err := agent.NewStream(ch).Message()
	if err != nil {
		t.Fatalf("Message: %v", err)
	}

	var text string
	var gotTool bool
	for _, p := range msg.Parts {
		switch v := p.(type) {
		case agent.Text:
			text = v.Text
		case agent.ToolUse:
			gotTool = true
			if v.ID != "call_1" || v.Name != "get_weather" {
				t.Errorf("tool use = %+v", v)
			}
			if string(v.Args) != `{"city":"SF"}` {
				t.Errorf("assembled args = %s, want {\"city\":\"SF\"}", v.Args)
			}
		}
	}
	if text != "Let me check. " {
		t.Errorf("text = %q", text)
	}
	if !gotTool {
		t.Error("missing tool_use")
	}
	if usage.InputTokens != 11 || usage.OutputTokens != 9 {
		t.Errorf("usage = %+v, want {11 9}", usage)
	}
}

func TestBuildRequest_MessagesAndStrictSchema(t *testing.T) {
	type Args struct {
		City string `json:"city"`
	}
	tool := agent.Func("get_weather", "weather", agent.Safety{ReadOnly: true},
		func(_ context.Context, a Args) (string, error) { return "", nil })

	m := New("k", WithStrictSchema(), WithModel("gpt-4o"))
	body, err := m.buildRequest(agent.Request{
		Messages: []agent.Message{
			agent.SystemText("be terse"),
			agent.UserText("weather?"),
			{Role: agent.RoleAssistant, Parts: []agent.Part{
				agent.ToolUse{ID: "call_1", Name: "get_weather", Args: json.RawMessage(`{"city":"SF"}`)},
			}},
			{Role: agent.RoleTool, Parts: []agent.Part{
				agent.ToolResult{ToolUseID: "call_1", Result: json.RawMessage(`{"temp":68}`)},
			}},
		},
		Tools: []agent.Tool{tool},
	})
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want 4", len(msgs))
	}
	// assistant turn carries tool_calls
	asst := msgs[2].(map[string]any)
	if asst["role"] != "assistant" || asst["tool_calls"] == nil {
		t.Errorf("assistant tool_calls missing: %+v", asst)
	}
	// tool turn uses role=tool + tool_call_id
	tr := msgs[3].(map[string]any)
	if tr["role"] != "tool" || tr["tool_call_id"] != "call_1" {
		t.Errorf("tool message wrong: %+v", tr)
	}
	// strict mode: function has strict:true and additionalProperties:false in params
	fn := got["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["strict"] != true {
		t.Errorf("expected strict:true, got %+v", fn)
	}
	params := fn["parameters"].(map[string]any)
	if params["additionalProperties"] != false {
		t.Errorf("strict schema must set additionalProperties:false, got %+v", params)
	}
}
