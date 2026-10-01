package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
)

// t3Charge is a compensable retry-safe write whose first call takes effect and cancels the drive.
func t3Charge(cancel *context.CancelFunc, calls, charged *atomic.Int32, later func() (string, error)) Tool {
	return CompensatedFunc("charge", "", Safety{Idempotent: true},
		func(ctx context.Context, _ struct{}) (string, error) {
			if calls.Add(1) == 1 {
				charged.Add(1)
				(*cancel)()
				return "", ctx.Err()
			}
			return later()
		},
		func(context.Context, struct{}, string) error { return nil })
}

// T3 with a middleware that denies the re-drive's call without calling next (ErrToolNotCalled):
// the earlier drive's attempt may have taken effect, so the step's outcome is unknown.
func TestRev117e_T3_RedriveDenialAfterEarlierAttempt(t *testing.T) {
	var calls, charged atomic.Int32
	ctx1, cancel := context.WithCancel(context.Background())
	defer cancel()
	charge := t3Charge(&cancel, &calls, &charged, func() (string, error) { return "ok", nil })
	var deny atomic.Bool
	mw := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			if deny.Load() {
				return nil, fmt.Errorf("denied by policy: %w", ErrToolNotCalled)
			}
			return next(ctx, call)
		}
	})
	st := NewMemStore()
	a := New(NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done")), st, charge).UseTool(mw)
	if _, err := a.RunSaga(ctx1, "r", "go"); err == nil {
		t.Fatal("first drive: want the cancellation")
	}
	deny.Store(true)
	_, err := a.RunSaga(context.Background(), "r", "go")
	var ab *SagaAborted
	if !errors.As(err, &ab) || !slices.Contains(ab.UnknownOutcome, "charge") {
		t.Fatalf("second drive = %v; want *SagaAborted listing charge as unknown", err)
	}
}

// Within one drive: a middleware runs the step, which succeeds, then runs it again (a retry on a
// result check), and the second invocation fails with the tool's own error. The first
// invocation's effect is in place, so the step's outcome is unknown.
func TestRev117e_T3_InDriveRetryAfterSuccess(t *testing.T) {
	var calls atomic.Int32
	charge := CompensatedFunc("charge", "", Safety{Idempotent: true},
		func(context.Context, struct{}) (string, error) {
			if calls.Add(1) == 1 {
				return "ok", nil
			}
			return "", errors.New("card declined")
		},
		func(context.Context, struct{}, string) error { return nil })
	retry := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			if _, err := next(ctx, call); err != nil {
				return nil, err
			}
			return next(ctx, call) // the result check wants a second look
		}
	})
	a := New(NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done")), NewMemStore(), charge).UseTool(retry)
	_, err := a.RunSaga(context.Background(), "r", "go")
	var ab *SagaAborted
	if !errors.As(err, &ab) || !slices.Contains(ab.UnknownOutcome, "charge") {
		t.Fatalf("RunSaga = %v; want *SagaAborted listing charge as unknown", err)
	}
}

// Precision: with no earlier attempt, the tool's own error is a known failure, and the rollback
// reports nothing unknown.
func TestRev117e_T3_FirstAttemptOwnFailureIsKnown(t *testing.T) {
	charge := CompensatedFunc("charge", "", Safety{Idempotent: true},
		func(context.Context, struct{}) (string, error) { return "", errors.New("card declined") },
		func(context.Context, struct{}, string) error { return nil })
	retry := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			if _, err := next(ctx, call); err == nil {
				return nil, errors.New("unexpected")
			}
			return next(ctx, call) // a retry after the tool's own failure
		}
	})
	a := New(NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done")), NewMemStore(), charge).UseTool(retry)
	_, err := a.RunSaga(context.Background(), "r", "go")
	var ab *SagaAborted
	if !errors.As(err, &ab) || len(ab.UnknownOutcome) != 0 {
		t.Fatalf("RunSaga = %v; want *SagaAborted with no unknown outcome", err)
	}
}

