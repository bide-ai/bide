package agent

import (
	"context"
	"errors"
	"testing"
)

// A replayed run reports the same spend as the original: a turn whose first attempt failed and
// was retried discarded that attempt's usage, and the replay, run with no retry middleware at
// all, reports it again, in RunResult and in its journal.
func TestReplay_ReportsDiscardedSpend(t *testing.T) {
	ctx := context.Background()
	rec := NewMemStore()
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{
		truncatedTurn(billed),
		toolTurnWithUsage("c1", "lookup", `{}`, billed),
		textTurnWithUsage("done", billed),
	}}
	orig, err := New(m, rec, tool).Use(retryOnceMW).RunResult(ctx, "run", "go")
	if err != nil {
		t.Fatal(err)
	}

	rm, err := Replay(ctx, rec, "run")
	if err != nil {
		t.Fatal(err)
	}
	fresh := NewMemStore()
	replayed, err := New(rm, fresh, tool).RunResult(ctx, "run", "go")
	if err != nil {
		t.Fatalf("replay run: %v", err)
	}
	if replayed.Spend != orig.Spend || replayed.Usage != orig.Usage {
		t.Fatalf("replayed Spend %+v, Usage %+v; want %+v and %+v", replayed.Spend, replayed.Usage, orig.Spend, orig.Usage)
	}
	journalsEqual(t, rec, fresh, "run")
}

// The budget stops a replayed run where it stopped the original, counting discarded spend.
func TestReplay_BudgetCountsDiscardedSpend(t *testing.T) {
	ctx := context.Background()
	rec := NewMemStore()
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{
		truncatedTurn(billed),
		toolTurnWithUsage("c1", "lookup", `{}`, billed),
		textTurnWithUsage("done", billed),
	}}
	_, origErr := New(m, rec, tool).Use(retryOnceMW).WithTokenBudget(200).Run(ctx, "run", "go")
	if !errors.Is(origErr, ErrBudgetExceeded) {
		t.Fatalf("setup: err = %v, want ErrBudgetExceeded", origErr)
	}
	rm, err := Replay(ctx, rec, "run")
	if err != nil {
		t.Fatal(err)
	}
	fresh := NewMemStore()
	_, replayErr := New(rm, fresh, tool).WithTokenBudget(200).Run(ctx, "run", "go")
	if replayErr == nil || replayErr.Error() != origErr.Error() {
		t.Fatalf("replay err = %v, want %v", replayErr, origErr)
	}
	journalsEqual(t, rec, fresh, "run")
}

// A model call that failed for good in the original fails at the same point in the replay and
// journals the same spend, so re-invoking the replayed run meets the budget where the original
// did.
func TestReplay_FailedCallsFailAndCountAtTheSamePoint(t *testing.T) {
	ctx := context.Background()
	rec := NewMemStore()
	m := &scriptModel{turns: [][]Emit{truncatedTurn(billed), truncatedTurn(billed), textTurnWithUsage("done", billed)}}
	var origErrs []error
	for range 3 {
		_, err := New(m, rec).WithTokenBudget(200).Run(ctx, "run", "go")
		origErrs = append(origErrs, err)
	}
	if !errors.Is(origErrs[0], ErrModel) || !errors.Is(origErrs[1], ErrModel) || !errors.Is(origErrs[2], ErrBudgetExceeded) {
		t.Fatalf("setup: errs = %v, want two model errors then ErrBudgetExceeded", origErrs)
	}

	rm, err := Replay(ctx, rec, "run")
	if err != nil {
		t.Fatal(err)
	}
	fresh := NewMemStore()
	for i := range 3 {
		_, err := New(rm, fresh).WithTokenBudget(200).Run(ctx, "run", "go")
		if errors.Is(err, ErrBudgetExceeded) != errors.Is(origErrs[i], ErrBudgetExceeded) || errors.Is(err, ErrModel) != errors.Is(origErrs[i], ErrModel) {
			t.Fatalf("replay invocation %d: err = %v, want the original's %v", i, err, origErrs[i])
		}
	}
	journalsEqual(t, rec, fresh, "run")
}
