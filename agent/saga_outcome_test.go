package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// sagaTurns plays a fixed script: each turn is a list of tool calls (id, name, args), or, when
// empty, a final text answer.
type sagaTurns struct {
	turns [][][3]string
	n     atomic.Int32
}

func (m *sagaTurns) Stream(context.Context, Request) (*Stream, error) {
	i := int(m.n.Add(1)) - 1
	ch := make(chan Emit, 8)
	if i < len(m.turns) && len(m.turns[i]) > 0 {
		for k, c := range m.turns[i] {
			ch <- Emit{Event: ToolCallDelta{Index: k, ID: c[0], Name: c[1], ArgsFragment: []byte(c[2])}}
		}
		ch <- Emit{Event: Finish{Reason: "tool_use"}}
	} else {
		ch <- Emit{Event: TextDelta{Text: "done"}}
		ch <- Emit{Event: Finish{Reason: "stop"}}
	}
	close(ch)
	return NewStream(ch), nil
}

// A saga turn charges the card and books a flight at once. The booking fails; the charge had
// already gone through and was waiting for its response when the failure cancelled it. The
// rollback cannot know the charge's outcome, so it must not report a clean abort: it stops with
// a OutcomeUnknown for the charge, which lists it as uncompensated.
func TestSaga_RollbackHaltsOnAnUnknownOutcome(t *testing.T) {
	var charged, refunded atomic.Int32
	paying := make(chan struct{})
	pay := MustCompensatedFunc("pay", "charge the card", func(ctx context.Context, _ struct{}) (string, error) {
		charged.Add(1) // the payment went through
		close(paying)
		<-ctx.Done() // cancelled while waiting for the response
		return "", ctx.Err()
	},
		func(context.Context, struct{}, string) error { refunded.Add(1); return nil })
	book := MustFunc("book", "book the flight", func(context.Context, struct{}) (string, error) {
		<-paying // fail once the charge is in flight
		return "", errors.New("no seats")
	})
	m := &sagaTurns{turns: [][][3]string{{{"p1", "pay", `{}`}, {"b1", "book", `{}`}}}}
	_, err := mustNew(m, memJournal(), WithTools(pay, book)).Run(context.Background(), "r1", UserText("trip"), WithSaga())
	var aborted *SagaAborted
	if !errors.As(err, &aborted) {
		t.Fatalf("err = %v, want *SagaAborted", err)
	}
	var halt *OutcomeUnknown
	if !errors.As(aborted.CompensateErr, &halt) || halt.Op.ID != "p1" {
		t.Fatalf("SagaAborted = %+v; want the rollback to halt on the charge (p1), whose outcome is unknown (charged=%d refunded=%d)", aborted, charged.Load(), refunded.Load())
	}
}

// A sub-agent booked a hotel, then was cancelled mid-run when a sibling in the parent failed.
// The parent recorded no result for the sub-agent call, but the booking inside it stands, so
// the rollback must still reach into the sub-run and cancel it.
func TestSaga_RollbackReachesACancelledSubAgent(t *testing.T) {
	var booked, cancelled atomic.Int32
	hotel := MustCompensatedFunc("hotel", "book the hotel", func(context.Context, struct{}) (string, error) { booked.Add(1); return "h-1", nil },
		func(context.Context, struct{}, string) error { cancelled.Add(1); return nil })
	subModel := &slowSecondTurn{first: [][3]string{{"h1", "hotel", `{}`}}, inSecond: make(chan struct{})}
	store := memJournal()
	clerk := mustNew(subModel, store, WithTools(hotel))
	fail := MustFunc("visa", "apply for the visa", func(context.Context, struct{}) (string, error) {
		<-subModel.inSecond // fail once the sub-agent has booked and is thinking again
		return "", errors.New("visa refused")
	})
	m := &sagaTurns{turns: [][][3]string{{{"s1", "clerk", `{"task":"book"}`}, {"v1", "visa", `{}`}}}}
	_, err := mustNew(m, store, WithTools(MustSubAgent("clerk", "books things", clerk), fail)).Run(context.Background(), "r1", UserText("trip"), WithSaga())
	var aborted *SagaAborted
	if !errors.As(err, &aborted) {
		t.Fatalf("err = %v, want *SagaAborted", err)
	}
	if booked.Load() != 1 || cancelled.Load() != 1 {
		t.Fatalf("booked %d, cancelled %d (SagaAborted = %+v); the sub-agent's booking was not rolled back", booked.Load(), cancelled.Load(), aborted)
	}
}

// A rollback that stops on an unknown outcome inside a sub-agent's run names the root run to
// continue once the halt is resolved, as every other halt inside a sub-agent does: whether the
// parent's rollback reached into the sub-run, or the sub-agent's own saga rolled back.
func TestSaga_RollbackHaltInASubAgentNamesTheRoot(t *testing.T) {
	for _, failIn := range []string{"parent", "sub-agent"} {
		t.Run(failIn, func(t *testing.T) {
			charging := make(chan struct{})
			pay := MustCompensatedFunc("pay", "charge the card", func(ctx context.Context, _ struct{}) (string, error) {
				close(charging) // the payment went through
				<-ctx.Done()    // cancelled while waiting for the response
				return "", ctx.Err()
			},
				func(context.Context, struct{}, string) error { return nil })
			refuse := MustFunc("visa", "apply for the visa", func(context.Context, struct{}) (string, error) {
				<-charging
				return "", errors.New("visa refused")
			})
			store := memJournal()
			subTurn, parentTurn := [][3]string{{"p1", "pay", `{}`}}, [][3]string{{"s1", "clerk", `{"task":"pay"}`}}
			subTools, parentTools := []Tool{pay}, []Tool{}
			if failIn == "parent" {
				parentTurn = append(parentTurn, [3]string{"v1", "visa", `{}`})
				parentTools = append(parentTools, refuse)
			} else {
				subTurn = append(subTurn, [3]string{"v1", "visa", `{}`})
				subTools = append(subTools, refuse)
			}
			clerk := mustNew(&sagaTurns{turns: [][][3]string{subTurn}}, store, WithTools(subTools...))
			parentTools = append(parentTools, MustSubAgent("clerk", "pays", clerk))
			_, err := mustNew(&sagaTurns{turns: [][][3]string{parentTurn}}, store, WithTools(parentTools...)).Run(context.Background(), "r1", UserText("trip"), WithSaga())
			halt := rollbackHalt(err)
			if halt == nil {
				t.Fatalf("err = %v; want a rollback halted on p1", err)
			}
			if halt.RunID != "r1>s1" || halt.Op.ID != "p1" || halt.RootRunID != "r1" {
				t.Fatalf("halt on call %q in run %q names root %q; want p1 in r1>s1 with root r1", halt.Op.ID, halt.RunID, halt.RootRunID)
			}
		})
	}
}

