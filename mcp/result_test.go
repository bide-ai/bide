package mcp

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
