package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

var hundred = Usage{InputTokens: 80, OutputTokens: 20} // 100 tokens per model call

// stepper calls "step" until its conversation holds n tool results, then answers "done". Every
// call reports usage u, and calls counts them.
type stepper struct {
	n     int
	u     Usage
	calls atomic.Int32
}

func (m *stepper) Stream(_ context.Context, req Request) (*Stream, error) {
	m.calls.Add(1)
	results := 0
	for _, msg := range req.Messages {
		if msg.Role == RoleTool {
			results++
		}
	}
	ch := make(chan Emit, 2)
	if results >= m.n {
		ch <- Emit{Event: TextDelta{Text: "done"}}
		ch <- Emit{Event: Finish{Reason: "stop", Usage: m.u}}
	} else {
		ch <- Emit{Event: ToolCallDelta{Index: 0, ID: fmt.Sprintf("s%d", results), Name: "step", ArgsFragment: []byte(`{}`)}}
		ch <- Emit{Event: Finish{Reason: "tool_use", Usage: m.u}}
	}
	close(ch)
	return NewStream(ch), nil
}

// delegator calls the sub-agent tool "sub" once per id in one turn, in parallel, then answers.
type delegator struct {
	ids   []string
	u     Usage
	calls atomic.Int32
}

func (m *delegator) Stream(_ context.Context, req Request) (*Stream, error) {
	m.calls.Add(1)
	ch := make(chan Emit, len(m.ids)+1)
	if req.Messages[len(req.Messages)-1].Role == RoleTool {
		ch <- Emit{Event: TextDelta{Text: "ok"}}
		ch <- Emit{Event: Finish{Reason: "stop", Usage: m.u}}
	} else {
		for i, id := range m.ids {
			ch <- Emit{Event: ToolCallDelta{Index: i, ID: id, Name: "sub", ArgsFragment: []byte(`{"task":"go"}`)}}
		}
		ch <- Emit{Event: Finish{Reason: "tool_use", Usage: m.u}}
	}
	close(ch)
	return NewStream(ch), nil
}

func stepTool(fn func(ctx context.Context) error) Tool {
	return Func("step", "one step", Safety{ReadOnly: true}, func(ctx context.Context, _ struct{}) (string, error) {
		if err := fn(ctx); err != nil {
			return "", err
		}
		return "stepped", nil
	})
}

func times(u Usage, n int) Usage {
	return Usage{InputTokens: u.InputTokens * n, OutputTokens: u.OutputTokens * n, CacheReadTokens: u.CacheReadTokens * n, CacheWriteTokens: u.CacheWriteTokens * n}
}

// Result.Usage and Result.Spend are the whole run's, however many invocations it took: a run
// cut off after its first model call and resumed reports both calls, and re-entering it once
// finished reports the same again.
func TestRunResult_WholeRunAcrossResume(t *testing.T) {
	store := NewMemStore()
	ctx, cancel := context.WithCancel(context.Background())
	cut := stepTool(func(ctx context.Context) error { cancel(); return ctx.Err() })
	if _, err := New(&stepper{n: 1, u: hundred}, store, cut).RunResult(ctx, "r1", "q"); !errors.Is(err, context.Canceled) {
		t.Fatalf("first invocation: err = %v, want context.Canceled", err)
	}
	a := New(&stepper{n: 1, u: hundred}, store, stepTool(func(context.Context) error { return nil }))
	for _, pass := range []string{"resumed", "re-entered"} {
		res, err := a.RunResult(context.Background(), "r1", "q")
		if err != nil {
			t.Fatalf("%s: %v", pass, err)
		}
		if want := times(hundred, 2); res.Usage != want || res.Spend != want {
			t.Fatalf("%s: Usage = %+v, Spend = %+v; want both %+v (the run's two model calls)", pass, res.Usage, res.Spend, want)
		}
	}
}

// A sub-agent's model calls are part of its parent's run: the parent's Result counts them.
func TestRunResult_IncludesSubAgents(t *testing.T) {
	store := NewMemStore()
	sub := New(&stepper{n: 1, u: hundred}, store, stepTool(func(context.Context) error { return nil }))
	parent := New(&delegator{ids: []string{"p1", "p2"}, u: hundred}, store, SubAgent("sub", "delegate", sub))
	res, err := parent.RunResult(context.Background(), "r1", "q")
	if err != nil {
		t.Fatal(err)
	}
	// The parent's two calls and each sub-agent's two.
	if want := times(hundred, 6); res.Usage != want || res.Spend != want {
		t.Fatalf("Usage = %+v, Spend = %+v; want both %+v", res.Usage, res.Spend, want)
	}
}