// rollbackHalt returns the halt that stopped a saga rollback, following aborts whose cause is a
// sub-agent's abort, or nil.
func rollbackHalt(err error) *OutcomeUnknown {
	for {
		var ab *SagaAborted
		if !errors.As(err, &ab) {
			return nil
		}
		var halt *OutcomeUnknown
		if errors.As(ab.CompensateErr, &halt) {
			return halt
		}
		err = ab.Cause
	}
}

// slowSecondTurn calls its first-turn tools, then blocks its second turn until cancelled.
type slowSecondTurn struct {
	first    [][3]string
	calls    atomic.Int32
	inSecond chan struct{}
}

func (m *slowSecondTurn) Stream(ctx context.Context, _ Request) (*Stream, error) {
	if m.calls.Add(1) == 1 {
		ch := make(chan Emit, 8)
		for k, c := range m.first {
			ch <- Emit{Event: ToolCallDelta{Index: k, ID: c[0], Name: c[1], ArgsFragment: []byte(c[2])}}
		}
		ch <- Emit{Event: Finish{Reason: "tool_use"}}
		close(ch)
		return NewStream(ch), nil
	}
	close(m.inSecond)
	<-ctx.Done()
	return nil, ctx.Err()
}

// An Idempotent write is not free of effects: setting a status twice is harmless, but the status
// is still set. A completed one with no compensator is reported as uncompensated.
func TestSaga_IdempotentWriteWithoutCompensatorIsReported(t *testing.T) {
	set := MustFunc("set_status", "mark the order approved", func(context.Context, struct{}) (string, error) { return "ok", nil }, WithSafety(Safety{Idempotent: true}))
	fail := MustFunc("ship", "ship it", func(context.Context, struct{}) (string, error) { return "", errors.New("no stock") })
	m := &sagaTurns{turns: [][][3]string{{{"s1", "set_status", `{}`}}, {{"x1", "ship", `{}`}}}}
	_, err := mustNew(m, memJournal(), WithTools(set, fail)).Run(context.Background(), "r1", UserText("go"), WithSaga())
	var aborted *SagaAborted
	if !errors.As(err, &aborted) || len(aborted.Uncompensated) != 1 || aborted.Uncompensated[0] != "set_status" {
		t.Fatalf("err = %v; want set_status reported as uncompensated", err)
	}
}

// Resolving the halted charge lets the rollback finish: ResolveHalt records that the charge
// went through, and the next saga Run refunds it.
func TestSaga_ResolvedUnknownOutcomeIsCompensated(t *testing.T) {
	paying := make(chan struct{})
	var refunded atomic.Int32
	pay := MustCompensatedFunc("pay", "charge the card", func(ctx context.Context, _ struct{}) (string, error) {
		close(paying)
		<-ctx.Done()
		return "", ctx.Err()
	},
		func(_ context.Context, _ struct{}, receipt string) error {
			if receipt != "rcpt-9" {
				t.Errorf("refunded receipt %q, want the resolved rcpt-9", receipt)
			}
			refunded.Add(1)
			return nil
		})
	book := MustFunc("book", "book the flight", func(context.Context, struct{}) (string, error) {
		<-paying // fail once the charge is in flight
		return "", errors.New("no seats")
	})
	store := memJournal()
	m := &sagaTurns{turns: [][][3]string{{{"p1", "pay", `{}`}, {"b1", "book", `{}`}}}}
	a := mustNew(m, store, WithTools(pay, book))
	_, _ = a.Run(context.Background(), "r1", UserText("trip"), WithSaga())
	if err := ResolveHalt(context.Background(), store, HaltRef{RunID: "r1", Op: OpRef{Kind: OpTool, ID: "p1"}, Cause: HaltCrashed}, Outcome{Result: "rcpt-9", IsError: false}); err != nil {
		t.Fatal(err)
	}
	_, err := a.Run(context.Background(), "r1", UserText("trip"), WithSaga())
	var aborted *SagaAborted
	if !errors.As(err, &aborted) || aborted.CompensateErr != nil || refunded.Load() != 1 {
		t.Fatalf("after resolving: %v (refunds=%d); want a completed rollback that refunded once", err, refunded.Load())
	}
}

// A retry-safe write cut off by the abort is run again to learn its result, then undone.
func TestSaga_CutOffRetrySafeWriteIsUndone(t *testing.T) {
	var held, released atomic.Int32
	holding := make(chan struct{})
	reserve := MustCompensatedFunc("reserve", "hold the seat", func(ctx context.Context, _ struct{}) (string, error) {
		if held.Add(1) == 1 {
			close(holding)
			<-ctx.Done() // the first call is cut off by the abort
			return "", ctx.Err()
		}
		return "seat-12", nil // idempotent: the retry reports the same hold
	},
		func(_ context.Context, _ struct{}, seat string) error {
			if seat == "seat-12" {
				released.Add(1)
			}
			return nil
		}, WithSafety(Safety{Idempotent: true}))
	book := MustFunc("book", "book the flight", func(context.Context, struct{}) (string, error) {
		<-holding // fail once the hold is in flight
		return "", errors.New("no seats")
	})
	m := &sagaTurns{turns: [][][3]string{{{"r1", "reserve", `{}`}, {"b1", "book", `{}`}}}}
	_, err := mustNew(m, memJournal(), WithTools(reserve, book)).Run(context.Background(), "run", UserText("trip"), WithSaga())
	var aborted *SagaAborted
	if !errors.As(err, &aborted) || released.Load() != 1 {
		t.Fatalf("err = %v, released = %d; want the held seat released", err, released.Load())
	}
}

