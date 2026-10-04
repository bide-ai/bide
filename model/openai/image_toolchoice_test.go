package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/bide-ai/bide/agent"
)

func simpleTool() agent.Tool {
	return agent.MustFunc("get_weather", "weather", func(_ context.Context, _ struct{}) (string, error) { return "", nil }, agent.WithSafety(agent.Safety{ReadOnly: true}))
}

// A user message with image parts renders the OpenAI array-of-parts content: a text
// part, an image_url data URI for raw bytes, and an image_url passthrough for a URL.
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
	content := got["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("content parts = %d, want 3", len(content))
	}
	txt := content[0].(map[string]any)
	if txt["type"] != "text" || txt["text"] != "describe these" {
		t.Errorf("text part = %+v", txt)
	}
	// bytes -> data URI
	img := content[1].(map[string]any)
	if img["type"] != "image_url" {
		t.Errorf("part[1] type = %v, want image_url", img["type"])
	}
	wantURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)
	if url := img["image_url"].(map[string]any)["url"]; url != wantURI {
		t.Errorf("data URI = %v, want %v", url, wantURI)
	}
	// URL passthrough
	urlImg := content[2].(map[string]any)["image_url"].(map[string]any)
	if urlImg["url"] != "https://example.com/cat.jpg" {
		t.Errorf("url part = %+v", urlImg)
	}
}

// A user message with only text still serializes content as a plain string (maximally
// compatible; the array form is used only when an image is present).
func TestBuildRequest_TextOnlyStaysString(t *testing.T) {
	m := New("k")
	body, _ := m.buildRequest(agent.Request{Messages: []agent.Message{agent.UserText("hi")}})
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	content := got["messages"].([]any)[0].(map[string]any)["content"]
	if _, isString := content.(string); !isString {
		t.Fatalf("text-only content = %T, want string", content)
	}
}

// tool_choice maps onto OpenAI's wire shapes for each mode.
func TestBuildRequest_ToolChoice(t *testing.T) {
	cases := []struct {
		mode string
		name string
		want any
	}{
		{"auto", "", "auto"},
		{"", "", "auto"},
		{"none", "", "none"},
		{"required", "", "required"},
		{"tool", "get_weather", map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}},
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			m := New("k")
			body, err := m.buildRequest(agent.Request{
				Messages:   []agent.Message{agent.UserText("hi")},
				Tools:      []agent.ToolSpec{simpleTool().Spec()},
				ToolChoice: &agent.ToolChoice{Mode: c.mode, Name: c.name},
			})
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			tc := got["tool_choice"]
			switch want := c.want.(type) {
			case string:
				if tc != want {
					t.Errorf("mode %q: tool_choice = %v, want %v", c.mode, tc, want)
				}
			case map[string]any:
				tcm := tc.(map[string]any)
				if tcm["type"] != "function" {
					t.Errorf("tool_choice type = %v, want function", tcm["type"])
				}
				fn := tcm["function"].(map[string]any)
				if fn["name"] != want["function"].(map[string]any)["name"] {
					t.Errorf("forced tool name = %v", fn["name"])
				}
			}
		})
	}
}
