package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
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
	store := agent.NewMemStore()
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
	_, err = agent.New(script(), store, tools...).Run(context.Background(), "r1", "send $5")
	if err == nil {
		t.Fatal("the run completed as if the lost transfer had failed; want it to stop: the transfer may have happened")
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
	_, err = agent.New(script(), store, tools...).Run(context.Background(), "r1", "send $5")
	var halt *agent.ResumeHalt
	if !errors.As(err, &halt) || halt.ToolUseID != "c1" {
		t.Fatalf("resume err = %v, want *ResumeHalt for c1", err)
	}
	if n := transfers.Load(); n != 1 {
		t.Fatalf("the server made %d transfers, want 1", n)
	}
}
