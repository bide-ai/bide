package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/internal/toolhook"
)

// covWrapper wraps a tool, overriding its spec and, when set, binding the rollback context.
type covWrapper struct {
	inner Tool
	spec  func(ToolSpec) ToolSpec
	bind  func(context.Context, string) (context.Context, error)
	next  func() Tool // when set, Unwrap returns this instead of inner
}

func (w covWrapper) Spec() ToolSpec {
	s := w.inner.Spec()
	if w.spec != nil {
		s = w.spec(s)
	}
	return s
}
func (w covWrapper) Call(ctx context.Context, a json.RawMessage) (json.RawMessage, error) {
	return w.inner.Call(ctx, a)
}
func (w covWrapper) Unwrap() Tool {
	if w.next != nil {
		return w.next()
	}
	return w.inner
}

// covBinder is a covWrapper that is also a toolhook.RollbackBinder.
type covBinder struct{ covWrapper }

func (b covBinder) BindRollback(ctx context.Context, id string) (context.Context, error) {
	return b.bind(ctx, id)
}

// covCompensating is a covWrapper that is also a Compensator.
type covCompensating struct{ covWrapper }

func (covCompensating) Compensate(context.Context, json.RawMessage, json.RawMessage) error {
	return nil
}

func newSub(t *testing.T, turns ...[]Emit) Tool {
	t.Helper()
	return MustSubAgent("research", "delegate", mustNew(&scriptModel{turns: turns}, memJournal()))
}

// SubAgent refuses a nil agent and a bad tool option.
func TestSubAgent_Refusals(t *testing.T) {
	if _, err := SubAgent("x", "d", nil); !errors.Is(err, ErrConfig) {
		t.Errorf("nil agent = %v; want ErrConfig", err)
	}
	sub := mustNew(&scriptModel{}, memJournal())
	if _, err := SubAgent("x", "d", sub, nil); !errors.Is(err, ErrConfig) {
		t.Errorf("nil option = %v; want ErrConfig", err)
	}
}

// A wrapper over a sub-agent may not give it a Timeout or another Safety, and no wrapper on the
// Unwrap chain may be a Compensator, however deep: New refuses each with ErrConfig, so a saga
// rollback never compensates through a wrapper.
func TestNew_RefusesWrappersThatBreakTheContract(t *testing.T) {
	sub := newSub(t, textTurn("x"))
	withTimeout := covWrapper{inner: sub, spec: func(s ToolSpec) ToolSpec { s.Timeout = time.Second; return s }}
	withSafety := covWrapper{inner: sub, spec: func(s ToolSpec) ToolSpec { s.Safety = Safety{ReadOnly: true}; return s }}
	plain := covWrapper{inner: sub}
	for name, tool := range map[string]Tool{
		"timeout over a sub-agent":    withTimeout,
		"safety over a sub-agent":     withSafety,
		"compensating wrapper":        covCompensating{plain},
		"compensator under a wrapper": covWrapper{inner: covCompensating{plain}, spec: func(s ToolSpec) ToolSpec { s.Name = "outer"; return s }},
	} {
		if _, err := New(&scriptModel{}, memJournal(), WithTools(tool)); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: New = %v; want ErrConfig", name, err)
		}
	}
	// A wrapper that keeps the contract is accepted, and a rollback binder is no problem.
	if _, err := New(&scriptModel{}, memJournal(), WithTools(plain)); err != nil {
		t.Errorf("plain wrapper: New = %v", err)
	}
}

