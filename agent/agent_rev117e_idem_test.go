package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
)

// T1's saga case for a retry-safe step that mutates state (Idempotent, with a compensator): a
// middleware turns the step's success into an error (a result check, which the ToolMiddleware
// contract allows). The step fired, yet the saga failure is recorded as a known failure, so the
// rollback skips it as "made no change", and SagaAborted lists it nowhere.
func TestRev117e_IdempotentSagaStepRejectedSuccessIsNotAccounted(t *testing.T) {
	var charged, refunded atomic.Int32
	charge := CompensatedFunc("charge", "", Safety{Idempotent: true},
		func(context.Context, struct{}) (string, error) { charged.Add(1); return "ok", nil },
		func(context.Context, struct{}, string) error { refunded.Add(1); return nil })
	check := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			res, err := next(ctx, call)
			if err == nil && call.Use.Name == "charge" {
				return nil, errors.New("result failed validation")
			}
			return res, err
		}
	})
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	_, err := mustNew(m, memJournal(), WithTools(charge), WithToolMiddleware(check)).RunSaga(context.Background(), "r", "go")
	var ab *SagaAborted
	if !errors.As(err, &ab) {
		if !errors.Is(err, ErrToolOutcomeUnknown) {
			t.Fatalf("RunSaga = %v, want *SagaAborted or a halt on the step's outcome", err)
		}
		return
	}
	accounted := refunded.Load() == 1 || slices.Contains(ab.UnknownOutcome, "charge") || slices.Contains(ab.Uncompensated, "charge")
	if charged.Load() == 1 && !accounted {
		t.Fatalf("the idempotent charge fired once, was refunded %d times, and the abort lists it nowhere: compensated %v, uncompensated %v, unknown %v",
			refunded.Load(), ab.Compensated, ab.Uncompensated, ab.UnknownOutcome)
	}
}

// A ReadOnly saga step changed nothing, so a middleware's rejection of its success stays an
// ordinary failure: it is not reported as an unknown outcome.
func TestRev117e_ReadOnlySagaStepRejectedSuccessIsAFailure(t *testing.T) {
	for _, safety := range []Safety{{ReadOnly: true}, {ReadOnly: true, Idempotent: true}} {
		testReadOnlySagaStepRejectedSuccess(t, safety)
	}
}

func testReadOnlySagaStepRejectedSuccess(t *testing.T, safety Safety) {
	look := Func("look", "", safety, func(context.Context, struct{}) (string, error) { return "ok", nil })
	check := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			if _, err := next(ctx, call); err != nil {
				return nil, err
			}
			return nil, errors.New("result failed validation")
		}
	})
	m := NewScriptedModel(ToolTurn("c1", "look", `{}`), TextTurn("done"))
	_, err := mustNew(m, memJournal(), WithTools(look), WithToolMiddleware(check)).RunSaga(context.Background(), "r", "go")
	var ab *SagaAborted
	if !errors.As(err, &ab) || len(ab.UnknownOutcome) != 0 {
		t.Fatalf("%+v: RunSaga = %v; want a SagaAborted with no unknown outcome", safety, err)
	}
}
