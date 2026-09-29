package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// textResult is a tools/call result holding one text block.
func textResult(text string, isError bool) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": isError}
}

// A result is journaled and sent back to the model on every later turn, so a server that answers
// with megabytes fills the journal and the context window. By default a result over the cap is
// refused with an error; the call ran, so the error is a definite failure that says so, never a
// silently truncated result and never an unknown outcome.
func TestCall_OversizedResultIsRefused(t *testing.T) {
	for _, isError := range []bool{false, true} {
		huge := strings.Repeat("x", 2<<20)
		session := connectRaw(t, &rawServer{tools: []json.RawMessage{rawTool("dump")}, call: func(c *rawCall) { c.Reply(textResult(huge, isError)) }})
		tools, err := Tools(context.Background(), session)
		if err != nil {
			t.Fatal(err)
		}
		out, err := tools[0].Call(context.Background(), json.RawMessage(`{}`))
		if err == nil || !errors.Is(err, agent.ErrTool) || errors.Is(err, agent.ErrToolOutcomeUnknown) {
			t.Fatalf("isError=%v: Call = %d bytes, %v; want a definite ErrTool failure", isError, len(out), err)
		}
		if len(err.Error()) > 4096 || !strings.Contains(err.Error(), "ran") {
			t.Fatalf("isError=%v: error is %d bytes: %.200s; want a short error saying the tool ran", isError, len(err.Error()), err)
		}
	}
}

// A description is sent to the model with every request, for every tool, so a server's
// multi-megabyte description costs every turn of every run and is room for a prompt injection.
// By default Tools refuses a description over the cap.
func TestTools_OversizedDescriptionIsRefused(t *testing.T) {
	def, _ := json.Marshal(map[string]any{"name": "lookup", "description": strings.Repeat("d", 64<<10),
		"inputSchema": map[string]any{"type": "object"}})
	session := connectRaw(t, &rawServer{tools: []json.RawMessage{def}})
	tools, err := Tools(context.Background(), session)
	if !errors.Is(err, agent.ErrProtocol) {
		t.Fatalf("Tools = %d tools, %v; want ErrProtocol", len(tools), err)
	}
}
