package agent

import (
	"context"
	"errors"
	"testing"
)

// A replayed run reports the same spend as the original: a turn whose first attempt failed and
// was retried discarded that attempt's usage, and the replay, run with no retry middleware at
// all, reports it again, in Run and in its journal.
func TestReplay_ReportsDiscardedSpend(t *testing.T) {
	ctx := context.Background()
	rec := memJournal()
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{
		truncatedTurn(billed),
		toolTurnWithUsage("c1", "lookup", `{}`, billed),
		textTurnWithUsage("done", billed),
	}}
	orig, err := mustNew(m, rec, WithTools(tool), WithMiddleware(retryOnceMW)).Run(ctx, "run", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}

	rm, err := Replay(ctx, rec, "run")
	if err != nil {
		t.Fatal(err)
	}
	fresh := memJournal()
	replayed, err := mustNew(rm, fresh, WithTools(tool)).Run(ctx, "run", UserText("go"))
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
	rec := memJournal()
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{
		truncatedTurn(billed),
		toolTurnWithUsage("c1", "lookup", `{}`, billed),
		textTurnWithUsage("done", billed),
	}}
	_, origErr := must(mustNew(m, rec, WithTools(tool), WithMiddleware(retryOnceMW)).With(WithTokenBudget(200))).Run(ctx, "run", UserText("go"))
	if !errors.Is(origErr, ErrBudgetExceeded) {
		t.Fatalf("setup: err = %v, want ErrBudgetExceeded", origErr)
	}
	rm, err := Replay(ctx, rec, "run")
	if err != nil {
		t.Fatal(err)
	}
	fresh := memJournal()
	_, replayErr := mustNew(rm, fresh, WithTools(tool), WithTokenBudget(200)).Run(ctx, "run", UserText("go"))
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
	rec := memJournal()
	m := &scriptModel{turns: [][]Emit{truncatedTurn(billed), truncatedTurn(billed), textTurnWithUsage("done", billed)}}
	var origErrs []error
	for range 3 {
		_, err := mustNew(m, rec, WithTokenBudget(200)).Run(ctx, "run", UserText("go"))
		origErrs = append(origErrs, err)
	}
	if !errors.Is(origErrs[0], ErrModel) || !errors.Is(origErrs[1], ErrModel) || !errors.Is(origErrs[2], ErrBudgetExceeded) {
		t.Fatalf("setup: errs = %v, want two model errors then ErrBudgetExceeded", origErrs)
	}

	rm, err := Replay(ctx, rec, "run")
	if err != nil {
		t.Fatal(err)
	}
	fresh := memJournal()
	for i := range 3 {
		_, err := mustNew(rm, fresh, WithTokenBudget(200)).Run(ctx, "run", UserText("go"))
		if errors.Is(err, ErrBudgetExceeded) != errors.Is(origErrs[i], ErrBudgetExceeded) || errors.Is(err, ErrModel) != errors.Is(origErrs[i], ErrModel) {
			t.Fatalf("replay invocation %d: err = %v, want the original's %v", i, err, origErrs[i])
		}
	}
	journalsEqual(t, rec, fresh, "run")
}
