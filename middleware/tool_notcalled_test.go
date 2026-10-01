package middleware_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/middleware"
)

// A rate limiter that gives up waiting for a slot past the tool's deadline never called the tool:
// the agent records the call as a known failure, and a resume does not halt for it.
// It runs in a synctest bubble: on the wall clock the first call's 20ms deadline can pass while
// it is dispatched, and the agent then refuses it as not started, so nothing is charged. In a
// bubble the clock advances only once every goroutine blocks, so the first call reaches the tool.
func TestToolRateLimit_GivingUpRecordsNotCalled(t *testing.T) {
	synctest.Test(t, testToolRateLimitGivingUpRecordsNotCalled)
}

func testToolRateLimitGivingUpRecordsNotCalled(t *testing.T) {
	r := middleware.NewRateLimiter(time.Hour, 1) // one slot an hour
	var calls atomic.Int32
	charge := agent.Func("charge", "", agent.Safety{}, func(context.Context, struct{}) (string, error) {
		calls.Add(1)
		return "charged", nil
	}, agent.WithTimeout(20*time.Millisecond))
	store := agent.NewMemStore()
	m := agent.NewScriptedModel(agent.ToolTurn("c1", "charge", `{}`), agent.ToolTurn("c2", "charge", `{}`), agent.TextTurn("done"))
	a := agent.New(m, store, charge).UseTool(middleware.ToolRateLimit(r))
	if _, err := a.Run(context.Background(), "r1", "pay"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("charged %d times, want 1: the second call waits for a slot past its deadline", calls.Load())
	}
	recs, err := store.History(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		if rec.Kind == agent.StepToolResult && rec.ToolUseID == "c2" {
			if !rec.IsError {
				t.Fatalf("c2 recorded %s, want the known failure", rec.Result)
			}
			return
		}
	}
	t.Fatal("no result recorded for the call the limiter gave up on")
}

// ToolRetry says a call it never passed to next was not called: an invalid retry count, or a
// context already done before the first attempt.
func TestToolRetry_NeverCalledSaysSo(t *testing.T) {
	var calls int
	next := agent.ToolHandler(func(context.Context, agent.ToolCall) (json.RawMessage, error) { calls++; return nil, nil })
	idem := agent.ToolCall{Use: agent.ToolUse{ID: "c1", Name: "t"}, Spec: agent.ToolSpec{Safety: agent.Safety{Idempotent: true}}}
	if _, err := middleware.ToolRetry(-1)(next)(context.Background(), idem); !errors.Is(err, agent.ErrToolNotCalled) || !errors.Is(err, agent.ErrConfig) {
		t.Errorf("negative count: %v, want ErrConfig and ErrToolNotCalled", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := middleware.ToolRetry(2)(next)(ctx, idem); !errors.Is(err, agent.ErrToolNotCalled) {
		t.Errorf("done context: %v, want ErrToolNotCalled", err)
	}
	if calls != 0 {
		t.Fatalf("next called %d times", calls)
	}
}
