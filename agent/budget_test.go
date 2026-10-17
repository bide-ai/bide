package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// billed is the usage every scripted attempt in these tests reports: 120 tokens.
var billed = Usage{InputTokens: 100, OutputTokens: 20}

// truncatedTurn is an attempt cut off at max tokens: the provider billed it, but its tool-call
// arguments are incomplete, so the call fails with ErrTruncatedToolArgs.
func truncatedTurn(u Usage) []Emit {
	return []Emit{
		{Event: ToolCallDelta{Index: 0, ID: "cut", Name: "lookup", ArgsFragment: []byte(`{"q":`)}},
		{Event: Finish{Reason: "max_tokens", Usage: u}},
	}
}

func twice(u Usage) Usage {
	addUsage(&u, u)
	return u
}

// A turn whose first attempt failed and was retried used both attempts' tokens. The recorded
// turn keeps the answer's usage; the failed attempt's is reported as discarded spend, in
// Run and in the journal.
func TestResult_ReportsDiscardedSpend(t *testing.T) {
	m := &scriptModel{turns: [][]Emit{truncatedTurn(billed), textTurnWithUsage("done", billed)}}
	store := memJournal()
	res, err := mustNew(m, store, WithMiddleware(retryOnceMW)).Run(context.Background(), "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage != billed {
		t.Fatalf("Usage = %+v, want the answer's %+v", res.Usage, billed)
	}
	if res.Spend != twice(billed) {
		t.Fatalf("Spend = %+v, want both attempts' %+v", res.Spend, twice(billed))
	}
	recs, _ := store.History(context.Background(), "r")
	var found bool
	for _, r := range recs {
		if r.Kind == StepModel {
			found = true
			if r.Usage == nil || *r.Usage != billed {
				t.Fatalf("journaled Usage = %+v, want %+v", r.Usage, billed)
			}
			if r.DiscardedUsage == nil || *r.DiscardedUsage != billed {
				t.Fatalf("journaled DiscardedUsage = %+v, want %+v", r.DiscardedUsage, billed)
			}
		}
	}
	if !found {
		t.Fatal("no model step journaled")
	}
}

// Budget 200: the first turn took two billed 120-token attempts, 240 tokens, so the run must
// stop before calling the model again. Counting only the recorded answer (120) let it go on.
func TestTokenBudget_StopsOnDiscardedSpend(t *testing.T) {
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{
		truncatedTurn(billed),
		toolTurnWithUsage("c1", "lookup", `{}`, billed),
		textTurnWithUsage("done", billed),
	}}
	_, err := must(mustNew(m, memJournal(), WithTools(tool), WithMiddleware(retryOnceMW)).With(WithTokenBudget(200))).Run(context.Background(), "r", UserText("go"))
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	if m.i != 2 {
		t.Fatalf("the model was called %d times, want 2 (the first turn's two attempts)", m.i)
	}
}

// The discarded spend is journaled, so a resumed run's budget still counts it.
func TestTokenBudget_DiscardedSpendSurvivesResume(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	var calls int
	gated := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, approval: SingleApproval(), calls: &calls}
	m := &scriptModel{turns: [][]Emit{truncatedTurn(billed), toolTurnWithUsage("c1", "lookup", `{}`, billed)}}
	_, err := mustNew(m, store, WithTools(gated), WithMiddleware(retryOnceMW)).Run(ctx, "r", UserText("go"))
	var pa *ApprovalPending
	if !errors.As(err, &pa) {
		t.Fatalf("err = %v, want ApprovalPending", err)
	}
	if err := Approve(ctx, store, "r", "c1", true); err != nil {
		t.Fatal(err)
	}
	resumed := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
	_, err = mustNew(resumed, store, WithTools(gated), WithTokenBudget(200)).Run(ctx, "r", UserText("go"))
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded (240 tokens already spent)", err)
	}
	if resumed.i != 0 {
		t.Fatalf("the resumed run called the model %d times, want 0", resumed.i)
	}
}

