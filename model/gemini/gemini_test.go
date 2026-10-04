package gemini

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

func simpleTool() agent.Tool {
	return agent.MustFunc("get_weather", "weather", func(_ context.Context, _ struct{}) (string, error) { return "", nil }, agent.WithSafety(agent.Safety{ReadOnly: true}))
}

// A system turn folds into systemInstruction; user/assistant/tool turns become
// contents with Gemini roles (user/model) and functionCall/functionResponse parts.
func TestBuildRequest_MessagesAndSystemInstruction(t *testing.T) {
	m := New("k")
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
		Tools: []agent.ToolSpec{simpleTool().Spec()},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}

	// systemInstruction folds the system turn out of contents.
	si := got["systemInstruction"].(map[string]any)
	sp := si["parts"].([]any)[0].(map[string]any)
	if sp["text"] != "be terse" {
		t.Errorf("systemInstruction text = %v", sp["text"])
	}

	contents := got["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("contents = %d, want 3 (system folded out)", len(contents))
	}

	// user turn
	if r := contents[0].(map[string]any)["role"]; r != "user" {
		t.Errorf("contents[0] role = %v, want user", r)
	}
	// assistant turn is role "model" carrying a functionCall
	asst := contents[1].(map[string]any)
	if asst["role"] != "model" {
		t.Errorf("contents[1] role = %v, want model", asst["role"])
	}
	fc := asst["parts"].([]any)[0].(map[string]any)["functionCall"].(map[string]any)
	if fc["name"] != "get_weather" {
		t.Errorf("functionCall name = %v", fc["name"])
	}
	// tool result turn is a user turn carrying a functionResponse keyed by tool name
	tr := contents[2].(map[string]any)
	if tr["role"] != "user" {
		t.Errorf("contents[2] role = %v, want user (functionResponse rides a user turn)", tr["role"])
	}
	fr := tr["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)
	if fr["name"] != "get_weather" {
		t.Errorf("functionResponse name = %v, want get_weather (recovered from call id)", fr["name"])
	}
	resp := fr["response"].(map[string]any)
	if resp["temp"].(float64) != 68 {
		t.Errorf("functionResponse response = %v", resp)
	}

	// tool declaration
	decl := got["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)
	if decl["name"] != "get_weather" {
		t.Errorf("functionDeclaration name = %v", decl["name"])
	}
}

// Image parts render inlineData (base64 for raw bytes) and fileData (fileUri for a URL).
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
	parts := got["contents"].([]any)[0].(map[string]any)["parts"].([]any)
	if len(parts) != 3 {
		t.Fatalf("parts = %d, want 3", len(parts))
	}
	// inlineData block for raw bytes
	inline := parts[1].(map[string]any)["inlineData"].(map[string]any)
	if inline["mimeType"] != "image/png" {
		t.Errorf("inlineData mimeType = %v, want image/png", inline["mimeType"])
	}
	if inline["data"] != base64.StdEncoding.EncodeToString(raw) {
		t.Errorf("inlineData data = %v", inline["data"])
	}
	// fileData block for the URL
	fileData := parts[2].(map[string]any)["fileData"].(map[string]any)
	if fileData["fileUri"] != "https://example.com/cat.jpg" {
		t.Errorf("fileData fileUri = %v", fileData["fileUri"])
	}
}

// tool_choice maps onto Gemini's functionCallingConfig modes; "tool" forces ANY plus
// an allowedFunctionNames restriction.
func TestBuildRequest_ToolChoice(t *testing.T) {
	cases := []struct {
		mode        string
		name        string
		wantMode    string
		wantAllowed bool
	}{
		{"", "", "AUTO", false},
		{"auto", "", "AUTO", false},
		{"required", "", "ANY", false},
		{"none", "", "NONE", false},
		{"tool", "get_weather", "ANY", true},
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
			fcc := got["toolConfig"].(map[string]any)["functionCallingConfig"].(map[string]any)
			if fcc["mode"] != c.wantMode {
				t.Errorf("mode %q: functionCallingConfig mode = %v, want %v", c.mode, fcc["mode"], c.wantMode)
			}
			allowed, has := fcc["allowedFunctionNames"]
			if has != c.wantAllowed {
				t.Errorf("mode %q: allowedFunctionNames present = %v, want %v", c.mode, has, c.wantAllowed)
			}
			if c.wantAllowed {
				names := allowed.([]any)
				if len(names) != 1 || names[0] != c.name {
					t.Errorf("mode %q: allowedFunctionNames = %v, want [%s]", c.mode, names, c.name)
				}
			}
		})
	}
}

