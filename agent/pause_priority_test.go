package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// signalTool is a read-only tool whose call runs fn.
type signalTool struct {
	name string
	fn   func() (json.RawMessage, error)
}

func (t signalTool) Name() string                { return t.name }
func (t signalTool) Description() string         { return "" }
func (t signalTool) ArgsSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t signalTool) Safety() agent.Safety        { return agent.Safety{ReadOnly: true} }
func (t signalTool) Call(context.Context, json.RawMessage) (json.RawMessage, error) {
	return t.fn()
}

// When the calls of a turn pause in different ways, the run reports a halt ahead of any other
// pause, even one from an earlier call that paused later: the halt needs an operator, and the
// run cannot go on until it is resolved, whatever else is answered.
//
// With two calls allowed at once, b halts while a waits; c starts only once b's goroutine has
// finished, and releases a, which then pauses for an approval.
func TestRun_HaltReportedAheadOfAnEarlierCallsPause(t *testing.T) {
	release := make(chan struct{})
	a := signalTool{"a", func() (json.RawMessage, error) {
		select {
		case <-release:
		case <-time.After(10 * time.Second):
			return nil, errors.New("c never ran")
		}
		return nil, &agent.ApprovalPending{RunRef: agent.RunRef{RunID: "r/a", RootRunID: "r"}, ToolUseID: "inner", ToolName: "gated"}
	}}
	b := signalTool{"b", func() (json.RawMessage, error) {
		return nil, &agent.OutcomeUnknown{RunRef: agent.RunRef{RunID: "r/b", RootRunID: "r"}, Op: agent.OpRef{Kind: agent.OpTool, ID: "effect", ToolName: "charge"}, Cause: agent.HaltCrashed}
	}}
	c := signalTool{"c", func() (json.RawMessage, error) {
		close(release)
		return json.RawMessage(`{}`), nil
	}}
	turn := []agent.Emit{
		{Event: agent.ToolCallDelta{Index: 0, ID: "ca", Name: "a", ArgsFragment: json.RawMessage(`{}`)}},
		{Event: agent.ToolCallDelta{Index: 1, ID: "cb", Name: "b", ArgsFragment: json.RawMessage(`{}`)}},
		{Event: agent.ToolCallDelta{Index: 2, ID: "cc", Name: "c", ArgsFragment: json.RawMessage(`{}`)}},
		{Event: agent.Finish{Reason: "tool_use"}},
	}
	model := modelFunc(func() []agent.Emit { return turn })
	_, err := agenttest.MustNew(model, agenttest.MemJournal(), agent.WithTools(a, b, c), agent.WithMaxConcurrency(2)).Run(context.Background(), "r", agent.UserText("go"))
	var halt *agent.OutcomeUnknown
	if !errors.As(err, &halt) || halt.Op.ID != "effect" {
		t.Fatalf("Run = %v; want the halt on effect", err)
	}
}

// modelFunc is a model that streams what fn returns.
type modelFunc func() []agent.Emit

func (m modelFunc) Stream(context.Context, agent.Request) (*agent.Stream, error) {
	emits := m()
	ch := make(chan agent.Emit, len(emits))
	for _, e := range emits {
		ch <- e
	}
	close(ch)
	return agent.NewStream(ch), nil
}
