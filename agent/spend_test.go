package agent

import (
	"context"
	"errors"
	"iter"
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
// RunResult and in the journal.
func TestRunResult_ReportsDiscardedSpend(t *testing.T) {
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
	var pa *PendingApproval
	if !errors.As(err, &pa) {
		t.Fatalf("err = %v, want PendingApproval", err)
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
func TestRunResult_SpendOfBrokenStream(t *testing.T) {
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
func TestRunResult_SpendOfSuppliedResponse(t *testing.T) {
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
func TestRunResult_SpendAcrossTurns(t *testing.T) {
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
