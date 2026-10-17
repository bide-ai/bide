package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordTool captures the args it was actually called with, and counts executions.
type recordTool struct {
	name    string
	safety  Safety
	calls   *int
	lastArg *string
}

// Spec describes the tool to the agent (see Tool).
func (t *recordTool) Spec() ToolSpec {
	return ToolSpec{Name: t.name, Description: "", Input: json.RawMessage(`{"type":"object"}`), Safety: t.safety}
}

func (t *recordTool) Call(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
	*t.calls++
	if t.lastArg != nil {
		*t.lastArg = string(args)
	}
	return json.RawMessage(`{"ran":true}`), nil
}

// Tool middleware runs outermost-first, can mutate outgoing args, and can transform the
// result the model sees.
func TestToolMiddleware_OrderMutateTransform(t *testing.T) {
	var calls int
	var seenArgs string
	tool := &recordTool{name: "act", safety: Safety{ReadOnly: true}, calls: &calls, lastArg: &seenArgs}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "act", `{"q":"orig"}`), textTurn("done")}}

	var order []string
	mw := func(tag string) ToolMiddleware {
		return func(next ToolHandler) ToolHandler {
			return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
				order = append(order, tag)
				return next(ctx, call)
			}
		}
	}
	// Inner middleware rewrites the args; another transforms the result.
	rewrite := func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			call.Use.Args = json.RawMessage(`{"q":"rewritten"}`)
			res, err := next(ctx, call)
			if err != nil {
				return res, err
			}
			return json.RawMessage(strings.Replace(string(res), "true", "false", 1)), nil
		}
	}

	a := mustNew(m, memJournal(), WithTools(tool), WithToolMiddleware(mw("outer"), mw("inner"), rewrite))
	res2, err := a.Run(context.Background(), "r", UserText("go"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := res2.Message
	if got := textOf(out); got != "done" {
		t.Fatalf("answer = %q", got)
	}
	if calls != 1 {
		t.Fatalf("tool ran %d times, want 1", calls)
	}
	if want := []string{"outer", "inner"}; strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", order, want)
	}
	if seenArgs != `{"q":"rewritten"}` {
		t.Fatalf("tool saw args %q, want rewritten", seenArgs)
	}
}

// A short-circuiting middleware returns a result WITHOUT calling the tool, and that
// result is what the run records and the model sees.
func TestToolMiddleware_ShortCircuits(t *testing.T) {
	var calls int
	tool := &recordTool{name: "act", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "act", `{}`), textTurn("done")}}

	deny := func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			return json.RawMessage(`"cached"`), nil // never calls next
		}
	}
	a := mustNew(m, memJournal(), WithTools(tool), WithToolMiddleware(deny))
	if _, err := a.Run(context.Background(), "r", UserText("go")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != 0 {
		t.Fatalf("tool ran %d times, want 0 (short-circuited)", calls)
	}
}

// The middleware chain runs INSIDE the durable step: a short-circuit result is
// journaled, so on resume it replays without re-running the middleware or the tool.
func TestToolMiddleware_ResultIsJournaled(t *testing.T) {
	store := memJournal()
	var calls, mwHits int
	tool := &recordTool{name: "act", safety: Safety{ReadOnly: true}, calls: &calls}

	counting := func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			mwHits++
			return next(ctx, call)
		}
	}

	// First run: model asks for the tool, then crashes before answering.
	crashy := &scriptModel{turns: [][]Emit{toolTurn("c1", "act", `{}`), errTurn(errCrash)}}
	if _, err := mustNew(crashy, store, WithTools(tool), WithToolMiddleware(counting)).Run(context.Background(), "r", UserText("go")); err == nil {
		t.Fatal("expected crash on first attempt")
	}
	if calls != 1 || mwHits != 1 {
		t.Fatalf("after first run: calls=%d mwHits=%d, want 1,1", calls, mwHits)
	}

	// Resume with a healthy model. The journaled tool result replays — neither the tool
	// nor the middleware runs again.
	recovered := &scriptModel{turns: [][]Emit{textTurn("done")}}
	res, err := mustNew(recovered, store, WithTools(tool), WithToolMiddleware(counting)).Run(context.Background(), "r", UserText("go"))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	out := res.Message
	if got := textOf(out); got != "done" {
		t.Fatalf("answer = %q", got)
	}
	if calls != 1 {
		t.Fatalf("tool ran %d times across crash+resume, want 1", calls)
	}
	if mwHits != 1 {
		t.Fatalf("middleware ran %d times across crash+resume, want 1 (journaled result replays)", mwHits)
	}
}

