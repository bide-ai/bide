package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
)

// The call states, end to end: what the loop records when the middleware chain returns without
// the tool being reached (closed), by the error it returns, for a side effect and a retry-safe tool.
func TestCallState_ClosedChain(t *testing.T) {
	deny := func(err error) ToolMiddleware {
		return func(next ToolHandler) ToolHandler {
			return func(context.Context, ToolCall) (json.RawMessage, error) { return nil, err }
		}
	}
	cacheHit := func(next ToolHandler) ToolHandler {
		return func(context.Context, ToolCall) (json.RawMessage, error) { return json.RawMessage(`"cached"`), nil }
	}
	for _, tc := range []struct {
		name      string
		safety    Safety
		mw        ToolMiddleware
		wantHalt  bool   // the drive fails with ErrToolOutcomeUnknown and a resume halts
		wantError bool   // the call is recorded as a failure
		want      string // the recorded result, when one is recorded
	}{
		{"denial says not called, side effect", Safety{}, deny(fmt.Errorf("policy: denied (%w)", ErrToolNotCalled)), false, true, ""},
		{"denial without the sentinel, side effect", Safety{}, deny(errors.New("policy: denied")), true, false, ""},
		{"denial without the sentinel, retry-safe", Safety{Idempotent: true}, deny(errors.New("policy: denied")), false, true, ""},
		{"cache hit, side effect", Safety{}, cacheHit, false, false, `"cached"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			tool := Func("t", "", tc.safety, func(context.Context, struct{}) (string, error) { calls.Add(1); return "ran", nil })
			store := NewMemStore()
			m := NewScriptedModel(ToolTurn("c1", "t", `{}`), TextTurn("done"))
			_, err := New(m, store, tool).UseTool(tc.mw).Run(context.Background(), "r1", "go")
			rec, recorded := hasStep(t, store, "r1", ToolResultStep("c1"))
			if calls.Load() != 0 {
				t.Fatalf("the tool ran %d times; the middleware never called next", calls.Load())
			}
			if tc.wantHalt {
				if !errors.Is(err, ErrToolOutcomeUnknown) || recorded {
					t.Fatalf("Run = %v, recorded %v; want ErrToolOutcomeUnknown and nothing recorded", err, recorded)
				}
				_, err = New(m, store, tool).Run(context.Background(), "r1", "go")
				var halt *OutcomeUnknown
				if !errors.As(err, &halt) {
					t.Fatalf("resume = %v, want *OutcomeUnknown", err)
				}
				return
			}
			if err != nil || !recorded || rec.IsError != tc.wantError || tc.want != "" && string(rec.Result) != tc.want {
				t.Fatalf("Run = %v, result %s (recorded %v, is_error %v)", err, rec.Result, recorded, rec.IsError)
			}
		})
	}
}

// enterTool moves an open or refused call to reached, and never a closed one; a reached call
// stays reached.
func TestCallState_EnterTool(t *testing.T) {
	for _, tc := range []struct {
		from int32
		ok   bool
		to   int32
	}{
		{callOpen, true, callReached},
		{callRefused, true, callReached},
		{callReached, true, callReached},
		{callClosed, false, callClosed},
		{callRefusedClosed, false, callRefusedClosed},
	} {
		var st atomic.Int32
		st.Store(tc.from)
		if ok := enterTool(&st); ok != tc.ok || st.Load() != tc.to {
			t.Errorf("from %d: enterTool = %v, state %d; want %v, %d", tc.from, ok, st.Load(), tc.ok, tc.to)
		}
	}
}

// A call the base handler refused is known not to have run, whatever error the middleware then
// returns: a middleware that replaces the error (dropping ErrToolNotCalled) still gets a known
// failure, not a halt, because the call's state says refused.
func TestCallState_RefusedSurvivesAReplacedError(t *testing.T) {
	var calls atomic.Int32
	charge := Func("charge", "", Safety{}, func(context.Context, struct{}) (string, error) { calls.Add(1); return "ok", nil })
	rename := func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			call.Use.ID = "other" // the base handler refuses a re-identified call
			if _, err := next(ctx, call); err != nil {
				return nil, errors.New("wrapped away") // the sentinel is dropped
			}
			return nil, nil
		}
	}
	store := NewMemStore()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	if _, err := New(m, store, charge).UseTool(rename).Run(context.Background(), "r1", "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rec, ok := hasStep(t, store, "r1", ToolResultStep("c1")); calls.Load() != 0 || !ok || !rec.IsError {
		t.Fatalf("calls %d, result %s (recorded %v); want the refused call recorded as a known failure", calls.Load(), rec.Result, ok)
	}
}

// A call the base handler refused because the run was cancelled first is recorded as never
// started, so a resume calls the tool instead of halting for it.
func TestCallState_RefusedOnCancelRecordsNotStarted(t *testing.T) {
	var calls atomic.Int32
	charge := Func("charge", "", Safety{}, func(context.Context, struct{}) (string, error) { calls.Add(1); return "ok", nil })
	ctx, cancel := context.WithCancel(context.Background())
	cancelFirst := func(next ToolHandler) ToolHandler {
		return func(c context.Context, call ToolCall) (json.RawMessage, error) {
			cancel() // the run is cancelled, then the call goes on: the base handler refuses it
			<-c.Done()
			return next(c, call)
		}
	}
	store := NewMemStore()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	if _, err := New(m, store, charge).UseTool(cancelFirst).Run(ctx, "r1", "go"); !errors.Is(err, context.Canceled) {
		t.Fatalf("first drive: %v, want context.Canceled", err)
	}
	if _, err := New(m, store, charge).Run(context.Background(), "r1", "go"); err != nil || calls.Load() != 1 {
		t.Fatalf("resume: %v after %d calls; want the call attempted again, once", err, calls.Load())
	}
}

// closeCall closes an open call and a refused one, and leaves a reached call reached; every closed
// state is terminal: neither enterTool nor another closeCall leaves it.
func TestCallState_CloseCall(t *testing.T) {
	for _, tc := range []struct{ from, to int32 }{
		{callOpen, callClosed},
		{callRefused, callRefusedClosed},
		{callReached, callReached},
		{callClosed, callClosed},
		{callRefusedClosed, callRefusedClosed},
	} {
		var st atomic.Int32
		st.Store(tc.from)
		if got := closeCall(&st); got != tc.to {
			t.Errorf("from %d: closeCall = %d, want %d", tc.from, got, tc.to)
		}
		if tc.to != callReached && enterTool(&st) {
			t.Errorf("from %d: a closed call was entered", tc.from)
		}
	}
}

// A call is marked as run only immediately before its tool: an invocation of next that the base
// handler refused (its context already done) does not make a later invocation "already ran".
func TestCallState_RefusalDoesNotMarkTheCallRun(t *testing.T) {
	var calls atomic.Int32
	charge := Func("charge", "", Safety{}, func(context.Context, struct{}) (string, error) { calls.Add(1); return "ok", nil })
	tryExpiredFirst := func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			done, cancel := context.WithCancel(ctx)
			cancel()
			if _, err := next(done, call); !errors.Is(err, ErrToolNotCalled) { // refused: not started
				return nil, fmt.Errorf("first invocation: %v", err)
			}
			return next(ctx, call) // a second, live invocation runs the tool
		}
	}
	store := NewMemStore()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	if _, err := New(m, store, charge).UseTool(tryExpiredFirst).Run(context.Background(), "r1", "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rec, _ := hasStep(t, store, "r1", ToolResultStep("c1")); calls.Load() != 1 || rec.IsError {
		t.Fatalf("calls %d, result %s; want the live invocation to run the tool once", calls.Load(), rec.Result)
	}
}

// An invocation of next leaked past the end of the chain writes nothing: not the saga's record of
// the arguments the tool accepted, and not the tool's effect.
func TestCallState_LeakedNextWritesNoAcceptedArgs(t *testing.T) {
	var calls atomic.Int32
	hold := CompensatedFunc("hold", "", Safety{}, func(context.Context, chargeArgs) (string, error) { calls.Add(1); return "held", nil },
		func(context.Context, chargeArgs, string) error { return nil })
	release := make(chan struct{})
	leaked := make(chan error, 1)
	leak := func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			go func() {
				<-release
				call.Use.Args = json.RawMessage(`{"amount":999}`) // rewritten: would be journaled
				_, err := next(context.WithoutCancel(ctx), call)
				leaked <- err
			}()
			return nil, fmt.Errorf("deferred (%w)", ErrToolNotCalled)
		}
	}
	store := NewMemStore()
	m := NewScriptedModel(ToolTurn("c1", "hold", `{"amount":5}`), TextTurn("done"))
	if _, err := New(m, store, hold).UseTool(leak).RunSaga(context.Background(), "s1", "go"); err == nil {
		t.Fatal("RunSaga: want the saga to abort on the deferred call")
	}
	close(release)
	if err := <-leaked; !errors.Is(err, ErrToolNotCalled) {
		t.Fatalf("leaked next = %v, want a refusal", err)
	}
	if _, ok := hasStep(t, store, "s1", sagaArgsStep("c1")); ok || calls.Load() != 0 {
		t.Fatalf("the leaked next journaled the accepted arguments (%v) or ran the tool (%d times)", ok, calls.Load())
	}
}
