package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// delegator calls a sub-agent tool once per id in one turn, in parallel, then answers. The tool
// for ids[i] is names[i], or "sub" when names is shorter.
type delegator struct {
	ids   []string
	names []string
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
			name := "sub"
			if i < len(m.names) {
				name = m.names[i]
			}
			ch <- Emit{Event: ToolCallDelta{Index: i, ID: id, Name: name, ArgsFragment: []byte(`{"task":"go"}`)}}
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
		sub := scope[:len(scope)-len(subRunSep+"s1")]
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

// truncating answers with tool-call arguments cut off, a call that fails for good, and reports
// usage u for it: billed, but with no response to record.
type truncating struct{ u Usage }

func (m truncating) Stream(context.Context, Request) (*Stream, error) {
	ch := make(chan Emit, 2)
	ch <- Emit{Event: ToolCallDelta{Index: 0, ID: "t1", Name: "step", ArgsFragment: []byte(`{"q":`)}}
	ch <- Emit{Event: Finish{Reason: "max_tokens", Usage: m.u}}
	close(ch)
	return NewStream(ch), nil
}

// A sub-agent's model call that failed for good was billed: the parent's Result.Spend and budget
// count it, though the sub-agent failed.
func TestTokenBudget_SubAgentFailedCallCounts(t *testing.T) {
	sub := New(truncating{u: hundred}, NewMemStore())
	parentModel := &delegator{ids: []string{"p1"}, u: hundred}
	res, err := New(parentModel, sub.store, SubAgent("sub", "delegate", sub)).RunResult(context.Background(), "r1", "q")
	if err != nil {
		t.Fatal(err)
	}
	if want := times(hundred, 2); res.Usage != want {
		t.Fatalf("Usage = %+v, want the parent's two answers %+v", res.Usage, want)
	}
	if want := times(hundred, 3); res.Spend != want {
		t.Fatalf("Spend = %+v, want %+v with the sub-agent's failed call", res.Spend, want)
	}

	parentModel = &delegator{ids: []string{"p1"}, u: hundred}
	_, err = New(parentModel, sub.store, SubAgent("sub", "delegate", sub)).WithTokenBudget(200).Run(context.Background(), "r2", "q")
	if !errors.Is(err, ErrBudgetExceeded) || parentModel.calls.Load() != 1 {
		t.Fatalf("err = %v after %d parent calls; want ErrBudgetExceeded after 1 (100 + the failed 100)", err, parentModel.calls.Load())
	}
}

// A sub-saga that aborts is part of its parent's usage too: the parent's failure record carries
// what the sub-saga used, also when the sub-saga aborted in an earlier attempt that the parent
// never recorded, so the parent re-enters an aborted sub-saga.
func TestSaga_FailureRecordCarriesSubAgentUsage(t *testing.T) {
	for _, earlier := range []bool{false, true} {
		store := NewMemStore()
		fail := stepTool(func(context.Context) error { return errors.New("step failed") })
		sub := New(&stepper{n: 1, u: hundred}, store, fail)
		if earlier {
			var aborted *SagaAborted
			id := SubRunID("r1", "p1")
			if _, err := sub.RunSaga(withRunScope(context.Background(), id), id, "go"); !errors.As(err, &aborted) {
				t.Fatalf("earlier attempt: err = %v, want *SagaAborted", err)
			}
		}
		parent := New(&delegator{ids: []string{"p1"}, u: hundred}, store, SubAgent("sub", "delegate", sub))
		var aborted *SagaAborted
		if _, err := parent.RunSaga(context.Background(), "r1", "q"); !errors.As(err, &aborted) {
			t.Fatalf("earlier=%v: err = %v, want *SagaAborted", earlier, err)
		}
		recs, err := store.History(context.Background(), "r1")
		if err != nil {
			t.Fatal(err)
		}
		var got Usage
		for _, r := range recs {
			if r.Kind == StepSagaFail && r.Usage != nil {
				got = *r.Usage
			}
		}
		if got != hundred {
			t.Fatalf("earlier=%v: the failure record carries %+v, want the sub-saga's %+v", earlier, got, hundred)
		}
	}
}

// Preloading reaches every level: a resumed tree counts a sub-agent's unfinished sub-agent too.
// Sub-agent A (two calls) and grandchild G (two calls, under sub-agent B's one) were cut off:
// with the parent's call, 600 used, over a budget of 450. A runs first on resume and must not
// call the model, though the tree it can see without G's journal has used only 400.
func TestTokenBudget_ResumedTreeCountsEveryLevel(t *testing.T) {
	store := NewMemStore()
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	seen := map[string]int{}
	var both sync.WaitGroup
	both.Add(2)
	cut := stepTool(func(ctx context.Context) error {
		scope := RunScope(ctx)
		run := scope[:strings.LastIndex(scope, subRunSep)]
		mu.Lock()
		seen[run]++
		second := seen[run] == 2
		mu.Unlock()
		if second {
			both.Done()
			both.Wait()
			cancel()
			return ctx.Err()
		}
		return nil
	})
	build := func(step Tool, models *[3]*stepper) *Agent {
		models[0], models[1] = &stepper{n: 3, u: hundred}, &stepper{n: 3, u: hundred}
		g := New(models[1], store, step)
		b := New(&delegator{ids: []string{"g1"}, u: hundred}, store, SubAgent("sub", "delegate", g))
		return New(&delegator{ids: []string{"p1", "p2"}, names: []string{"a", "b"}, u: hundred}, store,
			SubAgent("a", "delegate", New(models[0], store, step)), SubAgent("b", "delegate", b))
	}
	var first [3]*stepper
	if _, err := build(cut, &first).Run(ctx, "r1", "q"); !errors.Is(err, context.Canceled) {
		t.Fatalf("first invocation: err = %v, want context.Canceled", err)
	}
	if a, g := first[0].calls.Load(), first[1].calls.Load(); a != 2 || g != 2 {
		t.Fatalf("before the crash: A made %d calls, G %d; want 2 each", a, g)
	}
	var again [3]*stepper
	root := build(stepTool(func(context.Context) error { return nil }), &again).WithTokenBudget(450).SetMaxConcurrency(1)
	if _, err := root.Run(context.Background(), "r1", "q"); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("resumed: err = %v, want ErrBudgetExceeded", err)
	}
	if a, g := again[0].calls.Load(), again[1].calls.Load(); a != 0 || g != 0 {
		t.Fatalf("resumed tree called the model: A %d, G %d; want none (600 of 450 used)", a, g)
	}
}