// A tool that cannot tell whether its side effect happened (its connection dropped after the
// request went out) reports ErrToolOutcomeUnknown. For a side effect the loop must record no
// result, stop the run, and halt the resume, as it does for a cancelled call; a journaled
// failure would tell the model the charge did not happen.
func TestToolOutcomeUnknown_SideEffectIsNotRecorded(t *testing.T) {
	store := memJournal()
	var charged int
	charge := MustFunc("charge", "charge the card", func(context.Context, struct{}) (string, error) {
		charged++
		return "", fmt.Errorf("gateway connection reset (%w)", ErrToolOutcomeUnknown)
	})
	m := &greedyModel{script: [][]Emit{toolTurn("c1", "charge", `{}`), toolTurn("c2", "charge", `{}`), textTurn("done")}}
	_, err := mustNew(m, store, WithTools(charge)).Run(context.Background(), "r1", UserText("pay"))
	if !errors.Is(err, ErrToolOutcomeUnknown) {
		t.Fatalf("run err = %v, want ErrToolOutcomeUnknown", err)
	}
	if rec, ok := hasStep(t, store, "r1", ToolResultStep("c1")); ok {
		t.Fatalf("the call's unknown outcome was journaled as %s (is_error=%v)", rec.Result, rec.IsError)
	}

	m2 := &greedyModel{script: [][]Emit{toolTurn("c2", "charge", `{}`), textTurn("done")}}
	_, err = mustNew(m2, store, WithTools(charge)).Run(context.Background(), "r1", UserText("pay"))
	var halt *OutcomeUnknown
	if !errors.As(err, &halt) || halt.Op.ID != "c1" {
		t.Fatalf("resume err = %v, want OutcomeUnknown for c1", err)
	}
	if halt.Cause != HaltCrashed || halt.Op != (OpRef{Kind: OpTool, ID: "c1", ToolName: "charge"}) {
		t.Fatalf("halt = %+v, want an OpTool halt on charge c1 with Cause %q (a marker found on resume)", halt, HaltCrashed)
	}
	if charged != 1 {
		t.Fatalf("charged %d times, want 1", charged)
	}
}

// A retry-safe tool may simply run again, so its unknown outcome is an ordinary failure the
// model sees, and the run carries on.
func TestToolOutcomeUnknown_RetrySafeToolIsAFailure(t *testing.T) {
	store := memJournal()
	lookup := MustFunc("lookup", "read a balance", func(context.Context, struct{}) (string, error) {
		return "", fmt.Errorf("connection reset (%w)", ErrToolOutcomeUnknown)
	}, WithSafety(Safety{ReadOnly: true}))
	m := &greedyModel{script: [][]Emit{toolTurn("c1", "lookup", `{}`), textTurn("done")}}
	if _, err := mustNew(m, store, WithTools(lookup)).Run(context.Background(), "r1", UserText("balance?")); err != nil {
		t.Fatalf("run err = %v, want the failure passed to the model", err)
	}
	if rec, ok := hasStep(t, store, "r1", ToolResultStep("c1")); !ok || !rec.IsError {
		t.Fatalf("recorded %+v (found=%v), want a failed result", rec, ok)
	}
}

// Tests for the findings of model 9 (spec/tla/toolcall), the tool-call state machine.

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
			t.Fatalf("saga Run = %v, want *SagaAborted or a halt on the step's outcome", err)
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

// Model 9, T3: a retry-safe saga step that changes state (Idempotent, a compensator) fires, and
// the run is cancelled before its result is recorded (nothing is recorded: a retry-safe call has
// no attempt marker). The run is driven again and the re-run fails with the tool's own error,
// having done nothing this time: a known saga failure. The rollback skips the step as one that
// made no change, though the first attempt's charge is in place, and lists it nowhere.
func TestT3_IdempotentSagaStepEarlierAttemptNotAccounted(t *testing.T) {
	var calls, charged, refunded atomic.Int32
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	charge := MustCompensatedFunc("charge", "", func(ctx context.Context, _ struct{}) (string, error) {
		if calls.Add(1) == 1 {
			charged.Add(1) // the effect takes place, then the run is cancelled
			cancel1()
			return "", ctx.Err()
		}
		return "", errors.New("card declined") // the tool's own error: nothing done this time
	},
		func(context.Context, struct{}, string) error { refunded.Add(1); return nil }, WithSafety(Safety{Idempotent: true}))
	st := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	a := mustNew(m, st, WithTools(charge))
	_, err := a.Run(ctx1, "r", UserText("go"), WithSaga())
	if err == nil {
		t.Fatalf("first drive: want the cancellation, got nil")
	}
	_, err = a.Run(context.Background(), "r", UserText("go"), WithSaga())
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
	charge := MustCompensatedFunc("charge", "", func(ctx context.Context, _ struct{}) (string, error) {
		<-release
		if refunded.Load() > 0 {
			refundedBeforeCharge.Store(true)
		}
		charged.Add(1)
		close(ran)
		return "ok", nil
	},
		func(context.Context, struct{}, string) error { refunded.Add(1); return nil }, WithSafety(Safety{Idempotent: true}))
	fail := MustFunc("fail", "", func(context.Context, struct{}) (string, error) { return "", errors.New("declined") })
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
	_, err := mustNew(m, memJournal(), WithTools(charge, fail), WithToolMiddleware(leak)).Run(context.Background(), "r", UserText("go"), WithSaga())
	close(release)
	<-ran
	var ab *SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("saga Run = %v, want *SagaAborted", err)
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
	charge := MustCompensatedFunc("charge", "", func(ctx context.Context, _ struct{}) (string, error) {
		if refunded.Load() > 0 {
			chargedAfterRefund.Store(true)
		}
		charged.Add(1)
		return "ok", nil
	},
		func(context.Context, struct{}, string) error { refunded.Add(1); return nil }, WithSafety(Safety{Idempotent: true}))
	fail := MustFunc("fail", "", func(context.Context, struct{}) (string, error) { return "", errors.New("declined") })
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
	_, err := mustNew(m, memJournal(), WithTools(charge, fail), WithToolMiddleware(leak)).Run(context.Background(), "r", UserText("go"), WithSaga())
	close(release)
	<-done
	var ab *SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("saga Run = %v, want *SagaAborted", err)
	}
	if chargedAfterRefund.Load() {
		t.Fatalf("the tool began after its chain returned and charged after the refund: charged %d, refunded %d, compensated %v", charged.Load(), refunded.Load(), ab.Compensated)
	}
}