func deltas(evs ...Event) *Stream {
	ch := make(chan Emit, len(evs)+1)
	for _, e := range evs {
		ch <- Emit{Event: e}
	}
	ch <- Emit{Event: Finish{Reason: FinishToolUse}}
	close(ch)
	return NewStream(ch)
}

// A tool call's ID and name are set once, by the fragment that starts it. A later fragment for the
// same index that names a different ID or tool is a second call arriving under the first one's
// index (a block started twice, or a server that omits the index), not more of the first: merged,
// one call would be dropped or run with the other's arguments.
func TestStream_ToolCallIdentityIsSetOnce(t *testing.T) {
	for name, s := range map[string]*Stream{
		"new id": deltas(
			ToolCallDelta{Index: 0, ID: "a", Name: "refund"},
			ToolCallDelta{Index: 0, ID: "b", Name: "refund", ArgsFragment: []byte(`{"amount":5}`)}),
		"new name": deltas(
			ToolCallDelta{Index: 0, ID: "a", Name: "refund"},
			ToolCallDelta{Index: 0, Name: "charge", ArgsFragment: []byte(`{}`)}),
	} {
		msg, _, err := s.Message()
		if !errors.Is(err, ErrStreamProtocol) {
			t.Errorf("%s: got %+v, %v; want ErrStreamProtocol", name, msg.toolUses(), err)
		}
	}
}

// Fragments that repeat the call's own ID and name, or carry neither, continue it.
func TestStream_ToolCallFragmentsContinueTheCall(t *testing.T) {
	msg, _, err := deltas(
		ToolCallDelta{Index: 0, ID: "a", Name: "refund", ArgsFragment: []byte(`{"amount":`)},
		ToolCallDelta{Index: 0, ID: "a", Name: "refund", ArgsFragment: []byte(`5`)},
		ToolCallDelta{Index: 0, ArgsFragment: []byte(`}`)}).Message()
	if err != nil {
		t.Fatal(err)
	}
	if uses := msg.toolUses(); len(uses) != 1 || uses[0].ID != "a" || string(uses[0].Args) != `{"amount":5}` {
		t.Fatalf("calls = %+v", uses)
	}
}

