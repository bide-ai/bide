package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
)

// Baseline for R117-2: the same gap, before P12, for a retry-safe tool that itself says its
// outcome is unknown (#57), in a saga.
func TestR117Base_SagaUnknownOutcomeOfARetrySafeWrite(t *testing.T) {
	var committed, undone atomic.Int32
	hold := CompensatedFunc("hold", "", Safety{Idempotent: true},
		func(ctx context.Context, _ struct{}) (string, error) {
			committed.Add(1)
			return "", fmt.Errorf("connection reset: %w", ErrToolOutcomeUnknown)
		},
		func(context.Context, struct{}, string) error { undone.Add(1); return nil })
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "hold", `{}`), TextTurn("done"))
	_, err := mustNew(m, store, WithTools(hold)).RunSaga(context.Background(), "s1", "book")
	var ab *SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("RunSaga: err = %v, want *SagaAborted", err)
	}
	// Reported in the distinct UnknownOutcome list (it may have committed), not compensated blind.
	if undone.Load() != 0 || len(ab.UnknownOutcome) != 1 || ab.UnknownOutcome[0] != "hold" || len(ab.Uncompensated) != 0 {
		t.Fatalf("unknown-outcome write: undone %d, unknown %q, uncompensated %q; want it reported as unknown, not compensated", undone.Load(), ab.UnknownOutcome, ab.Uncompensated)
	}
	if !strings.Contains(ab.Error(), "UNKNOWN OUTCOME") {
		t.Fatalf("SagaAborted.Error() = %q, want the unknown outcome named", ab.Error())
	}
}

// A sub-agent's unknown-outcome step reaches the root's SagaAborted.UnknownOutcome: the tree's
// lists are whole.
func TestR117_UnknownOutcomeInASubAgentIsReportedAtTheRoot(t *testing.T) {
	hold := CompensatedFunc("hold", "", Safety{Idempotent: true},
		func(ctx context.Context, _ struct{}) (string, error) {
			return "", fmt.Errorf("connection reset: %w", ErrToolOutcomeUnknown)
		},
		func(context.Context, struct{}, string) error { return nil })
	store := memJournal()
	sub := mustNew(NewScriptedModel(ToolTurn("h1", "hold", `{}`), TextTurn("done")), store, WithTools(hold))
	parent := mustNew(
		NewScriptedModel(ToolTurn("p1", "delegate", `{"task":"x"}`), TextTurn("done")),
		store,
		WithTools(SubAgent("delegate", "", sub)),
	)
	_, err := parent.RunSaga(context.Background(), "root", "go")
	var ab *SagaAborted
	if !errors.As(err, &ab) || len(ab.UnknownOutcome) != 1 || ab.UnknownOutcome[0] != "hold" {
		t.Fatalf("RunSaga = %v; want *SagaAborted naming hold as unknown", err)
	}
}
