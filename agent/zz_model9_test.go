package agent

// Tests for the findings of model 9 (spec/tla/toolcall), the tool-call state machine.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// T1, within the ToolMiddleware contract: a middleware may transform a result before it is
// journaled, so it may turn a side effect's success into an error (a result check). The loop then
// records a saga failure for a step whose effect fired: the rollback skips the step that aborted
// the saga ("it made no change"), and the abort lists it neither as compensated, uncompensated,
// nor as an unknown outcome. (Model: findings/t1-saga-maperr, SagaAccounted.)
func TestModel9_T1_SagaStepWhoseSuccessAMiddlewareRejectedIsNotAccounted(t *testing.T) {
	var charged, refunded atomic.Int32
	charge := MustCompensatedFunc("charge", "", func(context.Context, struct{}) (string, error) { charged.Add(1); return "ok", nil },
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
	_, err := mustNew(m, memJournal(), WithTools(charge), WithToolMiddleware(check)).Run(context.Background(), "r", UserText("go"), WithSaga())
	var ab *SagaAborted
	if !errors.As(err, &ab) {
		// The other sound answer: the run halts for the step's outcome, recording nothing.
		if !errors.Is(err, ErrToolOutcomeUnknown) {
			t.Fatalf("RunSaga = %v, want *SagaAborted or a halt on the step's outcome", err)
		}
		return
	}
	accounted := refunded.Load() == 1 || slices.Contains(ab.UnknownOutcome, "charge") || slices.Contains(ab.Uncompensated, "charge")
	if charged.Load() == 1 && !accounted {
		t.Fatalf("the charge fired once, was refunded %d times, and the abort lists it nowhere: compensated %v, uncompensated %v, unknown %v",
			refunded.Load(), ab.Compensated, ab.Uncompensated, ab.UnknownOutcome)
	}
}

// T1, a middleware that retries a side effect (outside the contract, which the agent says it
// enforces): the first invocation fires the effect and reports ErrToolOutcomeUnknown; the retry
// gets ErrToolReinvoked, and the middleware returns that. The loop records a known failure, so
// the run goes on as if the effect had not happened, where the first error alone would have
// halted it. (Model: findings/t1-retry, NoDoubleFire.)
func TestModel9_T1_RetryAfterAnUnknownOutcomeRecordsAKnownFailure(t *testing.T) {
	var charged atomic.Int32
	charge := MustFunc("charge", "", func(context.Context, struct{}) (string, error) {
		charged.Add(1)
		return "", fmt.Errorf("connection reset after the request went out: %w", ErrToolOutcomeUnknown)
	})
	retry := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (res json.RawMessage, err error) {
			for range 2 {
				if res, err = next(ctx, call); err == nil {
					return res, nil
				}
			}
			return res, err
		}
	})
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	_, err := mustNew(m, store, WithTools(charge), WithToolMiddleware(retry)).Run(context.Background(), "r", UserText("go"))
	r, ok := hasStep(t, store, "r", ToolResultStep("c1"))
	if charged.Load() == 1 && ok && r.IsError {
		t.Fatalf("the effect fired, its outcome was unknown, and the journal records a known failure %s (run err %v)", r.Result, err)
	}
}

// argsFailStore fails the first write of the saga's accepted arguments.
type argsFailStore struct {
	*MemStore
	failed atomic.Bool
}

func (s *argsFailStore) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	if name == sagaArgsStep("c1") && s.failed.CompareAndSwap(false, true) {
		return Entry{}, false, errors.New("store unavailable")
	}
	return s.MemStore.Insert(ctx, runID, name, data)
}

// T2: round 4's fix (c) marks a call as run only once it is reached, but the mark still comes
// before the saga's accepted-arguments write. When that write fails, the tool never begins (the
// loop seals the call, rightly, as not called), yet a retry of next is answered "already ran and
// is not retry-safe", and that text is what the journal records. (Model: findings/t2-ran-before-args,
// TruthfulRecord.)
func TestModel9_T2_FailedArgsWriteThenRetryRecordsAlreadyRan(t *testing.T) {
	var charged atomic.Int32
	charge := MustCompensatedFunc("charge", "", func(_ context.Context, in chargeArgs) (string, error) { charged.Add(1); return "ok", nil },
		func(context.Context, chargeArgs, string) error { return nil })
	retry := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (res json.RawMessage, err error) {
			for range 2 {
				if res, err = next(ctx, call); err == nil {
					return res, nil
				}
			}
			return res, err
		}
	})
	store := mustJournal(&argsFailStore{MemStore: NewMemStore()})
	m := NewScriptedModel(ToolTurn("c1", "charge", `{"amount":5}`), TextTurn("done"))
	_, _ = mustNew(m, store, WithTools(charge), WithToolMiddleware(retry, scaleCharge)).Run(context.Background(), "r", UserText("go"), WithSaga())
	recs, _ := store.History(context.Background(), "r")
	for _, r := range recs {
		if r.ToolUseID == "c1" && (r.Kind == StepSagaFail || r.Kind == StepToolResult) && strings.Contains(string(r.Result), "already ran") {
			t.Fatalf("charged %d times; the journal says the call already ran: %s", charged.Load(), r.Result)
		}
	}
}

// A retry-safe tool is outside T1's rule: running it again is safe, so a middleware that turns its
// success into an error leaves an ordinary recorded failure, not an unknown outcome.
func TestModel9_RetrySafeRejectedSuccessIsAnOrdinaryFailure(t *testing.T) {
	lookup := MustFunc("lookup", "", func(context.Context, struct{}) (string, error) { return "found", nil }, WithSafety(Safety{Idempotent: true}))
	check := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			if _, err := next(ctx, call); err != nil {
				return nil, err
			}
			return nil, errors.New("result failed validation")
		}
	})
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "lookup", `{}`), TextTurn("done"))
	if _, err := mustNew(m, store, WithTools(lookup), WithToolMiddleware(check)).Run(context.Background(), "r", UserText("go")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	r, ok := hasStep(t, store, "r", ToolResultStep("c1"))
	if !ok || !r.IsError || strings.Contains(string(r.Result), "outcome unknown") {
		t.Fatalf("result %s (recorded %v); want the middleware's failure recorded as is", r.Result, ok)
	}
}
