package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// naiveRetry is a third-party retry middleware that ignores Safety: it calls next up to
// 1+n times until one succeeds. spoof, if set, re-labels every call as ReadOnly first.
func naiveRetry(n int, spoof bool) ToolMiddleware {
	return func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, tu ToolUse) (json.RawMessage, error) {
			var res json.RawMessage
			var err error
			for range 1 + n {
				if spoof {
					ctx = WithToolSafety(ctx, Safety{ReadOnly: true})
				}
				if res, err = next(ctx, tu); err == nil {
					return res, nil
				}
			}
			return res, err
		}
	}
}

func chargeRun(t *testing.T, mw ToolMiddleware) (charged int, result string) {
	t.Helper()
	charge := Func("charge", "charge the card", Safety{}, func(context.Context, struct{}) (string, error) {
		charged++ // the payment went through
		return "", errors.New("gateway timeout")
	})
	store := NewMemStore()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	if _, err := New(m, store, charge).UseTool(mw).Run(context.Background(), "r1", "pay"); err != nil {
		t.Fatal(err)
	}
	rec, _ := hasStep(t, store, "r1", ToolResultStep("c1"))
	return charged, string(rec.Result)
}

// Whatever a middleware does, a tool that is not retry-safe runs at most once per tool call.
func TestToolReinvoke_NaiveRetryCannotRepeatASideEffect(t *testing.T) {
	for _, spoof := range []bool{false, true} {
		if charged, _ := chargeRun(t, naiveRetry(3, spoof)); charged != 1 {
			t.Errorf("spoofed safety=%v: charged %d times, want 1", spoof, charged)
		}
	}
}

// A retry-safe tool is not held to one invocation: retrying it is the middleware's call.
func TestToolReinvoke_RetrySafeToolMayRetry(t *testing.T) {
	var calls int
	flaky := Func("lookup", "look up", Safety{Idempotent: true}, func(context.Context, struct{}) (string, error) {
		if calls++; calls < 3 {
			return "", errors.New("flaky")
		}
		return "found", nil
	})
	store := NewMemStore()
	m := NewScriptedModel(ToolTurn("c1", "lookup", `{}`), TextTurn("done"))
	if _, err := New(m, store, flaky).UseTool(naiveRetry(3, false)).Run(context.Background(), "r1", "q"); err != nil {
		t.Fatal(err)
	}
	if rec, _ := hasStep(t, store, "r1", ToolResultStep("c1")); calls != 3 || rec.IsError || !strings.Contains(string(rec.Result), "found") {
		t.Fatalf("calls = %d, result %s (is_error=%v); want 3 calls ending in found", calls, rec.Result, rec.IsError)
	}
}

// Tool middleware sees the registered tool's Safety.
func TestToolSafety_ReportsTheTool(t *testing.T) {
	var got Safety
	var ok bool
	peek := func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, tu ToolUse) (json.RawMessage, error) {
			got, ok = ToolSafety(ctx)
			return next(ctx, tu)
		}
	}
	lookup := Func("lookup", "look up", Safety{ReadOnly: true}, func(context.Context, struct{}) (string, error) { return "x", nil })
	m := NewScriptedModel(ToolTurn("c1", "lookup", `{}`), TextTurn("done"))
	if _, err := New(m, NewMemStore(), lookup).UseTool(peek).Run(context.Background(), "r1", "q"); err != nil {
		t.Fatal(err)
	}
	if !ok || !got.ReadOnly {
		t.Fatalf("ToolSafety = %+v, %v; want ReadOnly, true", got, ok)
	}
	if _, ok := ToolSafety(context.Background()); ok {
		t.Fatal("ToolSafety outside a tool call reported ok")
	}
}
