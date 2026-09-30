package anthropic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/bide-ai/bide/agent"
)

func simpleTool() agent.Tool {
	return agent.Func("get_weather", "weather", agent.Safety{ReadOnly: true},
		func(_ context.Context, _ struct{}) (string, error) { return "", nil })
}

// A user message with image parts renders Anthropic image content blocks: a base64
// source for raw bytes and a url source for a hosted image.
func TestBuildRequest_Image(t *testing.T) {
	raw := []byte{0x01, 0x02, 0x03, 0x04}
	m := New("k")
	body, err := m.buildRequest(agent.Request{
		Messages: []agent.Message{
			agent.UserParts(
				agent.Text{Text: "describe these"},
				agent.ImageData("image/png", raw),
				agent.ImageURL("https://example.com/cat.jpg"),
			),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	blocks := got["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(blocks) != 3 {
		t.Fatalf("content blocks = %d, want 3", len(blocks))
	}
	// base64 source block
	b64 := blocks[1].(map[string]any)
	if b64["type"] != "image" {
		t.Errorf("block[1] type = %v, want image", b64["type"])
	}
	src := b64["source"].(map[string]any)
	if src["type"] != "base64" || src["media_type"] != "image/png" {
		t.Errorf("base64 source = %+v", src)
	}
	if src["data"] != base64.StdEncoding.EncodeToString(raw) {
		t.Errorf("base64 data = %v", src["data"])
	}
	// url source block
	usrc := blocks[2].(map[string]any)["source"].(map[string]any)
	if usrc["type"] != "url" || usrc["url"] != "https://example.com/cat.jpg" {
		t.Errorf("url source = %+v", usrc)
	}
}

// tool_choice maps onto Anthropic's wire shapes, "none" included.
func TestBuildRequest_ToolChoice(t *testing.T) {
	cases := []struct {
		mode string
		name string
		want map[string]any
	}{
		{"auto", "", map[string]any{"type": "auto"}},
		{"", "", map[string]any{"type": "auto"}},
		{"required", "", map[string]any{"type": "any"}},
		{"tool", "get_weather", map[string]any{"type": "tool", "name": "get_weather"}},
		{"none", "", map[string]any{"type": "none"}},
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			m := New("k")
			body, err := m.buildRequest(agent.Request{
				Messages:   []agent.Message{agent.UserText("hi")},
				Tools:      []agent.ToolSpec{agent.SpecOf(simpleTool())},
				ToolChoice: &agent.ToolChoice{Mode: c.mode, Name: c.name},
			})
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			tcm, _ := got["tool_choice"].(map[string]any)
			for k, v := range c.want {
				if tcm[k] != v {
					t.Errorf("mode %q: tool_choice[%q] = %v, want %v", c.mode, k, tcm[k], v)
				}
			}
		})
	}
}