// The rollback reports a denied compensable retry-safe write whose arguments an earlier attempt
// journaled (its "may have begun" marker): the attempt may have taken effect.
func TestRev117e_T3_RollbackReportsDeniedStepWithEarlierAttempt(t *testing.T) {
	ctx := context.Background()
	st := NewMemStore()
	charge := CompensatedFunc("charge", "", Safety{Idempotent: true},
		func(context.Context, struct{}) (string, error) { return "ok", nil },
		func(context.Context, struct{}, string) error { return nil })
	a := New(NewScriptedModel(TextTurn("x")), st, charge)
	asst := &Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "c1", Name: "charge", Args: json.RawMessage(`{}`)}}}
	safety := Safety{Idempotent: true}
	for _, r := range []Record{
		{Name: "@llm/0", Kind: StepModel, Message: asst},
		{Name: sagaArgsStep("c1"), Kind: StepValue, Result: json.RawMessage(`{}`)},
		{Name: ToolResultStep("c1"), Kind: StepToolResult, ToolUseID: "c1", IsError: true, Result: json.RawMessage(`"tool call denied by human"`), Safety: &safety},
	} {
		if _, err := st.Do(ctx, "r", r.Name, func(context.Context) (Record, error) { return r, nil }); err != nil {
			t.Fatal(err)
		}
	}
	_, _, unknown, err := a.rollbackRun(ctx, "r", "r")
	if err != nil || !slices.Contains(unknown, "charge") {
		t.Fatalf("rollbackRun = unknown %v, %v; want charge reported", unknown, err)
	}
}

// The rollback re-runs a retry-safe write that a sibling's failure cut off, to learn the result to
// compensate, and a result check rejects every success: the re-run's outcome is unknown. The step
// is reported as unknown and the rollback reaches SagaAborted, as the live path does, rather than
// stopping on "learn the outcome" on every drive.
func TestRev117e_T4_RollbackRerunRejectedSuccessIsUnknown(t *testing.T) {
	var calls, refunded atomic.Int32
	started := make(chan struct{})
	charge := CompensatedFunc("charge", "", Safety{Idempotent: true},
		func(ctx context.Context, _ struct{}) (string, error) {
			if calls.Add(1) == 1 {
				close(started)
				<-ctx.Done() // cut off by the sibling's failure
				return "", ctx.Err()
			}
			return "ok", nil
		},
		func(context.Context, struct{}, string) error { refunded.Add(1); return nil })
	fail := Func("fail", "", Safety{}, func(context.Context, struct{}) (string, error) {
		<-started // charge is in flight
		return "", errors.New("declined")
	})
	check := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			res, err := next(ctx, call)
			if err == nil && call.Use.Name == "charge" {
				return nil, errors.New("result failed validation")
			}
			return res, err
		}
	})
	a := New(t4TwoCalls{}, NewMemStore(), charge, fail).UseTool(check)
	for drive := 1; drive <= 2; drive++ {
		_, err := a.RunSaga(context.Background(), "r", "go")
		var ab *SagaAborted
		if !errors.As(err, &ab) || !slices.Contains(ab.UnknownOutcome, "charge") || refunded.Load() != 0 {
			t.Fatalf("drive %d: RunSaga = %v (refunded %d); want *SagaAborted listing charge as unknown", drive, err, refunded.Load())
		}
	}
}

// t4TwoCalls calls charge and fail in its first turn, then answers.
type t4TwoCalls struct{}

func (t4TwoCalls) Stream(_ context.Context, req Request) (*Stream, error) {
	ch := make(chan Emit, 4)
	if len(req.Messages) <= 1 {
		ch <- Emit{Event: ToolCallDelta{Index: 0, ID: "c1", Name: "charge", ArgsFragment: json.RawMessage(`{}`)}}
		ch <- Emit{Event: ToolCallDelta{Index: 1, ID: "c2", Name: "fail", ArgsFragment: json.RawMessage(`{}`)}}
		ch <- Emit{Event: Finish{Reason: "tool_use"}}
	} else {
		ch <- Emit{Event: TextDelta{Text: "done"}}
		ch <- Emit{Event: Finish{Reason: "stop"}}
	}
	close(ch)
	return NewStream(ch), nil
}
