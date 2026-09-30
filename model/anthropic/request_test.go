package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

func build(t *testing.T, req agent.Request) map[string]any {
	t.Helper()
	body, err := New("k").buildRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

// Anthropic has tool_choice {"type":"none"}, and a request whose history holds tool_use
// blocks must declare the tools; dropping them to mean "none" gets the request rejected.
func TestBuildRequest_ToolChoiceNoneKeepsTools(t *testing.T) {
	got := build(t, agent.Request{
		Messages: []agent.Message{
			agent.UserText("hi"),
			{Role: agent.RoleAssistant, Parts: []agent.Part{agent.ToolUse{ID: "t1", Name: "get_weather", Args: json.RawMessage(`{}`)}}},
			{Role: agent.RoleTool, Parts: []agent.Part{agent.ToolResult{ToolUseID: "t1", Result: json.RawMessage(`1`)}}},
		},
		Tools:      []agent.ToolSpec{agent.SpecOf(simpleTool())},
		ToolChoice: &agent.ToolChoice{Mode: "none"},
	})
	if tc, _ := got["tool_choice"].(map[string]any); tc["type"] != "none" {
		t.Errorf("tool_choice = %v, want {type: none}", got["tool_choice"])
	}
	if tools, _ := got["tools"].([]any); len(tools) != 1 {
		t.Errorf("tools = %v, want the declared tool", got["tools"])
	}
}

// Anthropic rejects an empty (or whitespace-only) text block, and a thinking block without
// its signature (history from another provider): both are left out. System text from several
// parts or turns is joined with a blank line, not run together.
func TestBuildRequest_DropsBlocksAnthropicRejects(t *testing.T) {
	got := build(t, agent.Request{Messages: []agent.Message{
		agent.SystemText("You are terse."),
		{Role: agent.RoleSystem, Parts: []agent.Part{agent.Text{Text: "Answer in French."}, agent.Text{Text: "Be kind."}}},
		agent.SystemText(""),
		agent.UserText("go"),
		agent.UserText(" \n"),
		{Role: agent.RoleAssistant, Parts: []agent.Part{
			agent.Reasoning{Text: "from another provider"},
			agent.Reasoning{Text: "signed", Signature: "sig"},
			agent.Text{Text: ""},
			agent.Text{Text: "ok"},
		}},
	}})
	if got["system"] != "You are terse.\n\nAnswer in French.\n\nBe kind." {
		t.Errorf("system = %q", got["system"])
	}
	msgs := got["messages"].([]any)
	user := msgs[0].(map[string]any)["content"].([]any)
	if len(user) != 1 {
		t.Errorf("user content = %v, want only the non-empty text", user)
	}
	asst := msgs[1].(map[string]any)["content"].([]any)
	var kinds []string
	for _, b := range asst {
		bm := b.(map[string]any)
		kinds = append(kinds, bm["type"].(string))
		if bm["type"] == "thinking" && bm["signature"] != "sig" {
			t.Errorf("unsigned thinking block sent: %v", bm)
		}
	}
	if strings.Join(kinds, ",") != "thinking,text" {
		t.Errorf("assistant blocks = %v, want thinking,text", kinds)
	}
}

// A redacted_thinking block is encrypted reasoning Anthropic requires back unchanged. It is
// kept from the response, in order among the thinking blocks, and sent back as it came.
func TestRedactedThinkingRoundTrips(t *testing.T) {
	stream := `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"first"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-1"}}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"ENCRYPTED"}}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"thinking_delta","thinking":"second"}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"signature_delta","signature":"sig-2"}}

event: content_block_start
data: {"type":"content_block_start","index":3,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":3,"delta":{"type":"text_delta","text":"answer"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}

`
	msg, _, err := testStream(stream).Message()
	if err != nil {
		t.Fatal(err)
	}
	got := build(t, agent.Request{Messages: []agent.Message{agent.UserText("q"), msg}})
	asst := got["messages"].([]any)[1].(map[string]any)["content"]
	b, _ := json.Marshal(asst)
	want := `[{"signature":"sig-1","thinking":"first","type":"thinking"},{"data":"ENCRYPTED","type":"redacted_thinking"},` +
		`{"signature":"sig-2","thinking":"second","type":"thinking"},{"text":"answer","type":"text"}]`
	if string(b) != want {
		t.Fatalf("assistant content =\n%s\nwant\n%s", b, want)
	}
}
