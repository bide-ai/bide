package mcptools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// A tool may answer with structuredContent alone: the specification only says it SHOULD repeat
// it as text. Returning the empty content list would tell the model the tool returned nothing,
// after the tool has run.
func TestCall_StructuredContentOnly(t *testing.T) {
	for _, isError := range []bool{false, true} {
		session := connectRaw(t, &rawServer{tools: []json.RawMessage{rawTool("balance")}, call: func(c *rawCall) {
			c.Reply(map[string]any{"structuredContent": map[string]any{"cents": 4217}, "isError": isError})
		}})
		tools, err := Tools(context.Background(), session)
		if err != nil {
			t.Fatal(err)
		}
		out, err := tools[0].Call(context.Background(), json.RawMessage(`{}`))
		got := string(out)
		if err != nil {
			got = err.Error()
		}
		if (err != nil) != isError || !strings.Contains(got, "4217") {
			t.Errorf("isError=%v: Call = %s, %v; want the structured content", isError, out, err)
		}
	}
}

// Content, when the server sends it, stays the result: it is what the specification defines as
// the tool's output, and structured content beside it is the same data in another form. A result
// with neither stays an empty content list.
func TestCall_ContentIsTheResult(t *testing.T) {
	cases := []struct {
		name   string
		result map[string]any
		want   string
	}{
		{"content and structured", map[string]any{
			"content":           []any{map[string]any{"type": "text", "text": `{"cents":4217}`}},
			"structuredContent": map[string]any{"cents": 4217},
		}, `[{"type":"text","text":"{\"cents\":4217}"}]`},
		{"empty", map[string]any{"content": []any{}}, `[]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			session := connectRaw(t, &rawServer{tools: []json.RawMessage{rawTool("balance")}, call: func(rc *rawCall) { rc.Reply(c.result) }})
			tools, err := Tools(context.Background(), session)
			if err != nil {
				t.Fatal(err)
			}
			out, err := tools[0].Call(context.Background(), json.RawMessage(`{}`))
			if err != nil || string(out) != c.want {
				t.Errorf("Call = %s, %v; want %s", out, err, c.want)
			}
		})
	}
}
