package agent

import (
	"context"
	"encoding/json"
	"errors"
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
