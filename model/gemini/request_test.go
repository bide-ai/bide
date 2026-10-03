package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// contentsOf builds req and returns its contents array.
func contentsOf(t *testing.T, req agent.Request) []map[string]any {
	t.Helper()
	body, err := New("k").buildRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Contents []map[string]any `json:"contents"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	return got.Contents
}

// Gemini answers a model turn's function calls with ONE content holding a functionResponse
// part per call, in the calls' order. The agent emits one tool message per result, and on a
// resumed run in the order the results were journaled, not the order of the calls.
func TestBuildRequest_ParallelResultsShareOneContentInCallOrder(t *testing.T) {
	contents := contentsOf(t, agent.Request{Messages: []agent.Message{
		agent.UserText("x"),
		{Role: agent.RoleAssistant, Parts: []agent.Part{
			agent.ToolUse{ID: "id-a", Name: "a", Args: json.RawMessage(`{}`)},
			agent.ToolUse{ID: "id-b", Name: "b", Args: json.RawMessage(`{}`)},
		}},
		// A result for no known call goes last; a tool turn carries nothing but results.
		{Role: agent.RoleTool, Parts: []agent.Part{agent.ToolResult{ToolUseID: "zz", Result: json.RawMessage(`0`)}, agent.Text{Text: "stray"}}},
		{Role: agent.RoleTool, Parts: []agent.Part{agent.ToolResult{ToolUseID: "id-b", Result: json.RawMessage(`2`)}}},
		{Role: agent.RoleTool, Parts: []agent.Part{agent.ToolResult{ToolUseID: "id-a", Result: json.RawMessage(`1`)}}},
	}})
	if len(contents) != 3 {
		t.Fatalf("contents = %d, want 3 (user, model, one user content with every response): %v", len(contents), contents)
	}
	parts := contents[2]["parts"].([]any)
	if len(parts) != 3 {
		t.Fatalf("function responses = %d parts, want 3: %v", len(parts), parts)
	}
	for i, want := range []string{"a", "b", "zz"} {
		fr := parts[i].(map[string]any)["functionResponse"].(map[string]any)
		if fr["name"] != want {
			t.Errorf("response %d is for %v, want %s (the calls' order)", i, fr["name"], want)
		}
	}
}

// A user message after the function responses stays its own content: responses are not mixed
// with text, while other consecutive same-role turns merge.
func TestBuildRequest_MergesSameRoleButKeepsResponsesApart(t *testing.T) {
	contents := contentsOf(t, agent.Request{Messages: []agent.Message{
		agent.UserText("one"), agent.UserText("two"),
		{Role: agent.RoleAssistant, Parts: []agent.Part{agent.ToolUse{ID: "i", Name: "a", Args: json.RawMessage(`{}`)}}},
		{Role: agent.RoleTool, Parts: []agent.Part{agent.ToolResult{ToolUseID: "i", Result: json.RawMessage(`1`)}}},
		agent.UserText("three"),
	}})
	if len(contents) != 4 {
		t.Fatalf("contents = %d, want 4: %v", len(contents), contents)
	}
	if n := len(contents[0]["parts"].([]any)); n != 2 {
		t.Errorf("first user content has %d parts, want 2 (merged)", n)
	}
	if _, ok := contents[3]["parts"].([]any)[0].(map[string]any)["text"]; !ok {
		t.Errorf("last content = %v, want the user text on its own", contents[3])
	}
}

// A turn with nothing Gemini can take (an assistant turn holding only reasoning, an empty
// text) is left out rather than sent as {"parts":null}, which Gemini rejects.
func TestBuildRequest_SkipsTurnsWithNoParts(t *testing.T) {
	body, err := New("k").buildRequest(agent.Request{Messages: []agent.Message{
		agent.SystemText(""),
		agent.UserText("x"),
		{Role: agent.RoleAssistant, Parts: []agent.Part{agent.Reasoning{Text: "hmm", Signature: "s"}}},
		agent.UserText(""),
		{Role: agent.RoleAssistant, Parts: []agent.Part{agent.Text{Text: "ok"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"parts":null`) || strings.Contains(string(body), `"text":""`) {
		t.Fatalf("request carries an empty turn or part: %s", body)
	}
	var got struct{ Contents []map[string]any }
	json.Unmarshal(body, &got)
	if len(got.Contents) != 2 {
		t.Fatalf("contents = %d, want 2: %s", len(got.Contents), body)
	}
}

// Gemini 3 attaches a thoughtSignature to a functionCall part and rejects the next turn unless
// it comes back on that part.
func TestRun_ThoughtSignatureRoundTrips(t *testing.T) {
	srv, bodies := sseServer(t,
		`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{}},"thoughtSignature":"sig-1"}]},"finishReason":"STOP"}]}`,
		`{"candidates":[{"content":{"parts":[{"text":"done"}]},"finishReason":"STOP"}]}`,
	)
	tool := agent.Func("lookup", "l", agent.Safety{ReadOnly: true}, func(context.Context, struct{}) (string, error) { return "r", nil })
	if _, err := agenttest.MustNew(New("k", WithBaseURL(srv.URL)), agenttest.MemJournal(), agent.WithTools(tool)).Run(context.Background(), "r", agent.UserText("go")); err != nil {
		t.Fatal(err)
	}
	var req struct {
		Contents []struct {
			Parts []map[string]json.RawMessage `json:"parts"`
		} `json:"contents"`
	}
	json.Unmarshal([]byte(bodies()[1]), &req)
	var found bool
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			if _, ok := p["functionCall"]; ok {
				found = true
				if string(p["thoughtSignature"]) != `"sig-1"` {
					t.Fatalf("functionCall part = %v, want thoughtSignature sig-1", p)
				}
			}
		}
	}
	if !found {
		t.Fatal("no functionCall in the second request")
	}
}

