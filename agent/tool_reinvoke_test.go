package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// naiveRetry is a third-party retry middleware that ignores Safety: it calls next up to
// 1+n times until one succeeds. spoof, if set, re-labels every call as ReadOnly first.
func naiveRetry(n int, spoof bool) ToolMiddleware {
	return func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			var res json.RawMessage
			var err error
			for range 1 + n {
				if spoof {
					call.Spec.Safety = Safety{ReadOnly: true}
				}
				if res, err = next(ctx, call); err == nil {
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
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	if _, err := mustNew(m, store, WithTools(charge), WithToolMiddleware(mw)).Run(context.Background(), "r1", "pay"); err != nil {
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
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "lookup", `{}`), TextTurn("done"))
	if _, err := mustNew(m, store, WithTools(flaky), WithToolMiddleware(naiveRetry(3, false))).Run(context.Background(), "r1", "q"); err != nil {
		t.Fatal(err)
	}
	if rec, _ := hasStep(t, store, "r1", ToolResultStep("c1")); calls != 3 || rec.IsError || !strings.Contains(string(rec.Result), "found") {
		t.Fatalf("calls = %d, result %s (is_error=%v); want 3 calls ending in found", calls, rec.Result, rec.IsError)
	}
}

// Tool middleware sees the registered tool's spec, and the run the call belongs to.
func TestToolCall_CarriesTheSpec(t *testing.T) {
	var got ToolCall
	peek := func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			got = call
			return next(ctx, call)
		}
	}
	lookup := Func("lookup", "look up", Safety{ReadOnly: true}, func(context.Context, struct{}) (string, error) { return "x", nil }, WithTitle("Lookup"), WithTimeout(time.Minute))
	m := NewScriptedModel(ToolTurn("c1", "lookup", `{}`), TextTurn("done"))
	if _, err := mustNew(m, memJournal(), WithTools(lookup), WithToolMiddleware(peek)).Run(context.Background(), "r1", "q"); err != nil {
		t.Fatal(err)
	}
	if got.Use.ID != "c1" || got.RunID != "r1" || got.Spec.Name != "lookup" || !got.Spec.Safety.ReadOnly || got.Spec.Title != "Lookup" || got.Spec.Timeout != time.Minute {
		t.Fatalf("ToolCall = %+v; want call c1 of run r1 with lookup's spec", got)
	}
}