// A model call that fails for good is journaled with its spend, so a caller that keeps
// re-invoking a failing run is still stopped by the budget.
func TestTokenBudget_CountsFailedCalls(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	m := &scriptModel{turns: [][]Emit{truncatedTurn(billed), truncatedTurn(billed), truncatedTurn(billed)}}
	a := mustNew(m, store, WithTokenBudget(200))
	for i := range 2 {
		if _, err := a.Run(ctx, "r", UserText("go")); !errors.Is(err, ErrModel) {
			t.Fatalf("run %d: err = %v, want ErrModel", i, err)
		}
	}
	// 240 tokens spent on two failed calls: the third invocation must not call the model.
	_, err := mustNew(m, store, WithTokenBudget(200)).Run(ctx, "r", UserText("go"))
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	if m.i != 2 {
		t.Fatalf("the model was called %d times, want 2", m.i)
	}
}

// A stream that fails after reporting its usage was billed for it: a Finish followed by another
// event breaks the stream protocol, and the usage the Finish carried still counts.
func TestResult_SpendOfBrokenStream(t *testing.T) {
	broken := append(textTurnWithUsage("draft", billed), Emit{Event: TextDelta{Text: "late"}})
	m := &scriptModel{turns: [][]Emit{broken, textTurnWithUsage("done", billed)}}
	res, err := mustNew(m, memJournal(), WithMiddleware(retryOnceMW)).Run(context.Background(), "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Spend != twice(billed) {
		t.Fatalf("Spend = %+v, want both attempts' %+v", res.Spend, twice(billed))
	}
}

// A response a middleware supplies without sending a request (a cache) reports usage no request
// spent; the turn's discarded spend is then zero, never negative.
func TestResult_SpendOfSuppliedResponse(t *testing.T) {
	cache := func(ModelHandler) ModelHandler {
		return func(context.Context, ModelCall) (ModelResponse, error) {
			return ModelResponse{Message: Message{Role: RoleAssistant, Parts: []Part{Text{Text: "cached"}}}, Usage: billed}, nil
		}
	}
	store := memJournal()
	res, err := mustNew(&scriptModel{}, store, WithMiddleware(cache)).Run(context.Background(), "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Spend != billed {
		t.Fatalf("Spend = %+v, want %+v", res.Spend, billed)
	}
	recs, _ := store.History(context.Background(), "r")
	for _, r := range recs {
		if r.DiscardedUsage != nil {
			t.Fatalf("journaled DiscardedUsage %+v for a supplied response", *r.DiscardedUsage)
		}
	}
}

// Each turn records only its own discarded spend: what one turn discarded is not counted again
// with the next.
func TestResult_SpendAcrossTurns(t *testing.T) {
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{
		truncatedTurn(billed),
		toolTurnWithUsage("c1", "lookup", `{}`, billed),
		textTurnWithUsage("done", billed),
	}}
	res, err := mustNew(m, memJournal(), WithTools(tool), WithMiddleware(retryOnceMW)).Run(context.Background(), "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	want := twice(billed)
	addUsage(&want, billed)
	if res.Spend != want {
		t.Fatalf("Spend = %+v, want three requests' %+v", res.Spend, want)
	}
}

// cancelAwareStore is a MemStore whose every method fails once its context is cancelled, as a
// database store's does.
type cancelAwareStore struct{ *MemStore }

func (s cancelAwareStore) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, false, err
	}
	return s.MemStore.Insert(ctx, runID, name, data)
}

func (s cancelAwareStore) Get(ctx context.Context, runID, name string) (Entry, bool, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, false, err
	}
	return s.MemStore.Get(ctx, runID, name)
}

func (s cancelAwareStore) Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error] {
	if err := ctx.Err(); err != nil {
		return func(yield func(Entry, error) bool) { yield(Entry{}, err) }
	}
	return s.MemStore.Load(ctx, runID, after)
}

// A model call that fails because the run was cancelled was still billed: its spend is journaled
// even though the run's context has ended.
func TestTokenBudget_JournalsSpendOfCancelledCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelling := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			resp, err := next(ctx, call) // billed, then the caller gives up
			cancel()
			if err == nil {
				err = context.Canceled
			}
			return resp, err
		}
	}
	store := mustJournal(cancelAwareStore{NewMemStore()})
	m := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
	if _, err := mustNew(m, store, WithMiddleware(cancelling)).Run(ctx, "r", UserText("go")); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	recs, _ := store.History(context.Background(), "r")
	var journaled Usage
	for _, r := range recs {
		if r.DiscardedUsage != nil {
			addUsage(&journaled, *r.DiscardedUsage)
		}
	}
	if journaled != billed {
		t.Fatalf("journaled spend = %+v, want %+v", journaled, billed)
	}
}

