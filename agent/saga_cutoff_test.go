package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// syncTool is a retry-safe tool that does not watch its context: it runs whatever state the run
// is in by the time it is called.
type syncTool struct {
	name string
	fn   func() (json.RawMessage, error)
}

func (t syncTool) Name() string                { return t.name }
func (t syncTool) Description() string         { return "" }
func (t syncTool) ArgsSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t syncTool) Safety() agent.Safety        { return agent.Safety{Idempotent: true} }
func (t syncTool) Call(context.Context, json.RawMessage) (json.RawMessage, error) {
	return t.fn()
}

// In a sequential saga, a step fails; the call after it in the turn has not started, and must
// not start: the saga has already failed. It used to be called anyway, on the cancelled context,
// so a tool that does not watch its context (here a retry-safe one, which no attempt claim
// guards) ran its side effect after the transaction had failed.
func TestSaga_CallAfterTheFailureIsNotCalled(t *testing.T) {
	var fired atomic.Int32
	fail := syncTool{"fail", func() (json.RawMessage, error) { return nil, errors.New("no seats") }}
	notify := syncTool{"notify", func() (json.RawMessage, error) { fired.Add(1); return json.RawMessage(`{}`), nil }}
	turn := []agent.Emit{
		{Event: agent.ToolCallDelta{Index: 0, ID: "f1", Name: "fail", ArgsFragment: json.RawMessage(`{}`)}},
		{Event: agent.ToolCallDelta{Index: 1, ID: "n1", Name: "notify", ArgsFragment: json.RawMessage(`{}`)}},
		{Event: agent.Finish{Reason: "tool_use"}},
	}
	a := agenttest.MustNew(
		modelFunc(func() []agent.Emit { return turn }),
		agenttest.MemJournal(),
		agent.WithTools(fail, notify), agent.WithMaxConcurrency(1))
	_, err := a.RunSaga(context.Background(), "r", "go")
	var aborted *agent.SagaAborted
	if !errors.As(err, &aborted) {
		t.Fatalf("RunSaga = %v, want *SagaAborted", err)
	}
	if fired.Load() != 0 {
		t.Fatalf("notify ran %d times after the saga's step failed, want 0", fired.Load())
	}
}