// A wrapper chain that never ends (a cycle) is ErrConfig, not an endless loop, and one that
// unwraps to nil ends the chain.
func TestWrapperChains_AreBounded(t *testing.T) {
	sub := newSub(t, textTurn("x"))
	var cyc covWrapper
	cyc = covWrapper{inner: sub, next: func() Tool { return cyc }}
	if _, err := New(&scriptModel{}, memJournal(), WithTools(cyc)); !errors.Is(err, ErrConfig) {
		t.Errorf("cyclic wrapper: New = %v; want ErrConfig", err)
	}
	if _, ok := asSubAgent(cyc); ok {
		t.Error("asSubAgent found a sub-agent through a cycle")
	}
	if a := subRunAgentFor(cyc, "x"); a != nil {
		t.Error("subRunAgentFor found an agent through a cycle")
	}
	if _, err := bindRollback(context.Background(), cyc, "r>c"); err != nil {
		t.Errorf("bindRollback through a cycle = %v; want the unbound context", err)
	}
	endsNil := covWrapper{inner: sub, next: func() Tool { return nil }}
	if _, ok := asSubAgent(endsNil); ok {
		t.Error("asSubAgent found a sub-agent behind a nil Unwrap")
	}
	if a := subRunAgentFor(endsNil, "x"); a != nil {
		t.Error("subRunAgentFor found an agent behind a nil Unwrap")
	}
	if _, err := bindRollback(context.Background(), endsNil, "r>c"); err != nil {
		t.Errorf("bindRollback behind a nil Unwrap = %v", err)
	}
	if s, ok := asSubAgent(covWrapper{inner: covWrapper{inner: sub}}); !ok || s == nil {
		t.Error("asSubAgent did not find a sub-agent under two wrappers")
	}
}

type covCtxKey struct{ n int }

// bindRollback binds the context with every binder on the chain, outermost first, and an error from
// one stops the walk: the delegation is not compensated under a context that was not bound.
func TestBindRollback_BindsEveryWrapperAndStopsOnError(t *testing.T) {
	sub := newSub(t, textTurn("x"))
	var order []int
	mk := func(n int, inner Tool, err error) Tool {
		return covBinder{covWrapper{inner: inner, bind: func(ctx context.Context, id string) (context.Context, error) {
			order = append(order, n)
			if id != "r>c1" {
				t.Errorf("binder %d got sub-run %q", n, id)
			}
			return context.WithValue(ctx, covCtxKey{n}, n), err
		}}}
	}
	chain := mk(1, mk(2, sub, nil), nil)
	ctx, err := bindRollback(context.Background(), chain, "r>c1")
	if err != nil || len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Fatalf("bindRollback = %v, order %v; want both binders, outermost first", err, order)
	}
	if ctx.Value(covCtxKey{1}) != 1 || ctx.Value(covCtxKey{2}) != 2 {
		t.Error("the returned context lacks a binder's value")
	}
	order = nil
	bad := mk(1, mk(2, sub, errors.New("grant gone")), nil)
	if ctx, err := bindRollback(context.Background(), bad, "r>c1"); err == nil || ctx != nil || len(order) != 2 {
		t.Fatalf("bindRollback with a failing binder = %v, %v, order %v; want an error and no context", ctx, err, order)
	}
	order = nil
	stopsEarly := mk(1, mk(2, sub, nil), errors.New("denied"))
	if _, err := bindRollback(context.Background(), stopsEarly, "r>c1"); err == nil || len(order) != 1 {
		t.Fatalf("outer binder failing = %v, order %v; want the walk to stop at it", err, order)
	}
}

// toolhook.CheckTool, the check plan shares, refuses what is not a Tool and what New refuses.
func TestToolhookCheckTool(t *testing.T) {
	if err := toolhook.CheckTool("not a tool"); !errors.Is(err, ErrConfig) {
		t.Errorf("a non-tool = %v; want ErrConfig", err)
	}
	sub := newSub(t, textTurn("x"))
	if err := toolhook.CheckTool(covCompensating{covWrapper{inner: sub}}); !errors.Is(err, ErrConfig) {
		t.Errorf("a compensating wrapper = %v; want ErrConfig", err)
	}
	if err := toolhook.CheckTool(sub); err != nil {
		t.Errorf("a sub-agent = %v", err)
	}
}