// meteredModel calls "lookup" until it has a tool result, then answers. Every call reports
// usage u, and calls counts them.
type meteredModel struct {
	u     Usage
	calls atomic.Int32
}

func (m *meteredModel) Stream(_ context.Context, req Request) (*Stream, error) {
	m.calls.Add(1)
	ch := make(chan Emit, 2)
	if req.Messages[len(req.Messages)-1].Role == RoleTool {
		ch <- Emit{Event: TextDelta{Text: "ok"}}
		ch <- Emit{Event: Finish{Reason: "stop", Usage: m.u}}
	} else {
		ch <- Emit{Event: ToolCallDelta{Index: 0, ID: "c1", Name: "lookup", ArgsFragment: []byte(`{}`)}}
		ch <- Emit{Event: Finish{Reason: "tool_use", Usage: m.u}}
	}
	close(ch)
	return NewStream(ch), nil
}

func lookupTool(onCall func()) Tool {
	return MustFunc("lookup", "look up", func(ctx context.Context, _ struct{}) (string, error) {
		onCall()
		return "found", ctx.Err()
	}, WithSafety(Safety{ReadOnly: true}))
}

var turnUsage = Usage{InputTokens: 100, OutputTokens: 20} // 120 tokens per model call

// The budget is per run: one agent serving many runs gives each its own budget.
func TestTokenBudget_IsPerRun(t *testing.T) {
	a := mustNew(
		&meteredModel{u: turnUsage},
		memJournal(),
		WithTools(lookupTool(func() {})),
		WithTokenBudget(300),
	)
	for _, run := range []string{"r1", "r2", "r3"} {
		if _, err := a.Run(context.Background(), run, UserText("q")); err != nil {
			t.Errorf("run %s (240 tokens, budget 300): %v", run, err)
		}
	}
}

// The call that crosses the budget completes, since its usage is known only after it returns;
// the next call is refused before it reaches the model.
func TestTokenBudget_RefusesTheNextCall(t *testing.T) {
	m := &meteredModel{u: turnUsage}
	_, err := mustNew(m, memJournal(), WithTools(lookupTool(func() {})), WithTokenBudget(100)).Run(context.Background(), "r1", UserText("q"))
	if !errors.Is(err, ErrBudgetExceeded) || !errors.Is(err, ErrBudget) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	if n := m.calls.Load(); n != 1 {
		t.Fatalf("the model was called %d times, want 1 (the second call is over budget)", n)
	}
}

// Cached input counts: a call that reads most of its prompt from the cache still used it.
func TestTokenBudget_CountsCachedInput(t *testing.T) {
	m := &meteredModel{u: Usage{InputTokens: 10, CacheReadTokens: 200, OutputTokens: 5}}
	_, err := mustNew(m, memJournal(), WithTools(lookupTool(func() {})), WithTokenBudget(100)).Run(context.Background(), "r1", UserText("q"))
	if !errors.Is(err, ErrBudgetExceeded) || m.calls.Load() != 1 {
		t.Fatalf("err = %v after %d calls; want ErrBudgetExceeded after 1 (215 tokens, budget 100)", err, m.calls.Load())
	}
}

// The budget survives a resume. A run stops after one model call (120 tokens); a new process
// resumes it with the same budget of 100. The run has already spent its budget, so the resumed
// run must be refused without calling the model.
func TestTokenBudget_HoldsAcrossResume(t *testing.T) {
	store := memJournal()
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := mustNew(&meteredModel{u: turnUsage}, store, WithTools(lookupTool(cancel)), WithTokenBudget(1000)).Run(ctx, "r1", UserText("q")); !errors.Is(err, context.Canceled) {
		t.Fatalf("setup: %v", err)
	}
	m := &meteredModel{u: turnUsage}
	_, err := mustNew(m, store, WithTools(lookupTool(func() {})), WithTokenBudget(100)).Run(context.Background(), "r1", UserText("q"))
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("resumed run: err = %v, want ErrBudgetExceeded (120 tokens already used, budget 100)", err)
	}
	if n := m.calls.Load(); n != 0 {
		t.Fatalf("the resumed run called the model %d times, want 0", n)
	}
}