// crashTree runs parent's run "r1" once with ctx, which a step tool cancels, and fails the test
// unless the run stops on that cancellation.
func crashTree(t *testing.T, ctx context.Context, parent *Agent) {
	t.Helper()
	if _, err := parent.Run(ctx, "r1", "q"); !errors.Is(err, context.Canceled) {
		t.Fatalf("first invocation: err = %v, want context.Canceled", err)
	}
}

// A resumed tree counts what it used before the crash once. Two sub-agents cut off after two
// calls each (500 used with the parent's call) each need two more calls, and the parent one:
// 1000 in all. A budget of 901 lets the last call through, since 900 were used before it; counting
// the sub-agents' journals twice would refuse it.
func TestTokenBudget_ResumedTreeCountsOnce(t *testing.T) {
	store := NewMemStore()
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	seen := map[string]int{}
	var both sync.WaitGroup
	both.Add(2)
	cut := stepTool(func(ctx context.Context) error {
		scope := RunScope(ctx)
		run := scope[:strings.LastIndex(scope, subRunSep)]
		mu.Lock()
		seen[run]++
		second := seen[run] == 2
		mu.Unlock()
		if second {
			both.Done()
			both.Wait()
			cancel()
			return ctx.Err()
		}
		return nil
	})
	crashTree(t, ctx, New(&delegator{ids: []string{"p1", "p2"}, u: hundred}, store,
		SubAgent("sub", "delegate", New(&stepper{n: 3, u: hundred}, store, cut))))

	sub := New(&stepper{n: 3, u: hundred}, store, stepTool(func(context.Context) error { return nil }))
	res, err := New(&delegator{ids: []string{"p1", "p2"}, u: hundred}, store, SubAgent("sub", "delegate", sub)).
		WithTokenBudget(901).SetMaxConcurrency(1).RunResult(context.Background(), "r1", "q")
	if err != nil {
		t.Fatalf("resumed: %v", err)
	}
	if want := times(hundred, 10); res.Spend != want {
		t.Fatalf("Spend = %+v, want %+v", res.Spend, want)
	}
}

