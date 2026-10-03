package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// effectTool is a non-retry-safe tool whose call runs fn.
type effectTool struct {
	name string
	fn   func(ctx context.Context) (json.RawMessage, error)
}

func (t effectTool) Name() string                { return t.name }
func (t effectTool) Description() string         { return "" }
func (t effectTool) ArgsSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t effectTool) Safety() agent.Safety        { return agent.Safety{} }
func (t effectTool) Call(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	return t.fn(ctx)
}

// A call that loses its answer is a side effect of unknown outcome, like a halt, not a failure
// of the turn: it must not cut off its siblings in flight. It used to cancel the turn's context,
// so a sibling still running (here b, which honors cancellation) stopped with its own outcome
// unknown, and the resume halted on it too, in a run that never crashed.
func TestToolOutcomeUnknown_SiblingInFlightFinishes(t *testing.T) {
	bIn := make(chan struct{})
	var bFired atomic.Int32
	a := effectTool{"a", func(context.Context) (json.RawMessage, error) {
		<-bIn
		return nil, fmt.Errorf("connection dropped: %w", agent.ErrToolOutcomeUnknown)
	}}
	b := effectTool{"b", func(ctx context.Context) (json.RawMessage, error) {
		close(bIn)
		select {
		case <-ctx.Done():
			return nil, ctx.Err() // cut off mid-call: its outcome is unknown
		case <-time.After(300 * time.Millisecond):
		}
		bFired.Add(1)
		return json.RawMessage(`{"sent":true}`), nil
	}}
	turn := []agent.Emit{
		{Event: agent.ToolCallDelta{Index: 0, ID: "ca", Name: "a", ArgsFragment: json.RawMessage(`{}`)}},
		{Event: agent.ToolCallDelta{Index: 1, ID: "cb", Name: "b", ArgsFragment: json.RawMessage(`{}`)}},
		{Event: agent.Finish{Reason: "tool_use"}},
	}
	store := agenttest.MemJournal()
	ag := agenttest.MustNew(modelFunc(func() []agent.Emit { return turn }), store, agent.WithTools(a, b))
	_, err := ag.Run(context.Background(), "r", agent.UserText("go"))
	if !errors.Is(err, agent.ErrToolOutcomeUnknown) {
		t.Fatalf("Run = %v; want the lost answer", err)
	}
	if bFired.Load() != 1 {
		t.Fatalf("b's effect fired %d times; want 1: a lost answer must not cut off a sibling in flight", bFired.Load())
	}
	_, err = ag.Run(context.Background(), "r", agent.UserText("go"))
	var halt *agent.OutcomeUnknown
	if !errors.As(err, &halt) || halt.Op.ID != "ca" {
		t.Fatalf("resume = %v; want a halt on ca alone", err)
	}
}