// The error names the first fragment that broke the framing.
func TestStream_ToolCallIdentityErrorNamesTheFirst(t *testing.T) {
	_, _, err := deltas(
		ToolCallDelta{Index: 0, ID: "a", Name: "refund"},
		ToolCallDelta{Index: 0, ID: "first-intruder"},
		ToolCallDelta{Index: 0, ID: "second-intruder"}).Message()
	if !errors.Is(err, ErrStreamProtocol) || !strings.Contains(err.Error(), "first-intruder") {
		t.Fatalf("err = %v, want ErrStreamProtocol naming the first intruder", err)
	}
}

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
			tool := MustFunc("t", "", func(context.Context, struct{}) (string, error) { calls.Add(1); return "ran", nil }, WithSafety(tc.safety))
			store := memJournal()
			m := NewScriptedModel(ToolTurn("c1", "t", `{}`), TextTurn("done"))
			_, err := mustNew(m, store, WithTools(tool), WithToolMiddleware(tc.mw)).Run(context.Background(), "r1", UserText("go"))
			rec, recorded := hasStep(t, store, "r1", ToolResultStep("c1"))
			if calls.Load() != 0 {
				t.Fatalf("the tool ran %d times; the middleware never called next", calls.Load())
			}
			if tc.wantHalt {
				if !errors.Is(err, ErrToolOutcomeUnknown) || recorded {
					t.Fatalf("Run = %v, recorded %v; want ErrToolOutcomeUnknown and nothing recorded", err, recorded)
				}
				_, err = mustNew(m, store, WithTools(tool)).Run(context.Background(), "r1", UserText("go"))
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
	charge := MustFunc("charge", "", func(context.Context, struct{}) (string, error) { calls.Add(1); return "ok", nil })
	rename := func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			call.Use.ID = "other" // the base handler refuses a re-identified call
			if _, err := next(ctx, call); err != nil {
				return nil, errors.New("wrapped away") // the sentinel is dropped
			}
			return nil, nil
		}
	}
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	if _, err := mustNew(m, store, WithTools(charge), WithToolMiddleware(rename)).Run(context.Background(), "r1", UserText("go")); err != nil {
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
	charge := MustFunc("charge", "", func(context.Context, struct{}) (string, error) { calls.Add(1); return "ok", nil })
	ctx, cancel := context.WithCancel(context.Background())
	cancelFirst := func(next ToolHandler) ToolHandler {
		return func(c context.Context, call ToolCall) (json.RawMessage, error) {
			cancel() // the run is cancelled, then the call goes on: the base handler refuses it
			<-c.Done()
			return next(c, call)
		}
	}
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	if _, err := mustNew(m, store, WithTools(charge), WithToolMiddleware(cancelFirst)).Run(ctx, "r1", UserText("go")); !errors.Is(err, context.Canceled) {
		t.Fatalf("first drive: %v, want context.Canceled", err)
	}
	if _, err := mustNew(m, store, WithTools(charge)).Run(context.Background(), "r1", UserText("go")); err != nil || calls.Load() != 1 {
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
	charge := MustFunc("charge", "", func(context.Context, struct{}) (string, error) { calls.Add(1); return "ok", nil })
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
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	if _, err := mustNew(m, store, WithTools(charge), WithToolMiddleware(tryExpiredFirst)).Run(context.Background(), "r1", UserText("go")); err != nil {
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
	hold := MustCompensatedFunc("hold", "", func(context.Context, chargeArgs) (string, error) { calls.Add(1); return "held", nil },
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
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "hold", `{"amount":5}`), TextTurn("done"))
	if _, err := mustNew(m, store, WithTools(hold), WithToolMiddleware(leak)).Run(context.Background(), "s1", UserText("go"), WithSaga()); err == nil {
		t.Fatal("saga Run: want the saga to abort on the deferred call")
	}
	close(release)
	if err := <-leaked; !errors.Is(err, ErrToolNotCalled) {
		t.Fatalf("leaked next = %v, want a refusal", err)
	}
	if _, ok := hasStep(t, store, "s1", sagaArgsStep("c1")); ok || calls.Load() != 0 {
		t.Fatalf("the leaked next journaled the accepted arguments (%v) or ran the tool (%d times)", ok, calls.Load())
	}
}

// NextOnceKey numbers a tool call's operations, restarts the numbering when the call runs
// again, gives every step its own numbering, and keeps steps with look-alike names apart.
func TestNextOnceKey(t *testing.T) {
	ctx := context.Background()
	if k := NextOnceKey(ctx); k != "" {
		t.Fatalf("outside a tool call NextOnceKey = %q, want empty", k)
	}
	call := func() []string {
		cctx := withOnceScope(ctx, "r/c1")
		keys := []string{NextOnceKey(cctx), NextOnceKey(cctx)}
		d := memJournal()
		for _, st := range [][2]string{{"a:1", "b"}, {"a", "1:b"}} {
			k, err := d.Step(cctx, st[0], st[1], func(sctx context.Context) (string, error) { return NextOnceKey(sctx), nil })
			if err != nil {
				t.Fatal(err)
			}
			keys = append(keys, k)
		}
		return keys
	}
	first, again := call(), call()
	seen := map[string]bool{}
	for i, k := range first {
		if k == "" || seen[k] {
			t.Fatalf("keys of one call are empty or repeat: %q", first)
		}
		seen[k] = true
		if again[i] != k {
			t.Fatalf("the call running again got key %q for operation %d, first run %q", again[i], i, k)
		}
	}
}

// R117-6. The tool's Timeout bounds its middleware too, and the late rule does not ask whether
// the tool was ever called. A middleware that waits for a slot (as middleware.ToolRateLimit does)
// and gives up at the deadline makes a side-effect call that certainly never ran look like one
// that may have fired: the run fails with ErrToolOutcomeUnknown and every resume halts with
// *OutcomeUnknown until a human resolves a call that never happened. The base handler knows
// whether it reached t.Call.
func TestR117_MiddlewareTimeoutBeforeTheToolRunsHaltsAsIfItMayHaveFired(t *testing.T) {
	var calls atomic.Int32
	charge := MustFunc("charge", "", func(context.Context, struct{}) (string, error) {
		calls.Add(1)
		return "charged", nil
	}, WithTimeout(time.Millisecond))
	waitForSlot := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			<-ctx.Done()                                                            // the limiter has no slot before the deadline
			return nil, fmt.Errorf("no slot: %w (%w)", ctx.Err(), ErrToolNotCalled) // next was never called
		}
	})
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	_, err := mustNew(m, store, WithTools(charge), WithToolMiddleware(waitForSlot)).Run(context.Background(), "r1", UserText("pay"))
	t.Logf("first drive: %v", err)
	_, err = mustNew(m, store, WithTools(charge)).Run(context.Background(), "r1", UserText("pay")) // resume, limiter gone
	var halt *OutcomeUnknown
	if calls.Load() == 0 && errors.As(err, &halt) {
		t.Fatalf("the tool was never called, yet the resume halts for its outcome: %v", err)
	}
}