// Called outside a run, a sub-agent journals under a fixed fallback ID, so a second call answers
// from the first: the documented reason to drive sub-agents through Agent.Run.
func TestSubAgent_FallbackRunIDMemoizes(t *testing.T) {
	ctx := context.Background()
	m := &countModel{inner: &scriptModel{turns: [][]Emit{textTurn("first answer"), textTurn("second answer")}}}
	tool := MustSubAgent("research", "d", mustNew(m, memJournal()))
	for i := 0; i < 2; i++ {
		out, err := tool.Call(ctx, json.RawMessage(`{"task":"look"}`))
		if err != nil || string(out) != `"first answer"` {
			t.Fatalf("call %d = %s, %v; want the first answer both times", i, out, err)
		}
	}
	if n := m.calls.Load(); n != 1 {
		t.Fatalf("model called %d times; want 1", n)
	}
	if _, err := tool.Call(ctx, json.RawMessage(`{"task":`)); !errors.Is(err, ErrToolArgs) {
		t.Errorf("bad args = %v; want ErrToolArgs", err)
	}
}

type panicModel struct{}

func (panicModel) Stream(context.Context, Request) (*Stream, error) { panic("model exploded") }

// A sub-agent whose model panics is an ErrTool failure of the call, not a crashed process.
func TestSubAgent_PanicIsAToolError(t *testing.T) {
	tool := MustSubAgent("research", "d", mustNew(panicModel{}, memJournal()))
	_, err := tool.Call(context.Background(), json.RawMessage(`{"task":"look"}`))
	if !errors.Is(err, ErrTool) {
		t.Fatalf("Call = %v; want ErrTool", err)
	}
}

// clerkTree builds a root agent whose one tool is a sub-agent ("clerk", call s1) that calls
// inner (call c1) and then answers "sub done".
func clerkTree(store *Journal, inner Tool) *Agent {
	sub := mustNew(
		NewScriptedModel(ToolTurn("c1", inner.Spec().Name, `{}`), TextTurn("sub done")),
		store,
		WithTools(inner),
	)
	return mustNew(
		NewScriptedModel(ToolTurn("s1", "clerk", `{"task":"do it"}`), TextTurn("parent done")),
		store,
		WithTools(MustSubAgent("clerk", "does it", sub)),
	)
}

// A halt inside a sub-agent is resolved against the sub-run and continued from the root: the
// signal says which run to re-invoke, and doing so completes the whole tree.
func TestSubAgentHalt_ResolvedAndContinuedFromTheRoot(t *testing.T) {
	store := memJournal()
	ctx, cancel := context.WithCancel(context.Background())
	charge := MustFunc("charge", "charge", func(ctx context.Context, _ struct{}) (string, error) {
		cancel() // the charge went out; the run is cut off before it reports back
		return "", ctx.Err()
	})
	root := clerkTree(store, charge)
	_, _ = root.Run(ctx, "p", UserText("go"))
	_, err := root.Run(context.Background(), "p", UserText("go"))
	var halt *OutcomeUnknown
	if !errors.As(err, &halt) || halt.RunID != "p>s1" || halt.RootRunID != "p" {
		t.Fatalf("halt = %+v (%v); want RunID p>s1 and RootRunID p", halt, err)
	}
	if err := ResolveHalt(context.Background(), store, HaltRef{RunID: halt.RunID, Op: OpRef{Kind: OpTool, ID: halt.Op.ID}, Cause: HaltCrashed}, Outcome{Result: "charged", IsError: false}); err != nil {
		t.Fatal(err)
	}
	res, err := root.Run(context.Background(), halt.RootRunID, UserText("go"))
	var msg Message
	if res != nil {
		msg = res.Message
	}
	if err != nil || msg.Text() != "parent done" {
		t.Fatalf("continue from the root = %q, %v", msg.Text(), err)
	}
	if rec, ok := hasStep(t, store, "p", ToolResultStep("s1")); !ok || string(rec.Result) != `"sub done"` {
		t.Fatalf("the parent recorded %s for the sub-agent, want its own answer", rec.Result)
	}
}

