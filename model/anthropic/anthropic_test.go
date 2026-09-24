package anthropic

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	agent "github.com/dayna/go-agents"
)

// A realistic Anthropic SSE stream: thinking + signature, then a tool_use whose args
// arrive as fragmented input_json_delta. Verifies we normalize to agent events and
// assemble the Message correctly (incl. preserving the reasoning signature — our win
// vs Eino, whose reasoning parts drop it).
const sample = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":10}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"let me check the weather"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-abc"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"SF\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}

`

func TestStreamSSE_NormalizesAndAssembles(t *testing.T) {
	ch := make(chan agent.Emit)
	go streamSSE(io.NopCloser(strings.NewReader(sample)), ch)

	msg, usage, err := agent.NewStream(ch).Message()
	if err != nil {
		t.Fatalf("Message: %v", err)
	}

	var gotReasoning bool
	var gotTool bool
	for _, p := range msg.Parts {
		switch v := p.(type) {
		case agent.Reasoning:
			gotReasoning = true
			if v.Text != "let me check the weather" {
				t.Errorf("reasoning text = %q", v.Text)
			}
			if v.Signature != "sig-abc" {
				t.Errorf("reasoning signature = %q, want sig-abc (must be preserved)", v.Signature)
			}
		case agent.ToolUse:
			gotTool = true
			if v.ID != "toolu_1" || v.Name != "get_weather" {
				t.Errorf("tool use = %+v", v)
			}
			if string(v.Args) != `{"city":"SF"}` {
				t.Errorf("assembled args = %s, want {\"city\":\"SF\"}", v.Args)
			}
		}
	}
	if !gotReasoning {
		t.Error("missing reasoning part")
	}
	if !gotTool {
		t.Error("missing tool_use part")
	}
	if usage.InputTokens != 10 || usage.OutputTokens != 7 {
		t.Errorf("usage = %+v, want {10 7}", usage)
	}
}

// buildRequest folds system turns, echoes thinking signatures, and renders tool_result
// turns as user messages.
func TestBuildRequest_Translation(t *testing.T) {
	m := New("test-key")
	body, err := m.buildRequest(agent.Request{
		Messages: []agent.Message{
			agent.SystemText("be terse"),
			agent.UserText("weather in SF?"),
			{Role: agent.RoleAssistant, Parts: []agent.Part{
				agent.Reasoning{Text: "thinking", Signature: "sig-1"},
				agent.ToolUse{ID: "toolu_1", Name: "get_weather", Args: json.RawMessage(`{"city":"SF"}`)},
			}},
			{Role: agent.RoleTool, Parts: []agent.Part{
				agent.ToolResult{ToolUseID: "toolu_1", Result: json.RawMessage(`{"temp":68}`)},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["system"] != "be terse" {
		t.Errorf("system = %v", got["system"])
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 3 { // system folded out; user, assistant, tool(as user)
		t.Fatalf("messages = %d, want 3", len(msgs))
	}
	// the tool-result turn must render as a user role
	last := msgs[2].(map[string]any)
	if last["role"] != "user" {
		t.Errorf("tool-result turn role = %v, want user", last["role"])
	}
	block := last["content"].([]any)[0].(map[string]any)
	if block["type"] != "tool_result" || block["tool_use_id"] != "toolu_1" {
		t.Errorf("tool_result block = %+v", block)
	}
	// assistant thinking block must carry the signature back
	asst := msgs[1].(map[string]any)["content"].([]any)
	think := asst[0].(map[string]any)
	if think["type"] != "thinking" || think["signature"] != "sig-1" {
		t.Errorf("thinking block = %+v (signature must be echoed)", think)
	}
}