// ADV117b-1 (R117-6). A tool middleware that runs next in a goroutine and returns when its own
// context is done (an "abandon on cancel" / hedging wrapper) lets the loop read reached=false,
// while the goroutine goes on to reach the tool's Call afterwards. The claim is then recorded as
// not started, and a resume calls the side effect again. Before the fix, called was set
// unconditionally before the chain ran, so the same run halted for its outcome.
//
// The gate only fixes the interleaving for determinism: without it the same window exists
// whenever next has not yet reached the base handler when the middleware gives up.
func TestAdv117b_AbandoningMiddlewareDoubleFiresASideEffect(t *testing.T) {
	var charges atomic.Int32
	charge := MustFunc("charge", "", func(context.Context, struct{}) (string, error) {
		charges.Add(1)
		return "charged", nil
	})
	gate := make(chan struct{})
	var late sync.WaitGroup // the abandoned request
	ctx, cancel := context.WithCancel(context.Background())
	abandon := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			type out struct {
				res json.RawMessage
				err error
			}
			done := make(chan out, 1)
			late.Add(1)
			go func() {
				defer late.Done()
				<-gate
				r, e := next(context.WithoutCancel(ctx), call) // finish the request even if the caller leaves
				done <- out{r, e}
			}()
			cancel() // the run is cancelled while the request is queued
			select {
			case o := <-done:
				return o.res, o.err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	})
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	if _, err := mustNew(m, store, WithTools(charge), WithToolMiddleware(abandon)).Run(ctx, "r1", UserText("pay")); !errors.Is(err, context.Canceled) {
		t.Fatalf("first drive: %v, want context.Canceled", err)
	}
	close(gate) // the queued request goes out after the drive returned
	late.Wait()
	// The chain had returned, so the call was closed: the late request is refused and never reaches
	// the tool. The chain's error did not say ErrToolNotCalled, so the side effect's outcome is
	// unknown to the loop, and the resume halts for it rather than fire it.
	_, err := mustNew(m, store, WithTools(charge)).Run(context.Background(), "r1", UserText("pay"))
	var halt *OutcomeUnknown
	if charges.Load() > 1 {
		t.Fatalf("the side effect fired %d times (resume err %v); the claim was recorded as not started although the chain went on to call the tool", charges.Load(), err)
	}
	if charges.Load() != 0 || !errors.As(err, &halt) {
		t.Fatalf("charges %d, resume err %v; want the late request refused and the resume halted", charges.Load(), err)
	}
}

