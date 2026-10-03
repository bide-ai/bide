package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// A server that drops the connection after it has run a call but before it answers leaves the
// call's outcome unknown: the transfer may have happened. Journaling that as a failed call tells
// the model the transfer did not happen, so it asks again, and nothing stops the resume either.
// The run must stop without recording a result, and a resume must halt rather than transfer again.
func TestCall_ConnectionLostMidCallIsUnknownOutcome(t *testing.T) {
	var transfers atomic.Int32
	transferThenHangup := func(c *rawCall) {
		transfers.Add(1)
		c.Hangup()
	}
	store := agenttest.MemJournal()
	script := func() *agent.ScriptedModel {
		return agent.NewScriptedModel(
			agent.ToolTurn("c1", "transfer", `{"cents":500}`),
			agent.ToolTurn("c2", "transfer", `{"cents":500}`),
			agent.TextTurn("done"),
		)
	}

	session := connectRaw(t, &rawServer{tools: []json.RawMessage{rawTool("transfer")}, call: transferThenHangup})
	tools, err := Tools(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	_, err = agenttest.MustNew(script(), store, agent.WithTools(tools...)).Run(context.Background(), "r1", "send $5")
	if !errors.Is(err, agent.ErrToolOutcomeUnknown) {
		t.Fatalf("run err = %v, want ErrToolOutcomeUnknown: the transfer may have happened", err)
	}
	recs, _ := store.History(context.Background(), "r1")
	for _, r := range recs {
		if r.Kind == agent.StepToolResult && r.ToolUseID == "c1" {
			t.Fatalf("journaled a result for a call with an unknown outcome: %s", r.Result)
		}
	}

	// The host reconnects and resumes the run.
	session = connectRaw(t, &rawServer{tools: []json.RawMessage{rawTool("transfer")}, call: transferThenHangup})
	if tools, err = Tools(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	_, err = agenttest.MustNew(script(), store, agent.WithTools(tools...)).Run(context.Background(), "r1", "send $5")
	var halt *agent.ResumeHalt
	if !errors.As(err, &halt) || halt.Op.ID != "c1" {
		t.Fatalf("resume err = %v, want *ResumeHalt for c1", err)
	}
	if n := transfers.Load(); n != 1 {
		t.Fatalf("the server made %d transfers, want 1", n)
	}
}

// Only a call that may have run is an unknown outcome. A JSON-RPC error is the server's answer,
// a closed session never sent the request, and arguments that are not JSON are refused before
// it is sent: each is an ordinary failure.
func TestCall_KnownFailuresAreNotUnknownOutcomes(t *testing.T) {
	ctx := context.Background()
	s := &rawServer{tools: []json.RawMessage{rawTool("transfer")}, call: func(c *rawCall) { c.ReplyError(-32603, "insufficient funds") }}
	session := connectRaw(t, s)
	tools, err := Tools(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	tl := tools[0]
	known := func(what string, err error) {
		t.Helper()
		if err == nil || !errors.Is(err, agent.ErrTool) || errors.Is(err, agent.ErrToolOutcomeUnknown) {
			t.Errorf("%s: err = %v, want an ErrTool failure that is not ErrToolOutcomeUnknown", what, err)
		}
	}
	_, err = tl.Call(ctx, json.RawMessage(`{"cents":500}`))
	known("JSON-RPC error", err)

	_, err = tl.Call(ctx, json.RawMessage(`{"cents":`))
	known("invalid arguments", err)
	if !errors.Is(err, agent.ErrToolArgs) {
		t.Errorf("invalid arguments: err = %v, want ErrToolArgs", err)
	}

	session.Close()
	_, err = tl.Call(ctx, json.RawMessage(`{}`))
	known("closed session", err)
}

// A deadline that passes while the server is still working leaves the outcome unknown: the
// server may finish the call after the client stops waiting.
func TestCall_DeadlineIsUnknownOutcome(t *testing.T) {
	session := connectRaw(t, &rawServer{tools: []json.RawMessage{rawTool("transfer")}, call: func(*rawCall) {}})
	tools, err := Tools(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := tools[0].Call(ctx, json.RawMessage(`{}`)); !errors.Is(err, agent.ErrToolOutcomeUnknown) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want ErrToolOutcomeUnknown wrapping the deadline", err)
	}
}

// A retry-safe tool whose call is lost may simply be run again, so the loss is an ordinary
// failure the model sees and the run carries on.
func TestCall_ConnectionLostOnRetrySafeToolIsAFailure(t *testing.T) {
	ro, _ := json.Marshal(map[string]any{"name": "balance", "inputSchema": map[string]any{"type": "object"},
		"annotations": map[string]any{"readOnlyHint": true}})
	session := connectRaw(t, &rawServer{tools: []json.RawMessage{ro}, call: func(c *rawCall) { c.Hangup() }})
	tools, err := Tools(context.Background(), session, TrustAnnotations())
	if err != nil {
		t.Fatal(err)
	}
	m := agent.NewScriptedModel(agent.ToolTurn("c1", "balance", `{}`), agent.TextTurn("could not read it"))
	if _, err := agenttest.MustNew(m, agenttest.MemJournal(), agent.WithTools(tools...)).Run(context.Background(), "r1", "balance?"); err != nil {
		t.Fatalf("run err = %v, want the lost read to be a failure the model sees", err)
	}
}

// A trusted server that relabels a tool read-only after a call to it was lost must not get that
// call run again on resume: the call fired as a side effect, and that is what the resume honours.
func TestResume_RelabelledByTrustedServerStillHalts(t *testing.T) {
	var transfers atomic.Int32
	store := agenttest.MemJournal()
	session := connectRaw(t, &rawServer{tools: []json.RawMessage{rawTool("transfer")}, call: func(c *rawCall) {
		transfers.Add(1)
		c.Hangup()
	}})
	tools, err := Tools(context.Background(), session, TrustAnnotations())
	if err != nil {
		t.Fatal(err)
	}
	m := agent.NewScriptedModel(agent.ToolTurn("c1", "transfer", `{"cents":500}`), agent.TextTurn("done"))
	if _, err := agenttest.MustNew(m, store, agent.WithTools(tools...)).Run(context.Background(), "r1", "send $5"); !errors.Is(err, agent.ErrToolOutcomeUnknown) {
		t.Fatalf("run err = %v, want ErrToolOutcomeUnknown", err)
	}

	readOnly, _ := json.Marshal(map[string]any{"name": "transfer", "inputSchema": map[string]any{"type": "object"},
		"annotations": map[string]any{"readOnlyHint": true}})
	session = connectRaw(t, &rawServer{tools: []json.RawMessage{readOnly}, call: func(c *rawCall) {
		transfers.Add(1)
		c.Reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": "sent"}}})
	}})
	if tools, err = Tools(context.Background(), session, TrustAnnotations()); err != nil {
		t.Fatal(err)
	}
	_, err = agenttest.MustNew(agent.NewScriptedModel(agent.TextTurn("done")), store, agent.WithTools(tools...)).Run(context.Background(), "r1", "send $5")
	var halt *agent.ResumeHalt
	if !errors.As(err, &halt) || halt.Op.ID != "c1" {
		t.Fatalf("resume err = %v after %d transfers, want *ResumeHalt for c1", err, transfers.Load())
	}
	if n := transfers.Load(); n != 1 {
		t.Fatalf("the server made %d transfers, want 1", n)
	}
}
