package agent

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
)

// Model 9, T3: a retry-safe saga step that changes state (Idempotent, a compensator) fires, and
// the run is cancelled before its result is recorded (nothing is recorded: a retry-safe call has
// no attempt marker). The run is driven again and the re-run fails with the tool's own error,
// having done nothing this time: a known saga failure. The rollback skips the step as one that
// made no change, though the first attempt's charge is in place, and lists it nowhere.
func TestT3_IdempotentSagaStepEarlierAttemptNotAccounted(t *testing.T) {
	var calls, charged, refunded atomic.Int32
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	charge := CompensatedFunc("charge", "", Safety{Idempotent: true},
		func(ctx context.Context, _ struct{}) (string, error) {
			if calls.Add(1) == 1 {
				charged.Add(1) // the effect takes place, then the run is cancelled
				cancel1()
				return "", ctx.Err()
			}
			return "", errors.New("card declined") // the tool's own error: nothing done this time
		},
		func(context.Context, struct{}, string) error { refunded.Add(1); return nil })
	st := NewMemStore()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	a := New(m, st, charge)
	_, err := a.RunSaga(ctx1, "r", "go")
	if err == nil {
		t.Fatalf("first drive: want the cancellation, got nil")
	}
	_, err = a.RunSaga(context.Background(), "r", "go")
	var ab *SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("second drive = %v (calls %d), want *SagaAborted", err, calls.Load())
	}
	accounted := refunded.Load() == 1 || slices.Contains(ab.UnknownOutcome, "charge") || slices.Contains(ab.Uncompensated, "charge")
	if charged.Load() == 1 && !accounted {
		t.Fatalf("the idempotent charge took effect in the first attempt, was refunded %d times, and the abort lists it nowhere: compensated %v, uncompensated %v, unknown %v",
			refunded.Load(), ab.Compensated, ab.Uncompensated, ab.UnknownOutcome)
	}
}
