package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// R117-6. The tool's Timeout bounds its middleware too, and the late rule does not ask whether
// the tool was ever called. A middleware that waits for a slot (as middleware.ToolRateLimit does)
// and gives up at the deadline makes a side-effect call that certainly never ran look like one
// that may have fired: the run fails with ErrToolOutcomeUnknown and every resume halts with
// *OutcomeUnknown until a human resolves a call that never happened. The base handler knows
// whether it reached t.Call.
func TestR117_MiddlewareTimeoutBeforeTheToolRunsHaltsAsIfItMayHaveFired(t *testing.T) {
	var calls atomic.Int32
	charge := Func("charge", "", Safety{}, func(context.Context, struct{}) (string, error) {
		calls.Add(1)
		return "charged", nil
	}, WithTimeout(time.Millisecond))
	waitForSlot := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			<-ctx.Done() // the limiter has no slot before the deadline
			return nil, fmt.Errorf("no slot: %w (%w)", ctx.Err(), ErrToolNotCalled) // next was never called
		}
	})
	store := NewMemStore()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	_, err := New(m, store, charge).UseTool(waitForSlot).Run(context.Background(), "r1", "pay")
	t.Logf("first drive: %v", err)
	_, err = New(m, store, charge).Run(context.Background(), "r1", "pay") // resume, limiter gone
	var halt *OutcomeUnknown
	if calls.Load() == 0 && errors.As(err, &halt) {
		t.Fatalf("the tool was never called, yet the resume halts for its outcome: %v", err)
	}
}
