package anthropic

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bide-ai/bide/agent"
)

type toolStub struct{ name string }

func (t toolStub) Name() string                { return t.name }
func (t toolStub) Description() string         { return "" }
func (t toolStub) Safety() agent.Safety        { return agent.Safety{} }
func (t toolStub) ArgsSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t toolStub) Call(context.Context, json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}

func cacheReq() agent.Request {
	return agent.Request{
		Messages: []agent.Message{agent.SystemText("you are helpful"), agent.UserText("hi")},
		Tools:    []agent.ToolSpec{agent.SpecOf(toolStub{"a"}), agent.SpecOf(toolStub{"b"})},
	}
}

// With caching on, cache_control breakpoints appear on the system block and the last tool.
func TestBuildRequest_PromptCacheBreakpoints(t *testing.T) {
	m := New("k", WithPromptCache())
	body, err := m.buildRequest(cacheReq())
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}

	sys, ok := p["system"].([]any)
	if !ok || len(sys) != 1 {
		t.Fatalf("system should be a structured block array, got %T %v", p["system"], p["system"])
	}
	if sys[0].(map[string]any)["cache_control"] == nil {
		t.Error("system block missing cache_control")
	}
	tools := p["tools"].([]any)
	last := tools[len(tools)-1].(map[string]any)
	if last["cache_control"] == nil {
		t.Error("last tool missing cache_control breakpoint")
	}
	if first := tools[0].(map[string]any); first["cache_control"] != nil {
		t.Error("only the last tool should carry the breakpoint")
	}
}

// Without caching, system is a plain string and tools carry no cache_control.
func TestBuildRequest_NoCacheByDefault(t *testing.T) {
	m := New("k")
	body, _ := m.buildRequest(cacheReq())
	var p map[string]any
	_ = json.Unmarshal(body, &p)
	if _, isStr := p["system"].(string); !isStr {
		t.Errorf("system should be a plain string when caching is off, got %T", p["system"])
	}
	for _, tl := range p["tools"].([]any) {
		if tl.(map[string]any)["cache_control"] != nil {
			t.Error("no cache_control expected when caching is off")
		}
	}
}

// Cache token counts from message_start surface in agent.Usage.
func TestStreamSSE_ReportsCacheUsage(t *testing.T) {
	const stream = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":12,"cache_read_input_tokens":900,"cache_creation_input_tokens":100}}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`
	_, u, err := testStream(stream).Message()
	if err != nil {
		t.Fatal(err)
	}
	if u.CacheReadTokens != 900 {
		t.Errorf("CacheReadTokens = %d, want 900", u.CacheReadTokens)
	}
	if u.CacheWriteTokens != 100 {
		t.Errorf("CacheWriteTokens = %d, want 100", u.CacheWriteTokens)
	}
	if u.InputTokens != 12 {
		t.Errorf("InputTokens = %d, want 12", u.InputTokens)
	}
}