// Model 9, T6: a drive is cancelled while a next left running is still in the retry-safe tool
// (nothing is recorded). The re-drive's middleware answers from a cache without reaching the
// tool, so its chain is not "reached" and the in-flight count is not read: the cache answer is
// recorded, a later step fails the saga, the rollback compensates, and the first invocation's
// effect lands after the compensation.
func TestT6_CacheAnswerWhileEarlierDriveInvocationRuns(t *testing.T) {
	release, ran := make(chan struct{}), make(chan struct{})
	var charged, refunded, drives atomic.Int32
	var chargedAfterRefund atomic.Bool
	charge := MustCompensatedFunc("charge", "", func(ctx context.Context, _ struct{}) (string, error) {
		<-release
		if refunded.Load() > 0 {
			chargedAfterRefund.Store(true)
		}
		charged.Add(1)
		close(ran)
		return "ok", nil
	},
		func(context.Context, struct{}, string) error { refunded.Add(1); return nil }, WithSafety(Safety{Idempotent: true}))
	fail := MustFunc("fail", "", func(context.Context, struct{}) (string, error) { return "", errors.New("declined") })
	ctx1, cancel1 := context.WithCancel(context.Background())
	leak := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			if call.Use.Name != "charge" {
				return next(ctx, call)
			}
			if drives.Add(1) == 1 {
				go next(context.WithoutCancel(ctx), call) //nolint:errcheck
				time.Sleep(20 * time.Millisecond)         // let next reach the tool
				cancel1()
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return json.RawMessage(`"ok"`), nil // a cache hit, without next
		}
	})
	st := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), ToolTurn("c2", "fail", `{}`), TextTurn("done"))
	a := mustNew(m, st, WithTools(charge, fail), WithToolMiddleware(leak))
	if _, err := a.Run(ctx1, "r", UserText("go"), WithSaga()); err == nil {
		t.Fatal("first drive: want the cancellation")
	}
	_, err := a.Run(context.Background(), "r", UserText("go"), WithSaga())
	close(release)
	<-ran
	var ab *SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("second drive = %v, want *SagaAborted", err)
	}
	if slices.Contains(ab.Compensated, "charge") && chargedAfterRefund.Load() {
		t.Fatalf("charge reported compensated, but the first drive's invocation charged after the refund: charged %d, refunded %d", charged.Load(), refunded.Load())
	}
}

// T1's saga case for a retry-safe step that mutates state (Idempotent, with a compensator): a
// middleware turns the step's success into an error (a result check, which the ToolMiddleware
// contract allows). The step fired, yet the saga failure is recorded as a known failure, so the
// rollback skips it as "made no change", and SagaAborted lists it nowhere.
func TestRev117e_IdempotentSagaStepRejectedSuccessIsNotAccounted(t *testing.T) {
	var charged, refunded atomic.Int32
	charge := MustCompensatedFunc("charge", "", func(context.Context, struct{}) (string, error) { charged.Add(1); return "ok", nil },
		func(context.Context, struct{}, string) error { refunded.Add(1); return nil }, WithSafety(Safety{Idempotent: true}))
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
		if !errors.Is(err, ErrToolOutcomeUnknown) {
			t.Fatalf("saga Run = %v, want *SagaAborted or a halt on the step's outcome", err)
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
	look := MustFunc("look", "", func(context.Context, struct{}) (string, error) { return "ok", nil }, WithSafety(safety))
	check := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			if _, err := next(ctx, call); err != nil {
				return nil, err
			}
			return nil, errors.New("result failed validation")
		}
	})
	m := NewScriptedModel(ToolTurn("c1", "look", `{}`), TextTurn("done"))
	_, err := mustNew(m, memJournal(), WithTools(look), WithToolMiddleware(check)).Run(context.Background(), "r", UserText("go"), WithSaga())
	var ab *SagaAborted
	if !errors.As(err, &ab) || len(ab.UnknownOutcome) != 0 {
		t.Fatalf("%+v: saga Run = %v; want a SagaAborted with no unknown outcome", safety, err)
	}
}

// t3Charge is a compensable retry-safe write whose first call takes effect and cancels the drive.
func t3Charge(cancel *context.CancelFunc, calls, charged *atomic.Int32, later func() (string, error)) Tool {
	return MustCompensatedFunc("charge", "", func(ctx context.Context, _ struct{}) (string, error) {
		if calls.Add(1) == 1 {
			charged.Add(1)
			(*cancel)()
			return "", ctx.Err()
		}
		return later()
	},
		func(context.Context, struct{}, string) error { return nil }, WithSafety(Safety{Idempotent: true}))
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
	st := memJournal()
	a := mustNew(
		NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done")),
		st,
		WithTools(charge),
		WithToolMiddleware(mw),
	)
	if _, err := a.Run(ctx1, "r", UserText("go"), WithSaga()); err == nil {
		t.Fatal("first drive: want the cancellation")
	}
	deny.Store(true)
	_, err := a.Run(context.Background(), "r", UserText("go"), WithSaga())
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
	charge := MustCompensatedFunc("charge", "", func(context.Context, struct{}) (string, error) {
		if calls.Add(1) == 1 {
			return "ok", nil
		}
		return "", errors.New("card declined")
	},
		func(context.Context, struct{}, string) error { return nil }, WithSafety(Safety{Idempotent: true}))
	retry := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			if _, err := next(ctx, call); err != nil {
				return nil, err
			}
			return next(ctx, call) // the result check wants a second look
		}
	})
	a := mustNew(
		NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done")),
		memJournal(),
		WithTools(charge),
		WithToolMiddleware(retry),
	)
	_, err := a.Run(context.Background(), "r", UserText("go"), WithSaga())
	var ab *SagaAborted
	if !errors.As(err, &ab) || !slices.Contains(ab.UnknownOutcome, "charge") {
		t.Fatalf("saga Run = %v; want *SagaAborted listing charge as unknown", err)
	}
}

// Precision: with no earlier attempt, the tool's own error is a known failure, and the rollback
// reports nothing unknown.
func TestRev117e_T3_FirstAttemptOwnFailureIsKnown(t *testing.T) {
	charge := MustCompensatedFunc("charge", "", func(context.Context, struct{}) (string, error) { return "", errors.New("card declined") },
		func(context.Context, struct{}, string) error { return nil }, WithSafety(Safety{Idempotent: true}))
	retry := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			if _, err := next(ctx, call); err == nil {
				return nil, errors.New("unexpected")
			}
			return next(ctx, call) // a retry after the tool's own failure
		}
	})
	a := mustNew(
		NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done")),
		memJournal(),
		WithTools(charge),
		WithToolMiddleware(retry),
	)
	_, err := a.Run(context.Background(), "r", UserText("go"), WithSaga())
	var ab *SagaAborted
	if !errors.As(err, &ab) || len(ab.UnknownOutcome) != 0 {
		t.Fatalf("saga Run = %v; want *SagaAborted with no unknown outcome", err)
	}
}