// An Interrupt inside a sub-agent: answer it against the sub-run, continue from the root.
func TestSubAgentInterrupt_AnsweredAndContinuedFromTheRoot(t *testing.T) {
	store := memJournal()
	ask := MustFunc("ask", "ask", func(ctx context.Context, _ struct{}) (string, error) {
		return Interrupt[string](ctx, "confirm", "ok?")
	}, WithSafety(Safety{ReadOnly: true}))
	root := clerkTree(store, ask)
	_, err := root.Run(context.Background(), "p", UserText("go"))
	var intr *InterruptPending
	if !errors.As(err, &intr) || intr.RunID != "p>s1" || intr.RootRunID != "p" {
		t.Fatalf("interrupt = %+v (%v); want RunID p>s1 and RootRunID p", intr, err)
	}
	if err := store.AnswerInterrupt(context.Background(), intr.RunID, intr.Name, "yes"); err != nil {
		t.Fatal(err)
	}
	if msg, err := answerOf(root.Run(context.Background(), intr.RootRunID, UserText("go"))); err != nil || msg.Text() != "parent done" {
		t.Fatalf("continue from the root = %q, %v", msg.Text(), err)
	}
}

// A Sleep inside a sub-agent schedules a wake for the root run, which the waker's resume
// callback (the root agent) can drive; the woken run completes.
func TestSubAgentSleep_WakesTheRoot(t *testing.T) {
	store := memJournal()
	var fired bool
	nap := MustFunc("nap", "wait a moment", func(ctx context.Context, _ struct{}) (string, error) {
		if err := Sleep(ctx, "nap", 10*time.Millisecond); err != nil {
			return "", err
		}
		return "rested", nil
	}, WithSafety(Safety{ReadOnly: true}))
	root := clerkTree(store, nap)
	var woken []string
	w := NewMemWaker(func(ctx context.Context, runID string) error {
		woken = append(woken, runID)
		res, err := root.Run(ctx, runID, UserText("go"))
		var msg Message
		if res != nil {
			msg = res.Message
		}
		fired = err == nil && msg.Text() == "parent done"
		return err
	})
	_, err := root.Run(context.Background(), "p", UserText("go"), WithWaker(w))
	var slp *TimerPending
	if !errors.As(err, &slp) || slp.RootRunID != "p" {
		t.Fatalf("sleep = %+v (%v); want RootRunID p", slp, err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := w.Fire(contextWithWaker(context.Background(), w), time.Now()); err != nil {
		t.Fatalf("Fire: %v (woke %v)", err, woken)
	}
	if len(woken) != 1 || woken[0] != "p" || !fired {
		t.Fatalf("woke %v (completed=%v); want the root run p woken and completed", woken, fired)
	}
}

// wrappedSub is a tool that wraps a SubAgent (as audit.AttenuatingSubAgent does) and says so with
// Unwrap. Its Call adds nothing; what matters is that it is not the SubAgent tool itself.
type wrappedSub struct{ Tool }

func (w wrappedSub) Spec() ToolSpec { return w.Tool.Spec() }
func (w wrappedSub) Unwrap() Tool   { return w.Tool }
func (w wrappedSub) Call(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	return w.Tool.Call(ctx, args)
}

// A saga rollback recurses into a sub-agent's run through a tool that wraps the sub-agent: the
// sub-run's completed write is compensated, as it is for a plain SubAgent, rather than the
// delegation being reported as a write nothing can undo.
func TestSaga_RollbackRecursesThroughAWrappedSubAgent(t *testing.T) {
	l := newLedger()
	store := memJournal()
	sub := mustNew(
		&scriptModel{turns: [][]Emit{toolTurn("x1", "X", `{}`), textTurn("done")}},
		store,
		WithTools(l.write("X")),
	)
	parent := mustNew(
		&scriptModel{turns: [][]Emit{
			toolTurn("p1", "delegate", `{"task":"x"}`),
			toolTurn("p2", "boom", `{}`),
		}},
		store,
		WithTools(wrappedSub{MustSubAgent("delegate", "", sub)}, failTool("boom")),
	)

	_, err := parent.Run(context.Background(), "root", UserText("go"), WithSaga())
	var ab *SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("err = %v, want *SagaAborted", err)
	}
	if l.undoCount["X"] != 1 || len(ab.Uncompensated) != 0 {
		t.Fatalf("undo X = %d, uncompensated %v; want the sub-run's write undone once and nothing left", l.undoCount["X"], ab.Uncompensated)
	}
	l.assertClean(t)
}

// A wrapped sub-agent whose own saga failed rolled itself back, and the parent's rollback walks
// it again to report what it undid, so the tree's lists are whole, as for a plain SubAgent.
func TestSaga_RollbackReportsAWrappedSubAgentsOwnRollback(t *testing.T) {
	l := newLedger()
	store := memJournal()
	sub := mustNew(
		&scriptModel{turns: [][]Emit{toolTurn("x1", "X", `{}`), toolTurn("x2", "boom", `{}`)}},
		store,
		WithTools(l.write("X"), failTool("boom")),
	)
	parent := mustNew(
		&scriptModel{turns: [][]Emit{toolTurn("p1", "delegate", `{"task":"x"}`)}},
		store,
		WithTools(wrappedSub{MustSubAgent("delegate", "", sub)}),
	)

	_, err := parent.Run(context.Background(), "root", UserText("go"), WithSaga())
	var ab *SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("err = %v, want *SagaAborted", err)
	}
	if l.undoCount["X"] != 1 || len(ab.Compensated) != 1 || ab.Compensated[0] != "X" {
		t.Fatalf("undo X = %d, compensated %v; want X undone once and reported", l.undoCount["X"], ab.Compensated)
	}
	l.assertClean(t)
}