// The parent's token budget covers its sub-agents: a sub-agent stops calling the model once the
// tree has used the budget, and so does the parent.
func TestTokenBudget_CoversSubAgents(t *testing.T) {
	store := NewMemStore()
	subModel := &stepper{n: 10, u: hundred}
	sub := New(subModel, store, stepTool(func(context.Context) error { return nil }))
	parentModel := &delegator{ids: []string{"p1"}, u: hundred}
	parent := New(parentModel, store, SubAgent("sub", "delegate", sub)).WithTokenBudget(300)
	_, err := parent.Run(context.Background(), "r1", "q")
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	// Parent 100, then the sub-agent 100 and 100: 300 used, so the sub-agent's third call and the
	// parent's second are refused.
	if p, s := parentModel.calls.Load(), subModel.calls.Load(); p != 1 || s != 2 {
		t.Fatalf("model calls: parent %d, sub-agent %d; want 1 and 2", p, s)
	}
}

// Parallel sub-agents share the budget. Each stops before a call once the tree has used it, so
// the tree can pass the budget only by calls already in flight: at most one per agent running.
func TestTokenBudget_ParallelSubAgentsBound(t *testing.T) {
	const budget, subs = 1000, 4
	for range 20 {
		store := NewMemStore()
		subModel := &stepper{n: 100, u: hundred}
		sub := New(subModel, store, stepTool(func(context.Context) error { return nil }))
		ids := make([]string, subs)
		for i := range ids {
			ids[i] = fmt.Sprintf("p%d", i)
		}
		parentModel := &delegator{ids: ids, u: hundred}
		_, err := New(parentModel, store, SubAgent("sub", "delegate", sub)).WithTokenBudget(budget).Run(context.Background(), "r1", "q")
		if !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("err = %v, want ErrBudgetExceeded", err)
		}
		used := int(parentModel.calls.Load()+subModel.calls.Load()) * hundred.TotalTokens()
		if used < budget || used > budget-1+subs*hundred.TotalTokens() {
			t.Fatalf("the tree used %d tokens; want at least the budget %d and at most %d (one in-flight call per sub-agent past it)", used, budget, budget-1+subs*hundred.TotalTokens())
		}
	}
}

// A resumed tree is held to what all of it has already used, including sub-agents cut off
// mid-run, before any of them calls the model again. Two sub-agents each made two calls (200
// tokens) and the parent one (100) before the process died: 500 used, over a budget of 450, so
// the resumed tree makes no model call at all, even running its sub-agents one at a time.
func TestTokenBudget_ResumedTreeCountsUnfinishedSubAgents(t *testing.T) {
	store := NewMemStore()
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	seen := map[string]int{}
	var both sync.WaitGroup
	both.Add(2)
	cut := stepTool(func(ctx context.Context) error {
		scope := RunScope(ctx)
		sub := scope[:len(scope)-len("/s1")]
		mu.Lock()
		seen[sub]++
		second := seen[sub] == 2
		mu.Unlock()
		if second { // this sub-agent's second step: both have made two model calls, then die
			both.Done()
			both.Wait()
			cancel()
			return ctx.Err()
		}
		return nil
	})
	subModel := &stepper{n: 3, u: hundred}
	sub := New(subModel, store, cut)
	parentModel := &delegator{ids: []string{"p1", "p2"}, u: hundred}
	if _, err := New(parentModel, store, SubAgent("sub", "delegate", sub)).Run(ctx, "r1", "q"); !errors.Is(err, context.Canceled) {
		t.Fatalf("first invocation: err = %v, want context.Canceled", err)
	}
	if p, s := parentModel.calls.Load(), subModel.calls.Load(); p != 1 || s != 4 {
		t.Fatalf("before the crash: parent %d calls, sub-agents %d; want 1 and 4", p, s)
	}

	subModel2 := &stepper{n: 3, u: hundred}
	sub2 := New(subModel2, store, stepTool(func(context.Context) error { return nil }))
	parentModel2 := &delegator{ids: []string{"p1", "p2"}, u: hundred}
	parent2 := New(parentModel2, store, SubAgent("sub", "delegate", sub2)).WithTokenBudget(450).SetMaxConcurrency(1)
	if _, err := parent2.Run(context.Background(), "r1", "q"); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("resumed: err = %v, want ErrBudgetExceeded", err)
	}
	if p, s := parentModel2.calls.Load(), subModel2.calls.Load(); p != 0 || s != 0 {
		t.Fatalf("resumed tree called the model: parent %d, sub-agents %d; want none (500 of 450 used)", p, s)
	}
}