// A part flagged thought:true is the model's reasoning, not its answer.
func TestStreamSSE_ThoughtPartsAreReasoning(t *testing.T) {
	msg, _, err := testStream(`data: {"candidates":[{"content":{"parts":[{"text":"pondering","thought":true},{"text":"answer"}]},"finishReason":"STOP"}]}` + "\n\n").Message()
	if err != nil {
		t.Fatal(err)
	}
	if msg.Text() != "answer" {
		t.Fatalf("text = %q, want answer (the thought kept out)", msg.Text())
	}
	if r, ok := msg.Parts[0].(agent.Reasoning); !ok || r.Text != "pondering" {
		t.Fatalf("parts = %+v, want the thought as Reasoning", msg.Parts)
	}
}

// fileData requires a mimeType. Without Image.Mime it is taken from the URL's extension; with
// neither, the request is refused before it reaches Gemini.
func TestBuildRequest_FileDataMimeType(t *testing.T) {
	for _, tc := range []struct {
		img  agent.Image
		want string
	}{
		{agent.ImageURL("https://x/y/chart.png?sig=1"), "image/png"},
		{agent.Image{URL: "gs://b/o", Mime: "image/webp"}, "image/webp"},
	} {
		contents := contentsOf(t, agent.Request{Messages: []agent.Message{agent.UserParts(tc.img)}})
		fd := contents[0]["parts"].([]any)[0].(map[string]any)["fileData"].(map[string]any)
		if fd["mimeType"] != tc.want || fd["fileUri"] != tc.img.URL {
			t.Errorf("fileData = %v, want mimeType %s", fd, tc.want)
		}
	}
	_, err := New("k").buildRequest(agent.Request{Messages: []agent.Message{agent.UserParts(agent.ImageURL("https://x/image"))}})
	if !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("err = %v, want ErrConfig for an image URL with no known type", err)
	}
}

// A tool with no arguments declares no parameters: Gemini rejects an OBJECT with empty
// properties.
func TestBuildRequest_NoArgToolOmitsParameters(t *testing.T) {
	body, err := New("k").buildRequest(agent.Request{Tools: []agent.ToolSpec{agent.SpecOf(simpleTool())}})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Tools []struct {
			FunctionDeclarations []map[string]any `json:"functionDeclarations"`
		} `json:"tools"`
	}
	json.Unmarshal(body, &got)
	if d := got.Tools[0].FunctionDeclarations[0]; d["parameters"] != nil {
		t.Fatalf("declaration = %v, want no parameters", d)
	}
}

// A tool schema Gemini's subset cannot express (a map, an unconstrained value) is a config
// error naming the tool, not a 400 from Gemini.
func TestBuildRequest_InexpressibleToolSchemaIsAConfigError(t *testing.T) {
	for name, tool := range map[string]agent.Tool{
		"map": agent.Func("m", "m", agent.Safety{}, func(_ context.Context, _ struct {
			M map[string]int `json:"m"`
		}) (int, error) {
			return 0, nil
		}),
		"any": agent.Func("a", "a", agent.Safety{}, func(_ context.Context, _ struct {
			A any `json:"a"`
		}) (int, error) {
			return 0, nil
		}),
	} {
		_, err := New("k").buildRequest(agent.Request{Tools: []agent.ToolSpec{agent.SpecOf(tool)}})
		if !errors.Is(err, agent.ErrConfig) || !strings.Contains(fmtErr(err), tool.Name()) {
			t.Errorf("%s: err = %v, want ErrConfig naming the tool", name, err)
		}
	}
}

// Optional fields reach Gemini as nullable, not as a JSON Schema type list it cannot read.
func TestBuildRequest_ToolSchemaIsTranslated(t *testing.T) {
	tool := agent.Func("t", "t", agent.Safety{}, func(_ context.Context, _ struct {
		N *int `json:"n"`
	}) (int, error) {
		return 0, nil
	})
	req := agent.Request{Tools: []agent.ToolSpec{agent.SpecOf(tool)}, ResponseFormat: &agent.ResponseFormat{Name: "r",
		Schema: json.RawMessage(`{"type":"object","properties":{"s":{"type":["string","null"]}},"additionalProperties":false}`)}}
	body, err := New("k").buildRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"additionalProperties"`) || strings.Contains(string(body), `["string","null"]`) ||
		!strings.Contains(string(body), `"nullable":true`) {
		t.Fatalf("schemas not translated: %s", body)
	}
}

func fmtErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
