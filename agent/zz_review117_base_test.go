package agent

import (
	"context"
	"errors"
	"fmt"
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
	store := NewMemStore()
	m := NewScriptedModel(ToolTurn("c1", "hold", `{}`), TextTurn("done"))
	_, err := New(m, store, hold).RunSaga(context.Background(), "s1", "book")
	var ab *SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("RunSaga: err = %v, want *SagaAborted", err)
	}
	if committed.Load() == 1 && undone.Load() == 0 && len(ab.Uncompensated) == 0 {
		t.Fatalf("unknown-outcome write neither compensated nor reported: %+v", ab)
	}
}