// Sampling maps onto generationConfig; ResponseFormat sets responseMimeType + schema.
func TestBuildRequest_SamplingAndResponseFormat(t *testing.T) {
	temp := 0.2
	maxTok := 256
	m := New("k")
	body, err := m.buildRequest(agent.Request{
		Messages: []agent.Message{agent.UserText("hi")},
		Sampling: agent.Sampling{Temperature: &temp, MaxTokens: &maxTok, Stop: []string{"END"}},
		ResponseFormat: &agent.ResponseFormat{
			Name:   "answer",
			Schema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	gen := got["generationConfig"].(map[string]any)
	if gen["temperature"].(float64) != 0.2 {
		t.Errorf("temperature = %v", gen["temperature"])
	}
	if gen["maxOutputTokens"].(float64) != 256 {
		t.Errorf("maxOutputTokens = %v", gen["maxOutputTokens"])
	}
	if gen["stopSequences"].([]any)[0] != "END" {
		t.Errorf("stopSequences = %v", gen["stopSequences"])
	}
	if gen["responseMimeType"] != "application/json" {
		t.Errorf("responseMimeType = %v", gen["responseMimeType"])
	}
	sch := gen["responseSchema"].(map[string]any)
	if sch["type"] != "object" {
		t.Errorf("responseSchema type = %v", sch["type"])
	}
}

// A Gemini streamGenerateContent SSE response: a text chunk, a chunk carrying a
// complete functionCall, then a final chunk with finishReason + usageMetadata.
const sample = `data: {"candidates":[{"content":{"parts":[{"text":"Let me check. "}]}}]}

data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_weather","args":{"city":"SF"}}}]}}]}

data: {"candidates":[{"content":{"parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":9,"cachedContentTokenCount":4}}

`

func TestStreamSSE_NormalizesTextToolCallAndUsage(t *testing.T) {
	stream := testStream(sample)

	// Collect the raw event stream to assert on deltas and the terminal Finish.
	var text string
	var gotToolDelta bool
	var finish agent.Finish
	var sawFinish bool
	for ev, err := range stream.Events() {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		switch e := ev.(type) {
		case agent.TextDelta:
			text += e.Text
		case agent.ToolCallDelta:
			gotToolDelta = true
			if e.Name != "get_weather" {
				t.Errorf("tool call name = %q", e.Name)
			}
			if !strings.HasPrefix(e.ID, "call_") || len(e.ID) != len("call_")+24 {
				t.Errorf("synthesized id = %q, want call_ and 24 random hex digits", e.ID)
			}
			if string(e.ArgsFragment) != `{"city":"SF"}` {
				t.Errorf("args fragment = %s, want {\"city\":\"SF\"}", e.ArgsFragment)
			}
		case agent.Finish:
			finish = e
			sawFinish = true
		}
	}

	if text != "Let me check. " {
		t.Errorf("text = %q", text)
	}
	if !gotToolDelta {
		t.Error("missing ToolCallDelta")
	}
	if !sawFinish {
		t.Fatal("missing terminal Finish")
	}
	if finish.Reason != "tool_use" {
		t.Errorf("finish reason = %q, want tool_use (a tool call was seen)", finish.Reason)
	}
	// promptTokenCount 11 includes the 4 cached tokens, which are counted once, as cache reads.
	if finish.Usage.InputTokens != 7 || finish.Usage.OutputTokens != 9 || finish.Usage.CacheReadTokens != 4 {
		t.Errorf("usage = %+v, want {In:7 Out:9 CacheRead:4}", finish.Usage)
	}
}

// The assembled Message drains cleanly: a complete functionCall yields valid JSON args
// through the core's finalize() json.Valid gate.
func TestStreamSSE_AssemblesMessage(t *testing.T) {
	stream := testStream(sample)

	msg, usage, err := stream.Message()
	if err != nil {
		t.Fatalf("Message: %v", err)
	}
	var gotTool bool
	for _, p := range msg.Parts {
		if tu, ok := p.(agent.ToolUse); ok {
			gotTool = true
			if tu.Name != "get_weather" || string(tu.Args) != `{"city":"SF"}` {
				t.Errorf("tool use = %+v", tu)
			}
		}
	}
	if !gotTool {
		t.Error("missing tool_use in assembled message")
	}
	if usage.TotalInputTokens() != 11 || usage.OutputTokens != 9 {
		t.Errorf("usage = %+v", usage)
	}
}

// testStream feeds src through streamSSE the way Stream does.
func testStream(src string) *agent.Stream {
	return agent.NewStreamFunc(context.Background(), func(send func(agent.Emit) bool) {
		streamSSE(io.NopCloser(strings.NewReader(src)), send)
	})
}