// B4: the rollback re-runs a retry-safe compensable call cut off by a sibling's failure. The re-run
// starts a programmatic sub-run (which books) and then reports its outcome unknown. rollbackRun
// refreshes `links` only on the re-run's success path, so on the unknown path the new link is
// never read: the child's completed, compensable book is neither compensated nor listed (only
// "charge" is listed as unknown). The live path walks a failed-unknown call's sub-runs.
func TestAdv127b_RerunUnknownSkipsItsSubRuns(t *testing.T) {
	store := memJournal()
	var undone, calls atomic.Int32
	book := MustCompensatedFunc("book", "", func(context.Context, struct{}) (string, error) { return "booked", nil },
		func(context.Context, struct{}, string) error { undone.Add(1); return nil })
	child, err := New(NewScriptedModel(ToolTurn("k1", "book", `{}`), TextTurn("child done")), store, WithTools(book))
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	charge := MustCompensatedFunc("charge", "", func(ctx context.Context, _ struct{}) (string, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-ctx.Done() // cut off by the sibling's failure
			return "", ctx.Err()
		}
		info, _ := RunInfoFrom(ctx)
		if _, err := child.Run(ctx, info.SubRunFor("child"), UserText("work"), WithSaga()); err != nil {
			return "", err
		}
		return "", fmt.Errorf("charge: gateway timeout after the booking: %w", ErrToolOutcomeUnknown)
	},
		func(context.Context, struct{}, string) error { return nil }, WithSafety(Safety{Idempotent: true}),
		WithSubRuns(func(string) *Agent { return child }))
	fail := MustFunc("fail", "", func(context.Context, struct{}) (string, error) {
		<-started
		return "", errors.New("declined")
	})
	_, err = mustNew(t4TwoCalls{}, store, WithTools(charge, fail)).Run(context.Background(), "r", UserText("go"), WithSaga())
	var ab *SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("saga Run = %v, want *SagaAborted", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("charge ran %d times, want 2 (live, then the rollback's re-run)", calls.Load())
	}
	if undone.Load() != 1 && !slices.Contains(ab.Uncompensated, "book") && !slices.Contains(ab.UnknownOutcome, "book") {
		t.Errorf("re-run's sub-run: book undone %d; compensated %v, uncompensated %v, unknown %v, err %v; book listed nowhere",
			undone.Load(), ab.Compensated, ab.Uncompensated, ab.UnknownOutcome, ab.CompensateErr)
	}
	// The rollback finished and marked the run terminal, so no later drive revisits it.
	if _, err := mustNew(t4TwoCalls{}, store, WithTools(charge, fail)).Run(context.Background(), "r", UserText("go"), WithSaga()); errors.As(err, &ab) && undone.Load() == 0 {
		t.Logf("second drive: compensated %v, unknown %v; book still not undone", ab.Compensated, ab.UnknownOutcome)
	}
}