// The rollback reports a denied compensable retry-safe write whose arguments an earlier attempt
// journaled (its "may have begun" marker): the attempt may have taken effect.
func TestRev117e_T3_RollbackReportsDeniedStepWithEarlierAttempt(t *testing.T) {
	ctx := context.Background()
	st := memJournal()
	charge := MustCompensatedFunc("charge", "", func(context.Context, struct{}) (string, error) { return "ok", nil },
		func(context.Context, struct{}, string) error { return nil }, WithSafety(Safety{Idempotent: true}))
	a := mustNew(NewScriptedModel(TextTurn("x")), st, WithTools(charge))
	asst := &Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "c1", Name: "charge", Args: json.RawMessage(`{}`)}}}
	safety := Safety{Idempotent: true}
	for _, r := range []Record{
		{Name: "@llm/0", Kind: StepModel, Message: asst},
		{Name: sagaArgsStep("c1"), Kind: StepValue, Result: json.RawMessage(`{}`)},
		{Name: ToolResultStep("c1"), Kind: StepToolResult, ToolUseID: "c1", IsError: true, Result: json.RawMessage(`"tool call denied by human"`), Safety: &safety},
	} {
		if _, err := st.do(ctx, "r", r.Name, func(context.Context) (Record, error) { return r, nil }); err != nil {
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
	charge := MustCompensatedFunc("charge", "", func(ctx context.Context, _ struct{}) (string, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-ctx.Done() // cut off by the sibling's failure
			return "", ctx.Err()
		}
		return "ok", nil
	},
		func(context.Context, struct{}, string) error { refunded.Add(1); return nil }, WithSafety(Safety{Idempotent: true}))
	fail := MustFunc("fail", "", func(context.Context, struct{}) (string, error) {
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
	a := mustNew(t4TwoCalls{}, memJournal(), WithTools(charge, fail), WithToolMiddleware(check))
	for drive := 1; drive <= 2; drive++ {
		_, err := a.Run(context.Background(), "r", UserText("go"), WithSaga())
		var ab *SagaAborted
		if !errors.As(err, &ab) || !slices.Contains(ab.UnknownOutcome, "charge") || refunded.Load() != 0 {
			t.Fatalf("drive %d: saga Run = %v (refunded %d); want *SagaAborted listing charge as unknown", drive, err, refunded.Load())
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

// inflightEntries counts the in-flight map's entries, which must all be deleted at zero.
func inflightEntries() int {
	n := 0
	for i := range inflight {
		inflight[i].mu.Lock()
		n += len(inflight[i].n)
		inflight[i].mu.Unlock()
	}
	return n
}

// A sibling invocation overwrites the out word: one invocation of a retry-safe saga write is still
// running while another succeeds and the chain returns that success. The running one may take
// effect after the compensation, so the step's outcome is unknown.
func TestRev117e_T4_SiblingInvocationStillRunning(t *testing.T) {
	release, finished := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	inTool := make(chan struct{})
	charge := MustCompensatedFunc("charge", "", func(ctx context.Context, _ struct{}) (string, error) {
		if calls.Add(1) == 1 {
			close(inTool)
			<-release
		}
		return "ok", nil
	},
		func(context.Context, struct{}, string) error { return nil }, WithSafety(Safety{Idempotent: true}))
	hedge := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			if call.Use.Name != "charge" {
				return next(ctx, call)
			}
			go func() { next(context.WithoutCancel(ctx), call); close(finished) }() //nolint:errcheck
			<-inTool                                                                // the first invocation is in the tool
			return next(ctx, call)                                                  // a hedge: the second answers first
		}
	})
	fail := MustFunc("fail", "", func(context.Context, struct{}) (string, error) { return "", errors.New("declined") })
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), ToolTurn("c2", "fail", `{}`), TextTurn("done"))
	_, err := mustNew(m, memJournal(), WithTools(charge, fail), WithToolMiddleware(hedge)).Run(context.Background(), "r", UserText("go"), WithSaga())
	close(release)
	<-finished
	var ab *SagaAborted
	if !errors.As(err, &ab) || !slices.Contains(ab.UnknownOutcome, "charge") || slices.Contains(ab.Compensated, "charge") {
		t.Fatalf("saga Run = %v; want charge listed as unknown and not compensated", err)
	}
	if n := inflightEntries(); n != 0 {
		t.Fatalf("%d in-flight entries left", n)
	}
}

// An invocation an earlier, cancelled drive left running in this process: the re-drive's call
// succeeds while it still runs, so the step's outcome is unknown.
func TestRev117e_T4_EarlierDriveInvocationStillRunning(t *testing.T) {
	release, finished := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	ctx1, cancel := context.WithCancel(context.Background())
	charge := MustCompensatedFunc("charge", "", func(ctx context.Context, _ struct{}) (string, error) {
		if calls.Add(1) == 1 {
			cancel()
			<-release // ignores its context: still running after the drive ends
			close(finished)
			return "", ctx.Err()
		}
		return "ok", nil
	},
		func(context.Context, struct{}, string) error { return nil }, WithSafety(Safety{Idempotent: true}))
	leak := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			if call.Use.Name != "charge" || calls.Load() > 0 {
				return next(ctx, call)
			}
			res := make(chan error, 1)
			go func() { _, err := next(ctx, call); res <- err }()
			<-ctx.Done() // the drive is cancelled; its invocation is left running
			return nil, ctx.Err()
		}
	})
	fail := MustFunc("fail", "", func(context.Context, struct{}) (string, error) { return "", errors.New("declined") })
	st := memJournal()
	a := mustNew(
		NewScriptedModel(ToolTurn("c1", "charge", `{}`), ToolTurn("c2", "fail", `{}`), TextTurn("done")),
		st,
		WithTools(charge, fail),
		WithToolMiddleware(leak),
	)
	if _, err := a.Run(ctx1, "r", UserText("go"), WithSaga()); err == nil {
		t.Fatal("first drive: want the cancellation")
	}
	_, err := a.Run(context.Background(), "r", UserText("go"), WithSaga())
	close(release)
	<-finished
	var ab *SagaAborted
	if !errors.As(err, &ab) || !slices.Contains(ab.UnknownOutcome, "charge") || slices.Contains(ab.Compensated, "charge") {
		t.Fatalf("second drive = %v; want charge listed as unknown and not compensated", err)
	}
}