// Concurrent runs on one agent share no budget state (run under -race).
func TestTokenBudget_ConcurrentRuns(t *testing.T) {
	a := mustNew(
		&meteredModel{u: turnUsage},
		memJournal(),
		WithTools(lookupTool(func() {})),
		WithTokenBudget(300),
	)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.Run(context.Background(), string(rune('a'+i)), UserText("q")); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

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
	return MustFunc("step", "one step", func(ctx context.Context, _ struct{}) (string, error) {
		if err := fn(ctx); err != nil {
			return "", err
		}
		return "stepped", nil
	}, WithSafety(Safety{ReadOnly: true}))
}

func times(u Usage, n int) Usage {
	return Usage{InputTokens: u.InputTokens * n, OutputTokens: u.OutputTokens * n, CacheReadTokens: u.CacheReadTokens * n, CacheWriteTokens: u.CacheWriteTokens * n}
}

// Result.Usage and Result.Spend are the whole run's, however many invocations it took: a run
// cut off after its first model call and resumed reports both calls, and re-entering it once
// finished reports the same again.
func TestResult_WholeRunAcrossResume(t *testing.T) {
	store := memJournal()
	ctx, cancel := context.WithCancel(context.Background())
	cut := stepTool(func(ctx context.Context) error { cancel(); return ctx.Err() })
	if _, err := mustNew(&stepper{n: 1, u: hundred}, store, WithTools(cut)).Run(ctx, "r1", UserText("q")); !errors.Is(err, context.Canceled) {
		t.Fatalf("first invocation: err = %v, want context.Canceled", err)
	}
	a := mustNew(
		&stepper{n: 1, u: hundred},
		store,
		WithTools(stepTool(func(context.Context) error { return nil })),
	)
	for _, pass := range []string{"resumed", "re-entered"} {
		res, err := a.Run(context.Background(), "r1", UserText("q"))
		if err != nil {
			t.Fatalf("%s: %v", pass, err)
		}
		if want := times(hundred, 2); res.Usage != want || res.Spend != want {
			t.Fatalf("%s: Usage = %+v, Spend = %+v; want both %+v (the run's two model calls)", pass, res.Usage, res.Spend, want)
		}
	}
}

// A sub-agent's model calls are part of its parent's run: the parent's Result counts them.
func TestResult_IncludesSubAgents(t *testing.T) {
	store := memJournal()
	sub := mustNew(
		&stepper{n: 1, u: hundred},
		store,
		WithTools(stepTool(func(context.Context) error { return nil })),
	)
	parent := mustNew(
		&delegator{ids: []string{"p1", "p2"}, u: hundred},
		store,
		WithTools(MustSubAgent("sub", "delegate", sub)),
	)
	res, err := parent.Run(context.Background(), "r1", UserText("q"))
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
	store := memJournal()
	subModel := &stepper{n: 10, u: hundred}
	sub := mustNew(subModel, store, WithTools(stepTool(func(context.Context) error { return nil })))
	parentModel := &delegator{ids: []string{"p1"}, u: hundred}
	parent := mustNew(parentModel, store, WithTools(MustSubAgent("sub", "delegate", sub)), WithTokenBudget(300))
	_, err := parent.Run(context.Background(), "r1", UserText("q"))
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
		store := memJournal()
		subModel := &stepper{n: 100, u: hundred}
		sub := mustNew(subModel, store, WithTools(stepTool(func(context.Context) error { return nil })))
		ids := make([]string, subs)
		for i := range ids {
			ids[i] = fmt.Sprintf("p%d", i)
		}
		parentModel := &delegator{ids: ids, u: hundred}
		_, err := mustNew(parentModel, store, WithTools(MustSubAgent("sub", "delegate", sub)), WithTokenBudget(budget)).Run(context.Background(), "r1", UserText("q"))
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
	resumedTreeCountsUnfinished(t, func(t Tool) Tool { return t })
}

// The same holds through a tool that wraps the sub-agent (see asSubAgent).
func TestTokenBudget_ResumedTreeCountsUnfinishedWrappedSubAgents(t *testing.T) {
	resumedTreeCountsUnfinished(t, func(t Tool) Tool { return wrappedSub{t} })
}

