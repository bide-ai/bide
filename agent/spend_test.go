package agent

import (
	"context"
	"errors"
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
	store := NewMemStore()
	res, err := New(m, store).Use(retryOnceMW).RunResult(context.Background(), "r", "go")
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
	_, err := New(m, NewMemStore(), tool).Use(retryOnceMW).WithTokenBudget(200).Run(context.Background(), "r", "go")
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
	store := NewMemStore()
	var calls int
	gated := &countingTool{name: "lookup", safety: Safety{ReadOnly: true, RequiresApproval: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{truncatedTurn(billed), toolTurnWithUsage("c1", "lookup", `{}`, billed)}}
	_, err := New(m, store, gated).Use(retryOnceMW).Run(ctx, "r", "go")
	var pa *PendingApproval
	if !errors.As(err, &pa) {
		t.Fatalf("err = %v, want PendingApproval", err)
	}
	if err := Approve(ctx, store, "r", "c1", true); err != nil {
		t.Fatal(err)
	}
	resumed := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
	_, err = New(resumed, store, gated).WithTokenBudget(200).Run(ctx, "r", "go")
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
	store := NewMemStore()
	m := &scriptModel{turns: [][]Emit{truncatedTurn(billed), truncatedTurn(billed), truncatedTurn(billed)}}
	a := New(m, store).WithTokenBudget(200)
	for i := range 2 {
		if _, err := a.Run(ctx, "r", "go"); !errors.Is(err, ErrModel) {
			t.Fatalf("run %d: err = %v, want ErrModel", i, err)
		}
	}
	// 240 tokens spent on two failed calls: the third invocation must not call the model.
	_, err := New(m, store).WithTokenBudget(200).Run(ctx, "r", "go")
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	if m.i != 2 {
		t.Fatalf("the model was called %d times, want 2", m.i)
	}
}