// ADV117b-2 (R117-6). The reached flag is set only by the base handler, so a middleware that
// calls the tool itself (holding a reference to it) instead of next leaves reached false. If the
// tool then errors after the run was cancelled, the claim is recorded as not started and a resume
// fires the effect again. Before the fix this halted. Whether a middleware may do this is a
// contract question: ToolMiddleware's docs do not forbid it.
func TestAdv117b_MiddlewareCallingTheToolDirectlyDoubleFires(t *testing.T) {
	var charges atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	charge := MustFunc("charge", "", func(context.Context, struct{}) (string, error) {
		charges.Add(1)
		cancel() // the run is cancelled while the request is in flight
		return "", errors.New("connection reset")
	})
	direct := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			if call.Use.Name == "charge" {
				return charge.Call(ctx, call.Use.Args)
			}
			return next(ctx, call)
		}
	})
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	_, _ = mustNew(m, store, WithTools(charge), WithToolMiddleware(direct)).Run(ctx, "r1", UserText("pay"))
	_, err := mustNew(m, store, WithTools(charge)).Run(context.Background(), "r1", UserText("pay"))
	if charges.Load() > 1 {
		t.Fatalf("the side effect fired %d times (resume err %v)", charges.Load(), err)
	}
	var halt *OutcomeUnknown
	if !errors.As(err, &halt) {
		t.Fatalf("resume err %v, want *OutcomeUnknown: the call may have reached the tool", err)
	}
}

// outerWrap is a plain wrapper with no Compensate of its own.
type outerWrap struct{ Tool }

func (w outerWrap) Spec() ToolSpec { return w.Tool.Spec() }
func (w outerWrap) Unwrap() Tool   { return w.Tool }

// ADV117b-3 ((d)). checkWrapper looked only at the outermost tool: a Compensator one level down
// the Unwrap chain was accepted, and a rollback that recurses into the sub-run never calls it.
// New refuses it.
func TestAdv117b_NestedCompensatorWrapperIsAccepted(t *testing.T) {
	sub := mustNew(NewScriptedModel(TextTurn("x")), memJournal())
	tool := outerWrap{compWrap{MustSubAgent("delegate", "", sub)}}
	var calls atomic.Int32
	if a, err := New(&countingModel{n: &calls}, memJournal(), WithTools(tool)); !errors.Is(err, ErrConfig) || a != nil {
		t.Fatalf("New = %v, %v; want nil and ErrConfig for a Compensator inside the Unwrap chain", a, err)
	}
}

// A call the base handler REFUSED is not terminal once the chain returns: the loop only moves an
// OPEN call to closed, and enterTool moves refused to reached. So a middleware that got a refusal,
// left next running, and returned, reaches the tool after the loop has recorded the call as a
// known failure (called=false). The documented contract says the agent refuses any invocation of
// next that comes after the chain returned.
func TestAdv117c_RefusedThenLeakedNextReachesTool(t *testing.T) {
	var calls atomic.Int32
	charge := MustFunc("charge", "", func(context.Context, struct{}) (string, error) { calls.Add(1); return "ok", nil })
	release := make(chan struct{})
	leaked := make(chan error, 1)
	mw := func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			bad := call
			bad.Use.ID = "other"
			_, err := next(ctx, bad) // refused before the ran map: state open -> refused
			go func() {
				<-release
				_, e := next(context.WithoutCancel(ctx), call) // after the chain returned
				leaked <- e
			}()
			return nil, err
		}
	}
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	if _, err := mustNew(m, store, WithTools(charge), WithToolMiddleware(mw)).Run(context.Background(), "r1", UserText("go")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	rec, ok := hasStep(t, store, "r1", ToolResultStep("c1"))
	if !ok || !rec.IsError {
		t.Fatalf("setup: want the call recorded as a known failure, got %s (recorded %v)", rec.Result, ok)
	}
	close(release)
	e := <-leaked
	if n := calls.Load(); n != 0 {
		t.Fatalf("the journal records call c1 as a known failure (%s), yet a next invoked after the chain returned reached the tool %d time(s) (leaked next err: %v)", rec.Result, n, e)
	}
	if !errors.Is(e, ErrToolNotCalled) {
		t.Fatalf("leaked next = %v, want a refusal wrapping ErrToolNotCalled", e)
	}
}