// The rollback's re-run of a retry-safe write is answered by a middleware without reaching the
// tool (a cache): that is not the step's result, so the step is reported as unknown, not
// compensated on the cached answer.
func TestRev117e_T4_RollbackRerunCacheAnswerIsUnknown(t *testing.T) {
	var calls, refunded atomic.Int32
	started := make(chan struct{})
	charge := MustCompensatedFunc("charge", "", func(ctx context.Context, _ struct{}) (string, error) {
		calls.Add(1)
		close(started)
		<-ctx.Done() // cut off by the sibling's failure
		return "", ctx.Err()
	},
		func(context.Context, struct{}, string) error { refunded.Add(1); return nil }, WithSafety(Safety{Idempotent: true}))
	fail := MustFunc("fail", "", func(context.Context, struct{}) (string, error) {
		<-started
		return "", errors.New("declined")
	})
	cache := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			if call.Use.Name == "charge" && calls.Load() > 0 {
				return json.RawMessage(`"cached"`), nil
			}
			return next(ctx, call)
		}
	})
	_, err := mustNew(t4TwoCalls{}, memJournal(), WithTools(charge, fail), WithToolMiddleware(cache)).Run(context.Background(), "r", UserText("go"), WithSaga())
	var ab *SagaAborted
	if !errors.As(err, &ab) || !slices.Contains(ab.UnknownOutcome, "charge") || refunded.Load() != 0 {
		t.Fatalf("saga Run = %v (refunded %d); want charge listed as unknown and not compensated", err, refunded.Load())
	}
}

// secondArgsGate holds the second write of c1's saga-arguments record (the second invocation's)
// until release is closed. Each write starts with a read of the record (the journal reads a step
// before it runs it), so the gate is on the second Get.
type secondArgsGate struct {
	*MemStore
	n                int32
	writing, release chan struct{}
}

func (s *secondArgsGate) Get(ctx context.Context, runID, name string) (Entry, bool, error) {
	if name == sagaArgsStep("c1") && atomic.AddInt32(&s.n, 1) == 2 {
		close(s.writing)
		<-s.release
	}
	return s.MemStore.Get(ctx, runID, name)
}

// T5's window: a second invocation of a retry-safe write reaches the call while the chain runs, and
// is held until the chain has returned. The tool was begun by the first invocation, so only the
// closed check stops the second from beginning it again.
func TestRev117e_T5_ReachedBeforeCloseBeginsAfter(t *testing.T) {
	var charges atomic.Int32
	charge := MustCompensatedFunc("charge", "", func(context.Context, struct{}) (string, error) { charges.Add(1); return "ok", nil },
		func(context.Context, struct{}, string) error { return nil }, WithSafety(Safety{Idempotent: true}))
	store := &secondArgsGate{MemStore: NewMemStore(), writing: make(chan struct{}), release: make(chan struct{})}
	j := mustJournal(store)
	second := make(chan error, 1)
	leak := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			res, err := next(ctx, call)
			go func() { _, e := next(context.WithoutCancel(ctx), call); second <- e }()
			<-store.writing // the second invocation reached the call and is held
			return res, err
		}
	})
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), TextTurn("done"))
	if _, err := mustNew(m, j, WithTools(charge), WithToolMiddleware(leak)).Run(context.Background(), "r", UserText("go"), WithSaga()); err != nil {
		t.Fatal(err)
	}
	close(store.release)
	if err := <-second; !errors.Is(err, ErrToolNotCalled) || charges.Load() != 1 {
		t.Fatalf("second invocation = %v, charges %d; want it refused and one charge", err, charges.Load())
	}
	if n := inflightEntries(); n != 0 {
		t.Fatalf("%d in-flight entries left after the refused invocation", n)
	}
}

// The error path: a retry-safe saga write (no compensator, so no arguments record) whose re-drive
// fails with the tool's own error while an invocation the cancelled first drive left behind is
// still running. That invocation may yet take effect, so the step's outcome is unknown.
func TestRev117e_T4_OwnFailureWhileEarlierInvocationRuns(t *testing.T) {
	release, finished := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	ctx1, cancel := context.WithCancel(context.Background())
	write := MustFunc("write", "", func(ctx context.Context, _ struct{}) (string, error) {
		if calls.Add(1) == 1 {
			cancel()
			<-release
			close(finished)
			return "", ctx.Err()
		}
		return "", errors.New("rejected")
	}, WithSafety(Safety{Idempotent: true}))
	leak := ToolMiddleware(func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			if calls.Load() > 0 {
				return next(ctx, call)
			}
			go next(ctx, call) //nolint:errcheck
			<-ctx.Done()
			return nil, ctx.Err()
		}
	})
	a := mustNew(
		NewScriptedModel(ToolTurn("c1", "write", `{}`), TextTurn("done")),
		memJournal(),
		WithTools(write),
		WithToolMiddleware(leak),
	)
	if _, err := a.Run(ctx1, "r", UserText("go"), WithSaga()); err == nil {
		t.Fatal("first drive: want the cancellation")
	}
	_, err := a.Run(context.Background(), "r", UserText("go"), WithSaga())
	close(release)
	<-finished
	var ab *SagaAborted
	if !errors.As(err, &ab) || !slices.Contains(ab.UnknownOutcome, "write") {
		t.Fatalf("second drive = %v; want write listed as unknown", err)
	}
}

