package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// wrappedSub is a tool that wraps a SubAgent (as audit.AttenuatingSubAgent does) and says so with
// Unwrap. Its Call adds nothing; what matters is that it is not the SubAgent tool itself.
type wrappedSub struct{ Tool }

func (w wrappedSub) Spec() ToolSpec { return SpecOf(w.Tool) }
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
		WithTools(wrappedSub{SubAgent("delegate", "", sub)}, failTool("boom")),
	)

	_, err := parent.RunSaga(context.Background(), "root", "go")
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
		WithTools(wrappedSub{SubAgent("delegate", "", sub)}),
	)

	_, err := parent.RunSaga(context.Background(), "root", "go")
	var ab *SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("err = %v, want *SagaAborted", err)
	}
	if l.undoCount["X"] != 1 || len(ab.Compensated) != 1 || ab.Compensated[0] != "X" {
		t.Fatalf("undo X = %d, compensated %v; want X undone once and reported", l.undoCount["X"], ab.Compensated)
	}
	l.assertClean(t)
}