// The same hole with a run cancellation: the refused call is recorded as NEVER STARTED, the leaked
// next fires the side effect, and a resume fires it again.
func TestAdv117c_RefusedThenLeakedNextDoubleFiresAcrossResume(t *testing.T) {
	var calls atomic.Int32
	charge := MustFunc("charge", "", func(context.Context, struct{}) (string, error) { calls.Add(1); return "ok", nil })
	release := make(chan struct{})
	leaked := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	mw := func(next ToolHandler) ToolHandler {
		return func(c context.Context, call ToolCall) (json.RawMessage, error) {
			bad := call
			bad.Use.ID = "other"
			_, err := next(c, bad) // refused: state refused
			go func() {
				<-release
				_, e := next(context.WithoutCancel(c), call)
				leaked <- e
			}()
			cancel()
			<-c.Done()
			return nil, err
		}
	}
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	_, err1 := mustNew(m, store, WithTools(charge), WithToolMiddleware(mw)).Run(ctx, "r1", UserText("go"))
	if _, recorded := hasStep(t, store, "r1", ToolResultStep("c1")); recorded || calls.Load() != 0 {
		t.Fatalf("setup: first drive %v; want no result recorded and no call yet", err1)
	}
	close(release)
	e := <-leaked
	if _, err := mustNew(m, store, WithTools(charge)).Run(context.Background(), "r1", UserText("go")); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if n := calls.Load(); n > 1 {
		t.Fatalf("side-effect tool charge fired %d times for one call (first drive: %v; the leaked next after the chain returned: err %v; then the resume, since the claim was recorded never started)", n, err1, e)
	}
}

// Angles that hold: reached beats a sentinel added by a middleware or returned by the tool
// itself, and a sub-agent whose inner call was not called does not make the outer call "not
// called". Each case cancels the run inside the call, so a wrong "not called" verdict would
// record the claim never started and the resume would fire the side effect a second time.
func TestAdv117c_ReachedBeatsSentinel(t *testing.T) {
	type setup struct {
		mw    ToolMiddleware
		inner bool // the tool is a sub-agent whose own tool is refused
	}
	cases := map[string]setup{
		"middleware wraps a reached error": {mw: func(next ToolHandler) ToolHandler {
			return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
				_, err := next(ctx, call)
				return nil, fmt.Errorf("%w (%w)", err, ErrToolNotCalled)
			}
		}},
		"tool's own error wraps the sentinel": {},
		"sub-agent inner not-called":          {inner: true},
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			ctx, cancel := context.WithCancel(context.Background())
			var tool Tool
			if s.inner {
				innerTool := MustFunc("inner", "", func(context.Context, struct{}) (string, error) {
					calls.Add(1) // the sub-run's side effect
					cancel()
					return "", fmt.Errorf("declined (%w)", ErrToolNotCalled)
				})
				subStore := memJournal()
				sub := mustNew(
					NewScriptedModel(ToolTurn("i1", "inner", `{}`), TextTurn("done")),
					subStore,
					WithTools(innerTool),
				)
				tool = MustSubAgent("charge", "", sub)
			} else {
				tool = MustFunc("charge", "", func(context.Context, struct{}) (string, error) {
					calls.Add(1)
					cancel()
					return "", fmt.Errorf("declined (%w)", ErrToolNotCalled)
				})
			}
			store := memJournal()
			args := `{}`
			if s.inner {
				args = `{"task":"x"}`
			}
			m := NewScriptedModel(ToolTurn("c1", "charge", args), TextTurn("done"))
			a := mustNew(m, store, WithTools(tool))
			if s.mw != nil {
				a = must(a.With(WithToolMiddleware(s.mw)))
			}
			_, err1 := a.Run(ctx, "r1", UserText("go"))
			_, err2 := mustNew(m, store, WithTools(tool)).Run(context.Background(), "r1", UserText("go"))
			var halt *OutcomeUnknown
			if calls.Load() != 1 || !errors.As(err2, &halt) {
				t.Fatalf("calls %d; first %v; resume %v", calls.Load(), err1, err2)
			}
			t.Logf("calls %d; first %v; resume %v", calls.Load(), err1, err2)
		})
	}
}

