package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
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
// a ResumeHalt for the charge, which lists it as uncompensated.
func TestSaga_RollbackHaltsOnAnUnknownOutcome(t *testing.T) {
	var charged, refunded atomic.Int32
	pay := CompensatedFunc("pay", "charge the card", Safety{},
		func(ctx context.Context, _ struct{}) (string, error) {
			charged.Add(1) // the payment went through
			<-ctx.Done()   // cancelled while waiting for the response
			return "", ctx.Err()
		},
		func(context.Context, struct{}, string) error { refunded.Add(1); return nil })
	book := Func("book", "book the flight", Safety{}, func(context.Context, struct{}) (string, error) {
		time.Sleep(20 * time.Millisecond)
		return "", errors.New("no seats")
	})
	m := &sagaTurns{turns: [][][3]string{{{"p1", "pay", `{}`}, {"b1", "book", `{}`}}}}
	_, err := New(m, NewMemStore(), pay, book).RunSaga(context.Background(), "r1", "trip")
	var aborted *SagaAborted
	if !errors.As(err, &aborted) {
		t.Fatalf("err = %v, want *SagaAborted", err)
	}
	var halt *ResumeHalt
	if !errors.As(aborted.CompensateErr, &halt) || halt.ToolUseID != "p1" {
		t.Fatalf("SagaAborted = %+v; want the rollback to halt on the charge (p1), whose outcome is unknown (charged=%d refunded=%d)", aborted, charged.Load(), refunded.Load())
	}
}

// A sub-agent booked a hotel, then was cancelled mid-run when a sibling in the parent failed.
// The parent recorded no result for the sub-agent call, but the booking inside it stands, so
// the rollback must still reach into the sub-run and cancel it.
func TestSaga_RollbackReachesACancelledSubAgent(t *testing.T) {
	var booked, cancelled atomic.Int32
	hotel := CompensatedFunc("hotel", "book the hotel", Safety{},
		func(context.Context, struct{}) (string, error) { booked.Add(1); return "h-1", nil },
		func(context.Context, struct{}, string) error { cancelled.Add(1); return nil })
	subModel := &slowSecondTurn{first: [][3]string{{"h1", "hotel", `{}`}}, inSecond: make(chan struct{})}
	store := NewMemStore()
	clerk := New(subModel, store, hotel)
	fail := Func("visa", "apply for the visa", Safety{}, func(context.Context, struct{}) (string, error) {
		<-subModel.inSecond // fail once the sub-agent has booked and is thinking again
		return "", errors.New("visa refused")
	})
	m := &sagaTurns{turns: [][][3]string{{{"s1", "clerk", `{"task":"book"}`}, {"v1", "visa", `{}`}}}}
	_, err := New(m, store, SubAgent("clerk", "books things", clerk), fail).RunSaga(context.Background(), "r1", "trip")
	var aborted *SagaAborted
	if !errors.As(err, &aborted) {
		t.Fatalf("err = %v, want *SagaAborted", err)
	}
	if booked.Load() != 1 || cancelled.Load() != 1 {
		t.Fatalf("booked %d, cancelled %d (SagaAborted = %+v); the sub-agent's booking was not rolled back", booked.Load(), cancelled.Load(), aborted)
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
	set := Func("set_status", "mark the order approved", Safety{Idempotent: true}, func(context.Context, struct{}) (string, error) { return "ok", nil })
	fail := Func("ship", "ship it", Safety{}, func(context.Context, struct{}) (string, error) { return "", errors.New("no stock") })
	m := &sagaTurns{turns: [][][3]string{{{"s1", "set_status", `{}`}}, {{"x1", "ship", `{}`}}}}
	_, err := New(m, NewMemStore(), set, fail).RunSaga(context.Background(), "r1", "go")
	var aborted *SagaAborted
	if !errors.As(err, &aborted) || len(aborted.Uncompensated) != 1 || aborted.Uncompensated[0] != "set_status" {
		t.Fatalf("err = %v; want set_status reported as uncompensated", err)
	}
}

// Resolving the halted charge lets the rollback finish: ResolveHalt records that the charge
// went through, and the next RunSaga refunds it.
func TestSaga_ResolvedUnknownOutcomeIsCompensated(t *testing.T) {
	var refunded atomic.Int32
	pay := CompensatedFunc("pay", "charge the card", Safety{},
		func(ctx context.Context, _ struct{}) (string, error) { <-ctx.Done(); return "", ctx.Err() },
		func(_ context.Context, _ struct{}, receipt string) error {
			if receipt != "rcpt-9" {
				t.Errorf("refunded receipt %q, want the resolved rcpt-9", receipt)
			}
			refunded.Add(1)
			return nil
		})
	book := Func("book", "book the flight", Safety{}, func(context.Context, struct{}) (string, error) {
		time.Sleep(20 * time.Millisecond)
		return "", errors.New("no seats")
	})
	store := NewMemStore()
	m := &sagaTurns{turns: [][][3]string{{{"p1", "pay", `{}`}, {"b1", "book", `{}`}}}}
	a := New(m, store, pay, book)
	_, _ = a.RunSaga(context.Background(), "r1", "trip")
	if err := ResolveHalt(context.Background(), store, "r1", "p1", "rcpt-9", false); err != nil {
		t.Fatal(err)
	}
	_, err := a.RunSaga(context.Background(), "r1", "trip")
	var aborted *SagaAborted
	if !errors.As(err, &aborted) || aborted.CompensateErr != nil || refunded.Load() != 1 {
		t.Fatalf("after resolving: %v (refunds=%d); want a completed rollback that refunded once", err, refunded.Load())
	}
}

// A retry-safe write cut off by the abort is run again to learn its result, then undone.
func TestSaga_CutOffRetrySafeWriteIsUndone(t *testing.T) {
	var held, released atomic.Int32
	reserve := CompensatedFunc("reserve", "hold the seat", Safety{Idempotent: true},
		func(ctx context.Context, _ struct{}) (string, error) {
			if held.Add(1) == 1 {
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
		})
	book := Func("book", "book the flight", Safety{}, func(context.Context, struct{}) (string, error) {
		time.Sleep(20 * time.Millisecond)
		return "", errors.New("no seats")
	})
	m := &sagaTurns{turns: [][][3]string{{{"r1", "reserve", `{}`}, {"b1", "book", `{}`}}}}
	_, err := New(m, NewMemStore(), reserve, book).RunSaga(context.Background(), "run", "trip")
	var aborted *SagaAborted
	if !errors.As(err, &aborted) || released.Load() != 1 {
		t.Fatalf("err = %v, released = %d; want the held seat released", err, released.Load())
	}
}