// The in-flight count is per store: a run of the same ID and call on another store, still in its
// tool, does not make this run's result unknown.
func TestRev117e_T4_InflightIsPerStore(t *testing.T) {
	release, inTool, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	slow := MustFunc("send", "", func(context.Context, struct{}) (string, error) {
		close(inTool)
		<-release
		return "ok", nil
	})
	fast := MustFunc("send", "", func(context.Context, struct{}) (string, error) { return "ok", nil })
	m := func() Model { return NewScriptedModel(ToolTurn("c1", "send", `{}`), TextTurn("done")) }
	go func() {
		_, err := mustNew(m(), memJournal(), WithTools(slow)).Run(context.Background(), "r", UserText("go"))
		done <- err
	}()
	<-inTool
	_, err := mustNew(m(), memJournal(), WithTools(fast)).Run(context.Background(), "r", UserText("go"))
	close(release)
	if err != nil {
		t.Fatalf("Run on another store = %v; a call of the same run ID elsewhere is not this call", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Baseline for R117-2: the same gap, before P12, for a retry-safe tool that itself says its
// outcome is unknown (#57), in a saga.
func TestR117Base_SagaUnknownOutcomeOfARetrySafeWrite(t *testing.T) {
	var committed, undone atomic.Int32
	hold := MustCompensatedFunc("hold", "", func(ctx context.Context, _ struct{}) (string, error) {
		committed.Add(1)
		return "", fmt.Errorf("connection reset: %w", ErrToolOutcomeUnknown)
	},
		func(context.Context, struct{}, string) error { undone.Add(1); return nil }, WithSafety(Safety{Idempotent: true}))
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "hold", `{}`), TextTurn("done"))
	_, err := mustNew(m, store, WithTools(hold)).Run(context.Background(), "s1", UserText("book"), WithSaga())
	var ab *SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("saga Run: err = %v, want *SagaAborted", err)
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
	hold := MustCompensatedFunc("hold", "", func(ctx context.Context, _ struct{}) (string, error) {
		return "", fmt.Errorf("connection reset: %w", ErrToolOutcomeUnknown)
	},
		func(context.Context, struct{}, string) error { return nil }, WithSafety(Safety{Idempotent: true}))
	store := memJournal()
	sub := mustNew(NewScriptedModel(ToolTurn("h1", "hold", `{}`), TextTurn("done")), store, WithTools(hold))
	parent := mustNew(
		NewScriptedModel(ToolTurn("p1", "delegate", `{"task":"x"}`), TextTurn("done")),
		store,
		WithTools(MustSubAgent("delegate", "", sub)),
	)
	_, err := parent.Run(context.Background(), "root", UserText("go"), WithSaga())
	var ab *SagaAborted
	if !errors.As(err, &ab) || len(ab.UnknownOutcome) != 1 || ab.UnknownOutcome[0] != "hold" {
		t.Fatalf("saga Run = %v; want *SagaAborted naming hold as unknown", err)
	}
}

// The tests here give a tool a short WithTimeout and need the call to reach the tool before the
// deadline. On the wall clock it may not: under load the deadline can pass while the call is
// dispatched, and the base handler then rightly refuses it as not started (errDoneBeforeCall),
// so the tool never runs. A synctest bubble makes it deterministic: its clock advances only once
// every goroutine in it is durably blocked, so the deadline passes only after the tool has begun
// and blocks on its context. A tool that waits out its deadline must therefore block (on
// ctx.Done or time.Sleep), never spin on time.Now.

// R117-2. WithTimeout says an error returned after the deadline "has an unknown outcome", and a
// retry-safe tool records it. In a saga that record is the StepSagaFail of the step that aborted
// the saga, and rollback skips that step as one that "made no change". A retry-safe compensable
// write whose commit landed before it was cut off is then neither compensated nor reported:
// SagaAborted says nothing was left behind.
func TestR117_SagaLateErrorOfARetrySafeWriteIsNeitherUndoneNorReported(t *testing.T) {
	synctest.Test(t, testR117SagaLateErrorOfARetrySafeWrite)
}

func testR117SagaLateErrorOfARetrySafeWrite(t *testing.T) {
	var committed, undone atomic.Int32
	hold := MustCompensatedFunc("hold", "", func(ctx context.Context, _ struct{}) (string, error) {
		committed.Add(1) // the provider commits the hold ...
		<-ctx.Done()     // ... and its reply arrives after the deadline
		return "", ctx.Err()
	},
		func(context.Context, struct{}, string) error { undone.Add(1); return nil }, WithSafety(Safety{Idempotent: true}),
		WithTimeout(time.Millisecond))
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "hold", `{}`), TextTurn("done"))
	_, err := mustNew(m, store, WithTools(hold)).Run(context.Background(), "s1", UserText("book"), WithSaga())
	var ab *SagaAborted
	if !errors.As(err, &ab) {
		t.Fatalf("saga Run: err = %v, want *SagaAborted", err)
	}
	if committed.Load() != 1 || undone.Load() != 0 || len(ab.UnknownOutcome) != 1 || ab.UnknownOutcome[0] != "hold" {
		t.Fatalf("a write with an unknown outcome: committed %d, undone %d, compensated %q, uncompensated %q, unknown %q; want it committed once, reported as unknown, not compensated",
			committed.Load(), undone.Load(), ab.Compensated, ab.Uncompensated, ab.UnknownOutcome)
	}
}

// R117-3 (documented behaviour). NextOnceKey is scoped to one tool call: a retry is a new call
// (a new tool-use ID) and gets a new key, so a retry-safe tool whose late error was recorded, and
// which the model then calls again, applies its effect under two keys. Dedup across the model's
// retries needs a business key derived from the arguments (see Safety.Idempotent and NextOnceKey).
func TestR117_NextOnceKeyIsScopedToOneCall(t *testing.T) {
	synctest.Test(t, testR117NextOnceKeyIsScopedToOneCall)
}

func testR117NextOnceKeyIsScopedToOneCall(t *testing.T) {
	var mu sync.Mutex
	applied := map[string]bool{}
	post := MustFunc("post", "", func(ctx context.Context, _ struct{}) (string, error) {
		k := NextOnceKey(ctx)
		mu.Lock()
		applied[k] = true
		mu.Unlock()
		<-ctx.Done()
		return "", ctx.Err()
	}, WithSafety(Safety{Idempotent: true}), WithTimeout(time.Millisecond))
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "post", `{}`), ToolTurn("c2", "post", `{}`), TextTurn("done"))
	if _, err := mustNew(m, store, WithTools(post)).Run(context.Background(), "r1", UserText("post it")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(applied) != 2 || !applied[SubRunID("r1", "c1")+"#0"] || !applied[SubRunID("r1", "c2")+"#0"] {
		t.Fatalf("keys %v; want two distinct keys, one per call: a retry is a new call", applied)
	}
	// Both calls reached the tool and ran past their deadline: each recorded a late error (an
	// unknown outcome, recorded since the tool is retry-safe), not a call refused as not started.
	for _, id := range []string{"c1", "c2"} {
		rec, ok := hasStep(t, store, "r1", ToolResultStep(id))
		if !ok || !rec.IsError || !strings.Contains(string(rec.Result), "after its 1ms timeout") {
			t.Fatalf("%s: result %s (recorded %v, is_error %v); want the late error recorded", id, rec.Result, ok, rec.IsError)
		}
	}
}

// slowArgsStore holds the write of c1's accepted arguments until its context is cancelled (a
// slow store under a saga sibling's failure), and reports when that write has begun. The write
// begins with its read of the record (the journal reads a step before it runs it), so the hook is
// on that Get, which then fails with the context's error: nothing is written.
type slowArgsStore struct {
	*MemStore
	writing chan struct{}
}

func (s slowArgsStore) Get(ctx context.Context, runID, name string) (Entry, bool, error) {
	if name == sagaArgsStep("c1") {
		close(s.writing)
		<-ctx.Done()
		return Entry{}, false, ctx.Err()
	}
	return s.MemStore.Get(ctx, runID, name)
}