// delegateThenStep calls the sub-agent tool "sub", then the tool "step", then answers.
type delegateThenStep struct{ u Usage }

func (m delegateThenStep) Stream(_ context.Context, req Request) (*Stream, error) {
	results := 0
	for _, msg := range req.Messages {
		if msg.Role == RoleTool {
			results++
		}
	}
	ch := make(chan Emit, 2)
	switch results {
	case 0:
		ch <- Emit{Event: ToolCallDelta{Index: 0, ID: "p1", Name: "sub", ArgsFragment: []byte(`{"task":"go"}`)}}
	case 1:
		ch <- Emit{Event: ToolCallDelta{Index: 0, ID: "p2", Name: "step", ArgsFragment: []byte(`{}`)}}
	default:
		ch <- Emit{Event: TextDelta{Text: "ok"}}
	}
	ch <- Emit{Event: Finish{Reason: "stop", Usage: m.u}}
	close(ch)
	return NewStream(ch), nil
}

// A sub-agent that finished before the crash is counted once, through its call's record. The
// parent (two calls) and its finished sub-agent (two calls) used 400 before the process died in
// the parent's next tool call; a budget of 401 lets the parent's last call through.
func TestTokenBudget_ResumedTreeCountsFinishedSubAgentOnce(t *testing.T) {
	store := NewMemStore()
	ctx, cancel := context.WithCancel(context.Background())
	sub := New(&stepper{n: 1, u: hundred}, store, stepTool(func(context.Context) error { return nil }))
	crashTree(t, ctx, New(delegateThenStep{u: hundred}, store, SubAgent("sub", "delegate", sub),
		stepTool(func(ctx context.Context) error { cancel(); return ctx.Err() })))

	res, err := New(delegateThenStep{u: hundred}, store, SubAgent("sub", "delegate", sub),
		stepTool(func(context.Context) error { return nil })).WithTokenBudget(401).RunResult(context.Background(), "r1", "q")
	if err != nil {
		t.Fatalf("resumed: %v", err)
	}
	if want := times(hundred, 5); res.Spend != want {
		t.Fatalf("Spend = %+v, want %+v", res.Spend, want)
	}
}

// A sub-agent's own budget holds for its subtree on resume too. It was cut off after two calls
// (200); with a budget of 250 it makes one more call, then stops.
func TestTokenBudget_ResumedSubAgentKeepsItsBudget(t *testing.T) {
	store := NewMemStore()
	ctx, cancel := context.WithCancel(context.Background())
	steps := 0
	cut := stepTool(func(ctx context.Context) error {
		if steps++; steps == 2 {
			cancel()
			return ctx.Err()
		}
		return nil
	})
	crashTree(t, ctx, New(&delegator{ids: []string{"p1"}, u: hundred}, store,
		SubAgent("sub", "delegate", New(&stepper{n: 5, u: hundred}, store, cut))))

	subModel := &stepper{n: 5, u: hundred}
	sub := New(subModel, store, stepTool(func(context.Context) error { return nil })).WithTokenBudget(250)
	if _, err := New(&delegator{ids: []string{"p1"}, u: hundred}, store, SubAgent("sub", "delegate", sub)).Run(context.Background(), "r1", "q"); err != nil {
		t.Fatalf("resumed: %v (the sub-agent's budget failure is its tool call's error, not the parent's)", err)
	}
	if n := subModel.calls.Load(); n != 1 {
		t.Fatalf("the resumed sub-agent made %d model calls, want 1 (200 of 250 used before it)", n)
	}
}