// R117-1, the run's own deadline: an error a side effect returns once the run's deadline has
// passed, before the context's timer has fired, is the run's cancellation, not a failure: nothing
// is recorded. GOMAXPROCS(1) keeps the timer from firing while the tool busy-waits.
func TestR117_ErrorAfterTheRunsDeadlineBeforeItsTimerIsNotRecorded(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	charge := MustFunc("charge", "", func(ctx context.Context, _ struct{}) (string, error) {
		dl, _ := ctx.Deadline()
		for time.Now().Before(dl) {
		}
		return "", errors.New("gateway: client timeout awaiting response")
	})
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := mustNew(m, store, WithTools(charge)).Run(ctx, "r1", UserText("pay"))
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
			lookup := MustFunc("lookup", "", func(context.Context, struct{}) (string, error) { looked.Add(1); return "x", nil }, WithSafety(Safety{ReadOnly: true}))
			wire := MustFunc("wire", "", func(context.Context, struct{}) (string, error) { wired.Add(1); return "sent", nil })
			rewrite := ToolMiddleware(func(next ToolHandler) ToolHandler {
				return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
					change(&call)
					return next(ctx, call)
				}
			})
			store := memJournal()
			m := NewScriptedModel(ToolTurn("c1", "lookup", `{}`), TextTurn("done"))
			_, _ = mustNew(m, store, WithTools(lookup, wire), WithToolMiddleware(rewrite)).Run(context.Background(), "r1", UserText("q"))
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

func (w timedWrap) Spec() ToolSpec { s := w.Tool.Spec(); s.Timeout = time.Second; return s }
func (w timedWrap) Unwrap() Tool   { return w.Tool }

// compWrap is a wrapper that unwraps and is also a Compensator: rollback would take it for a
// sub-agent and never call its Compensate.
type compWrap struct{ Tool }

func (w compWrap) Spec() ToolSpec { return w.Tool.Spec() }
func (w compWrap) Unwrap() Tool   { return w.Tool }
func (w compWrap) Compensate(context.Context, json.RawMessage, json.RawMessage) error {
	return nil
}

// Suspicions (b) and (d): New refuses a wrapper with a timeout over a sub-agent, and a wrapper
// that unwraps and is also a Compensator, each with ErrConfig before any model call.
func TestR117_NewRefusesUnsafeWrappers(t *testing.T) {
	sub := mustNew(NewScriptedModel(TextTurn("x")), memJournal())
	for name, tool := range map[string]Tool{
		"timeout over a sub-agent":      timedWrap{MustSubAgent("delegate", "", sub)},
		"compensator that unwraps":      compWrap{MustSubAgent("delegate", "", sub)},
		"compensator unwrapping a tool": compWrap{MustFunc("f", "", func(context.Context, struct{}) (string, error) { return "", nil })},
	} {
		var calls atomic.Int32
		if a, err := New(&countingModel{n: &calls}, memJournal(), WithTools(tool)); !errors.Is(err, ErrConfig) || a != nil || calls.Load() != 0 {
			t.Errorf("%s: New = %v, %v after %d model calls; want nil and ErrConfig before any", name, a, err, calls.Load())
		}
	}
}

// R117-6, the other half: a middleware that waits out the deadline and then calls next does not
// start the tool: the base handler refuses a call whose context is already done, and the call
// fails as a known timeout, recorded, with no halt on resume.
func TestR117_BaseHandlerDoesNotStartACallPastItsDeadline(t *testing.T) {
	var calls atomic.Int32
	charge := MustFunc("charge", "", func(context.Context, struct{}) (string, error) {
		calls.Add(1)
		return "charged", nil
	}, WithTimeout(time.Millisecond))
	slow := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			<-ctx.Done()
			return next(ctx, call)
		}
	})
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	if _, err := mustNew(m, store, WithTools(charge), WithToolMiddleware(slow)).Run(context.Background(), "r1", UserText("pay")); err != nil {
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
	charge := MustFunc("charge", "", func(context.Context, struct{}) (string, error) {
		calls.Add(1)
		return "charged", nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	waiting := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			cancel() // the run is cancelled while the limiter waits
			<-ctx.Done()
			return nil, fmt.Errorf("no slot: %w (%w)", ctx.Err(), ErrToolNotCalled)
		}
	})
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	if _, err := mustNew(m, store, WithTools(charge), WithToolMiddleware(waiting)).Run(ctx, "r1", UserText("pay")); !errors.Is(err, context.Canceled) {
		t.Fatalf("first drive: %v, want context.Canceled", err)
	}
	if _, err := mustNew(m, store, WithTools(charge)).Run(context.Background(), "r1", UserText("pay")); err != nil {
		t.Fatalf("resume: %v, want the call attempted again", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("charged %d times, want 1", calls.Load())
	}
}
