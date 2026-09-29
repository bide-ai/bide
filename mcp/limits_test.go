package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

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

// callWith lists a one-tool raw server with opts and calls its tool once.
func callWith(t *testing.T, reply func(*rawCall), opts ...ToolsOption) (json.RawMessage, error) {
	t.Helper()
	session := connectRaw(t, &rawServer{tools: []json.RawMessage{rawTool("dump")}, call: reply})
	tools, err := Tools(context.Background(), session, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return tools[0].Call(context.Background(), json.RawMessage(`{}`))
}

// The result cap is exact, configurable, and can be removed.
func TestWithMaxResultBytes(t *testing.T) {
	text := strings.Repeat("x", 100)
	size := len(`[{"type":"text","text":"` + text + `"}]`)
	reply := func(c *rawCall) { c.Reply(textResult(text, false)) }
	if _, err := callWith(t, reply, WithMaxResultBytes(size)); err != nil {
		t.Errorf("a result of exactly the limit: %v", err)
	}
	_, err := callWith(t, reply, WithMaxResultBytes(size-1))
	if !errors.Is(err, ErrResultTooLarge) || !errors.Is(err, agent.ErrTool) || errors.Is(err, agent.ErrToolOutcomeUnknown) {
		t.Errorf("a result one byte over the limit: err = %v, want ErrResultTooLarge", err)
	}
	huge := strings.Repeat("x", 2<<20)
	if out, err := callWith(t, func(c *rawCall) { c.Reply(textResult(huge, false)) }, WithMaxResultBytes(0)); err != nil || len(out) < 2<<20 {
		t.Errorf("with no limit: %d bytes, %v; want the whole result", len(out), err)
	}
	// The default is 1 MiB, as documented.
	if out, err := callWith(t, func(c *rawCall) { c.Reply(textResult(strings.Repeat("x", 1<<20-100), false)) }); err != nil {
		t.Errorf("a result under the default limit: %d bytes, %v", len(out), err)
	}
	if _, err := callWith(t, func(c *rawCall) { c.Reply(textResult(strings.Repeat("x", 1<<20), false)) }); !errors.Is(err, ErrResultTooLarge) {
		t.Errorf("a result over the default limit: err = %v, want ErrResultTooLarge", err)
	}
}

// An oversized result from a side effect is a failure the model sees, not an unknown outcome:
// the run carries on, and the journal says the tool ran.
func TestOversizedResult_RunRecordsAFailure(t *testing.T) {
	session := connectRaw(t, &rawServer{tools: []json.RawMessage{rawTool("export")}, call: func(c *rawCall) {
		c.Reply(textResult(strings.Repeat("x", 2<<20), false))
	}})
	tools, err := Tools(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	store := agent.NewMemStore()
	m := agent.NewScriptedModel(agent.ToolTurn("c1", "export", `{}`), agent.TextTurn("done"))
	if _, err := agent.New(m, store, tools...).Run(context.Background(), "r1", "export"); err != nil {
		t.Fatalf("run err = %v, want the failure passed to the model", err)
	}
	hist, _ := store.History(context.Background(), "r1")
	for _, r := range hist {
		if r.Kind == agent.StepToolResult && r.ToolUseID == "c1" {
			if !r.IsError || !strings.Contains(string(r.Result), "ran") || len(r.Result) > 4096 {
				t.Fatalf("journaled %.200s (is_error=%v), want a short failure saying the tool ran", r.Result, r.IsError)
			}
			return
		}
	}
	t.Fatal("no result journaled for c1")
}

// The description cap is exact, configurable, and can be removed.
func TestWithMaxDescriptionBytes(t *testing.T) {
	list := func(desc string, opts ...ToolsOption) error {
		def, _ := json.Marshal(map[string]any{"name": "lookup", "description": desc, "inputSchema": map[string]any{"type": "object"}})
		_, err := Tools(context.Background(), connectRaw(t, &rawServer{tools: []json.RawMessage{def}}), opts...)
		return err
	}
	if err := list(strings.Repeat("d", 10), WithMaxDescriptionBytes(10)); err != nil {
		t.Errorf("a description of exactly the limit: %v", err)
	}
	if err := list(strings.Repeat("d", 11), WithMaxDescriptionBytes(10)); !errors.Is(err, agent.ErrProtocol) {
		t.Errorf("a description one byte over the limit: err = %v, want ErrProtocol", err)
	}
	if err := list(strings.Repeat("d", 64<<10), WithMaxDescriptionBytes(0)); err != nil {
		t.Errorf("with no limit: %v", err)
	}
	// The default is 8 KiB, as documented.
	if err := list(strings.Repeat("d", 8<<10)); err != nil {
		t.Errorf("a description of the default limit: %v", err)
	}
	if err := list(strings.Repeat("d", 8<<10+1)); !errors.Is(err, agent.ErrProtocol) {
		t.Errorf("a description one byte over the default limit: err = %v, want ErrProtocol", err)
	}
}

// A call that outlives WithCallTimeout fails as an unknown outcome: the server may still be
// running it. Without the option a slow call is left to finish.
func TestWithCallTimeout(t *testing.T) {
	never := func(*rawCall) {}
	errc := make(chan error, 1)
	go func() {
		_, err := callWith(t, never, WithCallTimeout(30*time.Millisecond))
		errc <- err
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, agent.ErrToolOutcomeUnknown) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want ErrToolOutcomeUnknown wrapping the deadline", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the call outlived its 30ms timeout by 2s")
	}
	slow := func(c *rawCall) { time.Sleep(100 * time.Millisecond); c.Reply(textResult("ok", false)) }
	for _, opts := range [][]ToolsOption{nil, {WithCallTimeout(0)}, {WithCallTimeout(5 * time.Second)}} {
		if _, err := callWith(t, slow, opts...); err != nil {
			t.Errorf("a slow call with options %d: %v", len(opts), err)
		}
	}
}
