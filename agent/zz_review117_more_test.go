package agent

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// R117-1, the run's own deadline: an error a side effect returns once the run's deadline has
// passed, before the context's timer has fired, is the run's cancellation, not a failure: nothing
// is recorded. GOMAXPROCS(1) keeps the timer from firing while the tool busy-waits.
func TestR117_ErrorAfterTheRunsDeadlineBeforeItsTimerIsNotRecorded(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	charge := Func("charge", "", Safety{}, func(ctx context.Context, _ struct{}) (string, error) {
		dl, _ := ctx.Deadline()
		for time.Now().Before(dl) {
		}
		return "", errors.New("gateway: client timeout awaiting response")
	})
	store := NewMemStore()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := New(m, store, charge).Run(ctx, "r1", "pay")
	if rec, ok := hasStep(t, store, "r1", ToolResultStep("c1")); ok {
		t.Fatalf("an error after the run's deadline was recorded as a failure (%s); Run err = %v", rec.Result, err)
	}
}

// Suspicion (a): a middleware that renames a call, or changes its ID, is refused with ErrConfig;
// the base handler never dispatches to another tool, or under another call's ID.
func TestR117_MiddlewareCannotRenameOrReIDACall(t *testing.T) {
	for name, change := range map[string]func(*ToolCall){
		"name": func(c *ToolCall) { c.Use.Name = "wire" },
		"id":   func(c *ToolCall) { c.Use.ID = "c9" },
	} {
		t.Run(name, func(t *testing.T) {
			var looked, wired atomic.Int32
			lookup := Func("lookup", "", Safety{ReadOnly: true}, func(context.Context, struct{}) (string, error) { looked.Add(1); return "x", nil })
			wire := Func("wire", "", Safety{}, func(context.Context, struct{}) (string, error) { wired.Add(1); return "sent", nil })
			rewrite := ToolMiddleware(func(next ToolHandler) ToolHandler {
				return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
					change(&call)
					return next(ctx, call)
				}
			})
			store := NewMemStore()
			m := NewScriptedModel(ToolTurn("c1", "lookup", `{}`), TextTurn("done"))
			_, _ = New(m, store, lookup, wire).UseTool(rewrite).Run(context.Background(), "r1", "q")
			rec, ok := hasStep(t, store, "r1", ToolResultStep("c1"))
			if wired.Load() != 0 || looked.Load() != 0 || !ok || !rec.IsError {
				t.Fatalf("lookup ran %d, wire ran %d, result %s (recorded %v); want neither run and the call failed", looked.Load(), wired.Load(), rec.Result, ok)
			}
		})
	}
}

// timedWrap is a custom wrapper that gives a wrapped sub-agent a timeout, which SubAgent itself
// refuses.
type timedWrap struct{ Tool }

func (w timedWrap) Spec() ToolSpec { s := SpecOf(w.Tool); s.Timeout = time.Second; return s }
func (w timedWrap) Unwrap() Tool   { return w.Tool }

// compWrap is a wrapper that unwraps and is also a Compensator: rollback would take it for a
// sub-agent and never call its Compensate.
type compWrap struct{ Tool }

func (w compWrap) Spec() ToolSpec { return SpecOf(w.Tool) }
func (w compWrap) Unwrap() Tool   { return w.Tool }
func (w compWrap) Compensate(context.Context, json.RawMessage, json.RawMessage) error {
	return nil
}

// Suspicions (b) and (d): New refuses a wrapper with a timeout over a sub-agent, and a wrapper
// that unwraps and is also a Compensator, each with ErrConfig before any model call.
func TestR117_NewRefusesUnsafeWrappers(t *testing.T) {
	sub := New(NewScriptedModel(TextTurn("x")), NewMemStore())
	for name, tool := range map[string]Tool{
		"timeout over a sub-agent":      timedWrap{SubAgent("delegate", "", sub)},
		"compensator that unwraps":      compWrap{SubAgent("delegate", "", sub)},
		"compensator unwrapping a tool": compWrap{Func("f", "", Safety{}, func(context.Context, struct{}) (string, error) { return "", nil })},
	} {
		var calls atomic.Int32
		_, err := New(&countingModel{n: &calls}, NewMemStore(), tool).Run(context.Background(), "r1", "go")
		if !errors.Is(err, ErrConfig) || calls.Load() != 0 {
			t.Errorf("%s: Run = %v after %d model calls; want ErrConfig before any", name, err, calls.Load())
		}
	}
}

// R117-6, the other half: a middleware that waits out the deadline and then calls next does not
// start the tool: the base handler refuses a call whose context is already done, and the call
// fails as a known timeout, recorded, with no halt on resume.
func TestR117_BaseHandlerDoesNotStartACallPastItsDeadline(t *testing.T) {
	var calls atomic.Int32
	charge := Func("charge", "", Safety{}, func(context.Context, struct{}) (string, error) {
		calls.Add(1)
		return "charged", nil
	}, WithTimeout(time.Millisecond))
	slow := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			<-ctx.Done()
			return next(ctx, call)
		}
	})
	store := NewMemStore()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	if _, err := New(m, store, charge).UseTool(slow).Run(context.Background(), "r1", "pay"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	rec, ok := hasStep(t, store, "r1", ToolResultStep("c1"))
	if calls.Load() != 0 || !ok || !rec.IsError || !strings.Contains(string(rec.Result), "not started") {
		t.Fatalf("calls %d, result %s (recorded %v); want the tool never called and the call failed as not started", calls.Load(), rec.Result, ok)
	}
}

// R117-6 with the run's own cancellation: a side effect whose middleware was still waiting when
// the run was cancelled never ran, so its claim is recorded as never started and a resume calls
// the tool instead of halting.
func TestR117_RunCancelledInMiddlewareRecordsNotStarted(t *testing.T) {
	var calls atomic.Int32
	charge := Func("charge", "", Safety{}, func(context.Context, struct{}) (string, error) {
		calls.Add(1)
		return "charged", nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	waiting := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			cancel() // the run is cancelled while the limiter waits
			<-ctx.Done()
			return nil, ctx.Err()
		}
	})
	store := NewMemStore()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	if _, err := New(m, store, charge).UseTool(waiting).Run(ctx, "r1", "pay"); !errors.Is(err, context.Canceled) {
		t.Fatalf("first drive: %v, want context.Canceled", err)
	}
	if _, err := New(m, store, charge).Run(context.Background(), "r1", "pay"); err != nil {
		t.Fatalf("resume: %v, want the call attempted again", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("charged %d times, want 1", calls.Load())
	}
}