func resumedTreeCountsUnfinished(t *testing.T, wrap func(Tool) Tool) {
	store := memJournal()
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	seen := map[string]int{}
	var both sync.WaitGroup
	both.Add(2)
	cut := stepTool(func(ctx context.Context) error {
		scope := runScope(ctx)
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
	sub := mustNew(subModel, store, WithTools(cut))
	parentModel := &delegator{ids: []string{"p1", "p2"}, u: hundred}
	if _, err := mustNew(parentModel, store, WithTools(wrap(MustSubAgent("sub", "delegate", sub)))).Run(ctx, "r1", UserText("q")); !errors.Is(err, context.Canceled) {
		t.Fatalf("first invocation: err = %v, want context.Canceled", err)
	}
	if p, s := parentModel.calls.Load(), subModel.calls.Load(); p != 1 || s != 4 {
		t.Fatalf("before the crash: parent %d calls, sub-agents %d; want 1 and 4", p, s)
	}

	subModel2 := &stepper{n: 3, u: hundred}
	sub2 := mustNew(subModel2, store, WithTools(stepTool(func(context.Context) error { return nil })))
	parentModel2 := &delegator{ids: []string{"p1", "p2"}, u: hundred}
	parent2 := must(mustNew(
		parentModel2,
		store,
		WithTools(wrap(MustSubAgent("sub", "delegate", sub2))),
		WithTokenBudget(450),
	).With(WithMaxConcurrency(1)))
	if _, err := parent2.Run(context.Background(), "r1", UserText("q")); !errors.Is(err, ErrBudgetExceeded) {
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
	sub := mustNew(truncating{u: hundred}, memJournal())
	parentModel := &delegator{ids: []string{"p1"}, u: hundred}
	res, err := mustNew(parentModel, sub.store, WithTools(MustSubAgent("sub", "delegate", sub))).Run(context.Background(), "r1", UserText("q"))
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
	_, err = mustNew(parentModel, sub.store, WithTools(MustSubAgent("sub", "delegate", sub)), WithTokenBudget(200)).Run(context.Background(), "r2", UserText("q"))
	if !errors.Is(err, ErrBudgetExceeded) || parentModel.calls.Load() != 1 {
		t.Fatalf("err = %v after %d parent calls; want ErrBudgetExceeded after 1 (100 + the failed 100)", err, parentModel.calls.Load())
	}
}

// A sub-saga that aborts is part of its parent's usage too: the parent's failure record carries
// what the sub-saga used, also when the sub-saga aborted in an earlier attempt that the parent
// never recorded, so the parent re-enters an aborted sub-saga.
func TestSaga_FailureRecordCarriesSubAgentUsage(t *testing.T) {
	for _, earlier := range []bool{false, true} {
		store := memJournal()
		fail := stepTool(func(context.Context) error { return errors.New("step failed") })
		sub := mustNew(&stepper{n: 1, u: hundred}, store, WithTools(fail))
		if earlier {
			var aborted *SagaAborted
			id := SubRunID("r1", "p1")
			if _, err := sub.Run(asToolCall(context.Background(), "r1", "p1"), id, UserText("go"), WithSaga()); !errors.As(err, &aborted) {
				t.Fatalf("earlier attempt: err = %v, want *SagaAborted", err)
			}
		}
		parent := mustNew(
			&delegator{ids: []string{"p1"}, u: hundred},
			store,
			WithTools(MustSubAgent("sub", "delegate", sub)),
		)
		var aborted *SagaAborted
		if _, err := parent.Run(context.Background(), "r1", UserText("q"), WithSaga()); !errors.As(err, &aborted) {
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
	store := memJournal()
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	seen := map[string]int{}
	var both sync.WaitGroup
	both.Add(2)
	cut := stepTool(func(ctx context.Context) error {
		scope := runScope(ctx)
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
		g := mustNew(models[1], store, WithTools(step))
		b := mustNew(
			&delegator{ids: []string{"g1"}, u: hundred},
			store,
			WithTools(MustSubAgent("sub", "delegate", g)),
		)
		return mustNew(
			&delegator{ids: []string{"p1", "p2"}, names: []string{"a", "b"}, u: hundred},
			store,
			WithTools(MustSubAgent("a", "delegate", mustNew(models[0], store, WithTools(step))), MustSubAgent("b", "delegate", b)),
		)
	}
	var first [3]*stepper
	if _, err := build(cut, &first).Run(ctx, "r1", UserText("q")); !errors.Is(err, context.Canceled) {
		t.Fatalf("first invocation: err = %v, want context.Canceled", err)
	}
	if a, g := first[0].calls.Load(), first[1].calls.Load(); a != 2 || g != 2 {
		t.Fatalf("before the crash: A made %d calls, G %d; want 2 each", a, g)
	}
	var again [3]*stepper
	root := must(must(build(stepTool(func(context.Context) error { return nil }), &again).With(WithTokenBudget(450))).With(WithMaxConcurrency(1)))
	if _, err := root.Run(context.Background(), "r1", UserText("q")); !errors.Is(err, ErrBudgetExceeded) {
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
	if _, err := parent.Run(ctx, "r1", UserText("q")); !errors.Is(err, context.Canceled) {
		t.Fatalf("first invocation: err = %v, want context.Canceled", err)
	}
}

// A resumed tree counts what it used before the crash once. Two sub-agents cut off after two
// calls each (500 used with the parent's call) each need two more calls, and the parent one:
// 1000 in all. A budget of 901 lets the last call through, since 900 were used before it; counting
// the sub-agents' journals twice would refuse it.
func TestTokenBudget_ResumedTreeCountsOnce(t *testing.T) {
	store := memJournal()
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	seen := map[string]int{}
	var both sync.WaitGroup
	both.Add(2)
	cut := stepTool(func(ctx context.Context) error {
		scope := runScope(ctx)
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
	crashTree(t, ctx, mustNew(
		&delegator{ids: []string{"p1", "p2"}, u: hundred},
		store,
		WithTools(MustSubAgent("sub", "delegate", mustNew(&stepper{n: 3, u: hundred}, store, WithTools(cut)))),
	))

	sub := mustNew(
		&stepper{n: 3, u: hundred},
		store,
		WithTools(stepTool(func(context.Context) error { return nil })),
	)
	res, err := must(mustNew(
		&delegator{ids: []string{"p1", "p2"}, u: hundred},
		store,
		WithTools(MustSubAgent("sub", "delegate", sub)),
		WithTokenBudget(901),
	).With(WithMaxConcurrency(1))).Run(context.Background(), "r1", UserText("q"))
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
	store := memJournal()
	ctx, cancel := context.WithCancel(context.Background())
	sub := mustNew(
		&stepper{n: 1, u: hundred},
		store,
		WithTools(stepTool(func(context.Context) error { return nil })),
	)
	crashTree(t, ctx, mustNew(
		delegateThenStep{u: hundred},
		store,
		WithTools(MustSubAgent("sub", "delegate", sub), stepTool(func(ctx context.Context) error { cancel(); return ctx.Err() })),
	))

	res, err := mustNew(
		delegateThenStep{u: hundred},
		store,
		WithTools(MustSubAgent("sub", "delegate", sub), stepTool(func(context.Context) error { return nil })),
		WithTokenBudget(401),
	).Run(context.Background(), "r1", UserText("q"))
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
	store := memJournal()
	ctx, cancel := context.WithCancel(context.Background())
	steps := 0
	cut := stepTool(func(ctx context.Context) error {
		if steps++; steps == 2 {
			cancel()
			return ctx.Err()
		}
		return nil
	})
	crashTree(t, ctx, mustNew(
		&delegator{ids: []string{"p1"}, u: hundred},
		store,
		WithTools(MustSubAgent("sub", "delegate", mustNew(&stepper{n: 5, u: hundred}, store, WithTools(cut)))),
	))

	subModel := &stepper{n: 5, u: hundred}
	sub := mustNew(
		subModel,
		store,
		WithTools(stepTool(func(context.Context) error { return nil })),
		WithTokenBudget(250),
	)
	if _, err := mustNew(
		&delegator{ids: []string{"p1"}, u: hundred},
		store,
		WithTools(MustSubAgent("sub", "delegate", sub)),
	).Run(context.Background(), "r1", UserText("q")); err != nil {
		t.Fatalf("resumed: %v (the sub-agent's budget failure is its tool call's error, not the parent's)", err)
	}
	if n := subModel.calls.Load(); n != 1 {
		t.Fatalf("the resumed sub-agent made %d model calls, want 1 (200 of 250 used before it)", n)
	}
}

// countDecodes counts the DecodeRecord calls, from any goroutine, whose bytes contain marker, until
// the test ends. A marker unique to the test keeps other tests' decodes out of the count.
func countDecodes(t *testing.T, marker string) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	h := func(b []byte) {
		if bytes.Contains(b, []byte(marker)) {
			n.Add(1)
		}
	}
	if !decodeHook.CompareAndSwap(nil, &h) {
		t.Fatal("another test holds the decode hook")
	}
	t.Cleanup(func() { decodeHook.Store(nil) })
	return &n
}

// A step MemStore.Do records is decoded once, by the encoding's own round trip: the record Do
// returns is that decoding, not a second decode of the same bytes. It is still exactly what
// History reads back for the step, and the caller's own copy.
func TestMemStoreDo_DecodesAWriteOnce(t *testing.T) {
	ctx := context.Background()
	const run = "decode-once-run"
	decodes := countDecodes(t, run)
	s := memJournal()
	msg := Message{Role: RoleAssistant, Parts: []Part{Text{Text: "hi <b>"}, ToolUse{ID: "c1", Name: "t", Args: json.RawMessage(`{ "a" : 1.50 }`)}}}
	got, err := s.do(ctx, run, "@llm/0", func(context.Context) (Record, error) {
		return Record{Kind: StepModel, Message: &msg, Usage: &Usage{InputTokens: 3}, Result: json.RawMessage(`"` + run + `"`)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := decodes.Load(); n != 1 {
		t.Fatalf("recording one step decoded it %d times, want once", n)
	}
	hist, err := s.History(ctx, run)
	if err != nil || len(hist) != 2 { // the journal header, then the step
		t.Fatalf("History = %v, %v", hist, err)
	}
	if !reflect.DeepEqual(got, hist[1]) {
		t.Fatalf("Do returned\n%#v\nHistory reads\n%#v", got, hist[1])
	}
	// The caller's copy is its own: changing it changes neither the journal nor the step's input.
	got.Message.Parts[0] = Text{Text: "changed"}
	got.Result[1] = 'X'
	again, _ := s.History(ctx, run)
	if !reflect.DeepEqual(again[1], hist[1]) {
		t.Fatal("changing the record Do returned changed the journal")
	}
	if msg.Parts[0] != (Text{Text: "hi <b>"}) {
		t.Fatal("changing the record Do returned changed the step's own record")
	}
}

// When the encoding's two passes differ, Do decodes the stored bytes, as History does, rather than
// hand back the first pass's decoding. They differ on invalid UTF-8 in a Go string field where the
// first pass writes it as the escape \ufffd and the second the character itself: in a
// GOEXPERIMENT=nojsonv2 build (CI runs one). The default build writes the character in both passes,
// so there the passes agree and one decode is all Do makes.
func TestMemStoreDo_UnstableEncodingDecodesStoredBytes(t *testing.T) {
	ctx := context.Background()
	const run = "decode-unstable-run"
	rec := Record{Name: "v", Kind: StepValue, Result: json.RawMessage(`"` + run + `"`), ApproverSignature: &ApproverSignature{Approver: "bad\xffutf8"}}
	first, err := marshalJournal(rec)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := EncodeRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	want := int64(1)
	if !bytes.Equal(first, stored) {
		want = 2 // the round trip's decode, then the stored bytes'
	}
	decodes := countDecodes(t, run)
	s := memJournal()
	got, err := s.do(ctx, run, "v", func(context.Context) (Record, error) {
		rec.Name = ""
		return rec, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := decodes.Load(); n != want {
		t.Fatalf("recording the step decoded it %d times, want %d (passes agree: %v)", n, want, want == 1)
	}
	hist, _ := s.History(ctx, run)
	if !reflect.DeepEqual(got, hist[1]) || got.Approver() != "bad"+replacementChar+"utf8" {
		t.Fatalf("Do returned %#v, History reads %#v", got, hist[1])
	}
}

// A step name the encoding does not keep (invalid UTF-8 becomes U+FFFD) is refused as the stored
// record naming another step, whichever way Do comes by the decoding.
func TestMemStoreDo_NameNotKeptIsStorageError(t *testing.T) {
	s := memJournal()
	_, err := s.do(context.Background(), "r", "bad\xffname", func(context.Context) (Record, error) {
		return Record{Kind: StepValue}, nil
	})
	if err == nil {
		t.Fatal("Do recorded a step under a name its record does not carry")
	}
}

// Callers that share one write through the single flight, or read it after it landed, each get
// their own copy of the stored record.
func TestMemStoreDo_SharedWriteCopies(t *testing.T) {
	ctx := context.Background()
	s := memJournal()
	started, release := make(chan struct{}), make(chan struct{})
	fn := func(context.Context) (Record, error) {
		close(started)
		<-release
		return Record{Kind: StepValue, Result: json.RawMessage(`{"a":[1,2]}`)}, nil
	}
	var wg sync.WaitGroup
	out := make([]Record, 3)
	wg.Add(1)
	go func() {
		defer wg.Done()
		out[0], _ = s.do(ctx, "r", "v", fn)
	}()
	<-started
	for i := 1; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i], _ = s.do(ctx, "r", "v", func(context.Context) (Record, error) {
				t.Error("a second fn ran for a step already being recorded")
				return Record{}, nil
			})
		}()
	}
	close(release)
	wg.Wait()
	for i := range out {
		if string(out[i].Result) != `{"a":[1,2]}` {
			t.Fatalf("caller %d got %q", i, out[i].Result)
		}
	}
	out[0].Result[1] = 'X'
	for i := 1; i < 3; i++ {
		if string(out[i].Result) != `{"a":[1,2]}` {
			t.Fatalf("changing caller 0's record changed caller %d's: %q", i, out[i].Result)
		}
	}
}

// A live run decodes each record it writes once: its budget and token totals are kept up to date
// from the records it writes, not by decoding them again or reading the journal back each turn.
// Its reads decode nothing: the Load once run:start is written (model 10's DStart returns to
// DOpen) needs only the entries' names, and the point reads of the end markers (at the turn
// boundary, and after run:complete) find none but run:complete, which carries no marker.
// TestBudget_FirstDriveAndCompletion and its neighbours hold the store round trips themselves.
func TestLiveRun_DecodesEachWriteOnce(t *testing.T) {
	const marker = "journal-ops-marker"
	decodes := countDecodes(t, marker)
	tool := MustFunc("noop", "no-op", func(context.Context, struct{}) (string, error) { return "ok", nil }, WithSafety(Safety{ReadOnly: true}))
	a := mustNew(NewScriptedModel(ToolTurn("c1-"+marker, "noop", `{}`), TextTurn("done "+marker)), memJournal(),
		WithTools(tool), WithTokenBudget(1_000_000))
	if _, err := a.Run(context.Background(), "r", UserText("go "+marker)); err != nil {
		t.Fatal(err)
	}
	// run:start, @llm/0, the tool's result and @llm/1 carry the marker (the input, the call's ID,
	// the answer). The three steps are decoded once each, as they are recorded; run:start is
	// written from its encoding and never decoded on the live path.
	if n := decodes.Load(); n != 3 {
		t.Fatalf("the run decoded the records that carry the marker %d times, want 3 (each step once)", n)
	}
}

// A raw value holding one line or paragraph separator alone is written with that separator's
// escape: the encoder's fast check for the separators' lead byte must skip neither replacement.
// The default JSON build escapes both itself, so this pins the check in a GOEXPERIMENT=nojsonv2
// build (CI runs one).
func TestEncodeRecord_SeparatorAloneEscaped(t *testing.T) {
	for _, c := range []struct{ sep, esc string }{{lineSep, jsonEscape + "2028"}, {paraSep, jsonEscape + "2029"}} {
		b, err := EncodeRecord(Record{Name: "v", Kind: StepValue, Result: json.RawMessage(`"` + c.sep + `"`)})
		if err != nil {
			t.Fatal(err)
		}
		if want := `{"name":"v","kind":"value","result":"` + c.esc + `"}`; string(b) != want {
			t.Errorf("EncodeRecord = %q, want %q", b, want)
		}
	}
}
