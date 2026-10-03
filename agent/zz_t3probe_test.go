package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"
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
	st := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	a := mustNew(m, st, WithTools(charge))
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

// Model 9, T4: a middleware leaves next running and answers the call itself with a success (a
// cache hit). The call is recorded as succeeded while its tool still runs; a later step fails the
// saga, the rollback compensates the call, and the tool's effect lands after its compensation.
func TestT4_LeakedNextEffectAfterCompensation(t *testing.T) {
	release, ran := make(chan struct{}), make(chan struct{})
	var charged, refunded atomic.Int32
	var refundedBeforeCharge atomic.Bool
	charge := CompensatedFunc("charge", "", Safety{Idempotent: true},
		func(ctx context.Context, _ struct{}) (string, error) {
			<-release
			if refunded.Load() > 0 {
				refundedBeforeCharge.Store(true)
			}
			charged.Add(1)
			close(ran)
			return "ok", nil
		},
		func(context.Context, struct{}, string) error { refunded.Add(1); return nil })
	fail := Func("fail", "", Safety{}, func(context.Context, struct{}) (string, error) { return "", errors.New("declined") })
	leak := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			if call.Use.Name != "charge" {
				return next(ctx, call)
			}
			go next(context.WithoutCancel(ctx), call) //nolint:errcheck
			time.Sleep(20 * time.Millisecond)         // let next reach the tool
			return json.RawMessage(`"ok"`), nil       // a cache hit
		}
	})
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), ToolTurn("c2", "fail", `{}`), TextTurn("done"))
	_, err := mustNew(m, memJournal(), WithTools(charge, fail), WithToolMiddleware(leak)).RunSaga(context.Background(), "r", "go")
	close(release)
	<-ran
	var ab *SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("RunSaga = %v, want *SagaAborted", err)
	}
	if slices.Contains(ab.Compensated, "charge") && refundedBeforeCharge.Load() {
		t.Fatalf("charge reported compensated, but its tool charged after the refund: charged %d, refunded %d", charged.Load(), refunded.Load())
	}
}

// Model 9, T5: a middleware calls next and returns its result, and also leaves a second next
// running that reaches the retry-safe tool only after the chain returned. The tool begins
// again after its result was recorded, and after the rollback's compensation.
func TestT5_RetrySafeBeginsAfterChainReturned(t *testing.T) {
	release, done := make(chan struct{}), make(chan struct{})
	var charged, refunded atomic.Int32
	var chargedAfterRefund atomic.Bool
	charge := CompensatedFunc("charge", "", Safety{Idempotent: true},
		func(ctx context.Context, _ struct{}) (string, error) {
			if refunded.Load() > 0 {
				chargedAfterRefund.Store(true)
			}
			charged.Add(1)
			return "ok", nil
		},
		func(context.Context, struct{}, string) error { refunded.Add(1); return nil })
	fail := Func("fail", "", Safety{}, func(context.Context, struct{}) (string, error) { return "", errors.New("declined") })
	leak := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			if call.Use.Name != "charge" {
				return next(ctx, call)
			}
			res, err := next(ctx, call)
			go func() { <-release; next(context.WithoutCancel(ctx), call); close(done) }() //nolint:errcheck
			return res, err
		}
	})
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), ToolTurn("c2", "fail", `{}`), TextTurn("done"))
	_, err := mustNew(m, memJournal(), WithTools(charge, fail), WithToolMiddleware(leak)).RunSaga(context.Background(), "r", "go")
	close(release)
	<-done
	var ab *SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("RunSaga = %v, want *SagaAborted", err)
	}
	if chargedAfterRefund.Load() {
		t.Fatalf("the tool began after its chain returned and charged after the refund: charged %d, refunded %d, compensated %v", charged.Load(), refunded.Load(), ab.Compensated)
	}
}