// F4. The accepted-arguments write now comes after enterTool, so its failure counts as reached.
// When that write is cut off by the saga's own cancellation (a sibling step failed), the tool was
// provably never called, yet its claim stays live: the rollback cannot tell the step never ran and
// stops on an unknown outcome for it. Before round 4 the refusal recorded the claim as never
// started and the saga aborted cleanly.
func TestRev117d_CancelledArgsWriteLeavesRollbackOnUnknownOutcome(t *testing.T) {
	var charges atomic.Int32
	charge := MustCompensatedFunc("charge", "charge the card", func(context.Context, chargeArgs) (string, error) { charges.Add(1); return "ok", nil },
		func(context.Context, chargeArgs, string) error { return nil })
	store := slowArgsStore{NewMemStore(), make(chan struct{})}
	j := mustJournal(store)
	book := MustFunc("book", "book the flight", func(context.Context, struct{}) (string, error) {
		<-store.writing // fails while charge's accepted arguments are being written
		return "", errors.New("no seats")
	})
	m := &sagaTurns{turns: [][][3]string{{{"c1", "charge", `{"amount":5}`}, {"b1", "book", `{}`}}}}
	_, err := mustNew(m, j, WithTools(charge, book), WithToolMiddleware(scaleCharge)).Run(context.Background(), "r", UserText("trip"), WithSaga())
	var aborted *SagaAborted
	if !errors.As(err, &aborted) {
		t.Fatalf("saga Run = %v, want *SagaAborted", err)
	}
	if charges.Load() != 0 {
		t.Fatalf("setup: the charge ran %d times", charges.Load())
	}
	if aborted.CompensateErr != nil || len(aborted.UnknownOutcome) != 0 {
		t.Fatalf("the charge tool was never called (0 calls), yet the rollback stops on it: CompensateErr %v, UnknownOutcome %v", aborted.CompensateErr, aborted.UnknownOutcome)
	}
}

// State machine stress: enterTool, a refusal's CAS and closeCall race from every non-terminal
// start. Invariant: closeCall's verdict is the final state, and a call whose enterTool returned
// true is never closed as not reached (closed or refusedClosed).
func TestRev117d_CallStateRace(t *testing.T) {
	for i := 0; i < 200000; i++ {
		var st atomic.Int32
		st.Store([]int32{callOpen, callRefused}[i%2])
		var entered atomic.Bool
		var verdict int32
		done := make(chan struct{}, 3)
		go func() { entered.Store(enterTool(&st)); done <- struct{}{} }()
		go func() { st.CompareAndSwap(callOpen, callRefused); done <- struct{}{} }()
		go func() { verdict = closeCall(&st); done <- struct{}{} }()
		<-done
		<-done
		<-done
		final := st.Load()
		if final == callReachedClosed { // a reached call the chain closed
			final = callReached
		}
		if entered.Load() != (final == callReached) || (verdict != final && verdict != callReached && final == callReached) {
			t.Fatalf("iter %d: entered %v, verdict %d, final %d", i, entered.Load(), verdict, final)
		}
		if entered.Load() && verdict != callReached && verdict != callOpen && verdict != callRefused {
			t.Fatalf("iter %d: entered the tool but closeCall said %d", i, verdict)
		}
	}
}

// safetyWrap is a wrapper that overrides the Safety of the sub-agent it wraps.
type safetyWrap struct{ Tool }

func (w safetyWrap) Spec() ToolSpec { s := w.Tool.Spec(); s.Safety = Safety{}; return s }
func (w safetyWrap) Unwrap() Tool   { return w.Tool }

// (c) New refuses a wrapper that overrides the Safety of a sub-agent it wraps, as SubAgent refuses
// WithSafety: the sub-run's own calls carry their safety.
func TestRev117d_NewRefusesSafetyOverrideOverASubAgent(t *testing.T) {
	sub := mustNew(NewScriptedModel(TextTurn("x")), memJournal())
	var calls atomic.Int32
	a, err := New(
		&countingModel{n: &calls},
		memJournal(),
		WithTools(safetyWrap{MustSubAgent("delegate", "", sub)}),
	)
	if !errors.Is(err, ErrConfig) || a != nil || calls.Load() != 0 {
		t.Fatalf("New = %v, %v after %d model calls; want nil and ErrConfig before any", a, err, calls.Load())
	}
}

// gatedArgsStore holds the write of c1's accepted arguments until release is closed, and reports
// when that write has begun (at its read of the record, which the journal makes first).
type gatedArgsStore struct {
	*MemStore
	writing, release chan struct{}
}

func (s gatedArgsStore) Get(ctx context.Context, runID, name string) (Entry, bool, error) {
	if name == sagaArgsStep("c1") {
		close(s.writing)
		<-s.release
	}
	return s.MemStore.Get(ctx, runID, name)
}

// F4, the other half: an invocation that reached the call but had not begun the tool when the chain
// returned finds the call sealed when it gets there, and refuses: the loop counted the call as not
// called, and it stays so.
func TestRev117d_SealedCallIsNeverBegun(t *testing.T) {
	var charges atomic.Int32
	charge := MustCompensatedFunc("charge", "", func(context.Context, chargeArgs) (string, error) { charges.Add(1); return "ok", nil },
		func(context.Context, chargeArgs, string) error { return nil })
	store := gatedArgsStore{NewMemStore(), make(chan struct{}), make(chan struct{})}
	j := mustJournal(store)
	leaked := make(chan error, 1)
	leak := func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			call.Use.Args = json.RawMessage(`{"amount":500}`) // rewritten, so the saga journals it
			go func() {
				_, err := next(context.WithoutCancel(ctx), call)
				leaked <- err
			}()
			<-store.writing                           // the invocation has reached the call and is writing the arguments
			return nil, errors.New("gave up waiting") // the chain returns: the loop seals the call
		}
	}
	m := &sagaTurns{turns: [][][3]string{{{"c1", "charge", `{"amount":5}`}}}}
	_, _ = mustNew(m, j, WithTools(charge), WithToolMiddleware(leak)).Run(context.Background(), "r", UserText("trip"), WithSaga())
	close(store.release)
	if err := <-leaked; !errors.Is(err, ErrToolNotCalled) || charges.Load() != 0 {
		t.Fatalf("the invocation that outlived the chain: %v, charges %d; want it refused, the tool never called", err, charges.Load())
	}
}
