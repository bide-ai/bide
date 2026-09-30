package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

type chargeArgs struct {
	Amount int `json:"amount"`
}

// scaleCharge is a tool middleware that rewrites a charge's amount (say, dollars to cents) before
// the tool runs: the tool charges 100 times what the model asked for.
func scaleCharge(next ToolHandler) ToolHandler {
	return func(ctx context.Context, tu ToolUse) (json.RawMessage, error) {
		if tu.Name == "charge" {
			var in chargeArgs
			if err := json.Unmarshal(tu.Args, &in); err == nil {
				tu.Args, _ = json.Marshal(chargeArgs{Amount: in.Amount * 100})
			}
		}
		return next(ctx, tu)
	}
}

// rewrittenChargeSaga runs a saga that charges (through scaleCharge) and then fails, so the charge is
// compensated. It returns what was charged and what was refunded.
func rewrittenChargeSaga(t *testing.T, store Durable, safety Safety, mw ...ToolMiddleware) (charged, refunded int, err error) {
	t.Helper()
	charge := CompensatedFunc("charge", "charge the card", safety,
		func(_ context.Context, in chargeArgs) (string, error) { charged = in.Amount; return "ok", nil },
		func(_ context.Context, in chargeArgs, _ string) error { refunded = in.Amount; return nil })
	fail := Func("book", "book the flight", Safety{}, func(context.Context, struct{}) (string, error) {
		return "", errors.New("no seats")
	})
	m := NewScriptedModel(ToolTurn("c1", "charge", `{"amount":5}`), ToolTurn("b1", "book", `{}`), TextTurn("done"))
	_, err = New(m, store, charge, fail).UseTool(mw...).RunSaga(context.Background(), "r", "trip")
	return charged, refunded, err
}

// Compensation undoes what the tool did: the arguments the tool accepted, after tool middleware.
// It used to take the model's arguments from the journal, so a middleware that rewrote a charge
// from 5 to 500 charged 500 and refunded 5.
func TestSaga_CompensatesTheArgumentsTheToolAccepted(t *testing.T) {
	for _, safety := range []Safety{{}, {Idempotent: true}} {
		charged, refunded, err := rewrittenChargeSaga(t, NewMemStore(), safety, scaleCharge)
		var aborted *SagaAborted
		if !errors.As(err, &aborted) || aborted.CompensateErr != nil {
			t.Fatalf("safety %+v: RunSaga = %v, want a clean *SagaAborted", safety, err)
		}
		if charged != 500 || refunded != 500 {
			t.Errorf("safety %+v: charged %d, refunded %d; want the refund to undo the 500 charged", safety, charged, refunded)
		}
	}
}

// The accepted arguments are journaled only when they matter: in a saga, for a compensable call,
// when a middleware changed them. Without a rewrite the journal is as before, and compensation
// reads the model's arguments, which are the ones the tool accepted.
func TestSaga_AcceptedArgumentsAreJournaledOnlyWhenRewritten(t *testing.T) {
	store := NewMemStore()
	charged, refunded, _ := rewrittenChargeSaga(t, store, Safety{})
	if charged != 5 || refunded != 5 {
		t.Fatalf("charged %d, refunded %d; want 5 and 5", charged, refunded)
	}
	recs, _ := store.History(context.Background(), "r")
	for _, r := range recs {
		if r.Name == sagaArgsStep("c1") {
			t.Fatalf("a call whose arguments no middleware changed journaled them: %+v", r)
		}
	}

	// A Run (not a saga) never compensates, so it journals no arguments either.
	var ran atomic.Int32
	charge := CompensatedFunc("charge", "charge the card", Safety{},
		func(context.Context, chargeArgs) (string, error) { ran.Add(1); return "ok", nil },
		func(context.Context, chargeArgs, string) error { return nil })
	store = NewMemStore()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{"amount":5}`), TextTurn("done"))
	if _, err := New(m, store, charge).UseTool(scaleCharge).Run(context.Background(), "r", "go"); err != nil || ran.Load() != 1 {
		t.Fatalf("Run = %v after %d charges", err, ran.Load())
	}
	recs, _ = store.History(context.Background(), "r")
	for _, r := range recs {
		if r.Name == sagaArgsStep("c1") {
			t.Fatalf("a Run journaled a call's arguments: %+v", r)
		}
	}
}

// The accepted arguments are journaled before the side effect fires, so a charge whose outcome
// became unknown (the resume halts on it) is, once an operator resolves the halt and the saga
// later aborts, refunded by what the tool was given, not by what the model asked for.
func TestSaga_ResolvedUnknownOutcomeCompensatesTheAcceptedArguments(t *testing.T) {
	var charged, refunded atomic.Int32
	store := NewMemStore()
	build := func() *Agent {
		charge := CompensatedFunc("charge", "charge the card", Safety{},
			func(_ context.Context, in chargeArgs) (string, error) {
				charged.Store(int32(in.Amount))
				return "", ErrToolOutcomeUnknown // the connection dropped after the request went out
			},
			func(_ context.Context, in chargeArgs, _ string) error { refunded.Store(int32(in.Amount)); return nil })
		fail := Func("book", "book the flight", Safety{}, func(context.Context, struct{}) (string, error) {
			return "", errors.New("no seats")
		})
		m := NewScriptedModel(ToolTurn("c1", "charge", `{"amount":5}`), ToolTurn("b1", "book", `{}`), TextTurn("done"))
		return New(m, store, charge, fail).UseTool(scaleCharge)
	}
	if _, err := build().RunSaga(context.Background(), "r", "trip"); !errors.Is(err, ErrToolOutcomeUnknown) {
		t.Fatalf("RunSaga = %v, want ErrToolOutcomeUnknown", err)
	}
	_, err := build().RunSaga(context.Background(), "r", "trip") // the resume halts on the charge
	var halt *ResumeHalt
	if !errors.As(err, &halt) || halt.ToolUseID != "c1" {
		t.Fatalf("resumed RunSaga = %v, want a ResumeHalt on the charge", err)
	}
	if err := ResolveHalt(context.Background(), store, halt.RunID, halt.ToolUseID, "charged (operator-confirmed)", false); err != nil {
		t.Fatalf("ResolveHalt: %v", err)
	}
	_, err = build().RunSaga(context.Background(), "r", "trip")
	var aborted *SagaAborted
	if !errors.As(err, &aborted) || aborted.CompensateErr != nil {
		t.Fatalf("resumed RunSaga = %v, want a clean *SagaAborted", err)
	}
	if charged.Load() != 500 || refunded.Load() != 500 {
		t.Fatalf("charged %d, refunded %d; want the refund to undo the 500 charged", charged.Load(), refunded.Load())
	}
}

// A journal written before accepted arguments were journaled has none: compensation falls back to
// the model's arguments, as it always did.
func TestSaga_JournalWithoutAcceptedArgumentsUsesTheModelArguments(t *testing.T) {
	// No middleware: nothing is journaled, which is exactly what an older journal looks like.
	charged, refunded, err := rewrittenChargeSaga(t, NewMemStore(), Safety{})
	var aborted *SagaAborted
	if !errors.As(err, &aborted) || charged != 5 || refunded != 5 {
		t.Fatalf("RunSaga = %v; charged %d, refunded %d; want 5 and 5", err, charged, refunded)
	}
}

// A retry-safe compensable call that a sibling's failure cut off has no recorded result, so the
// rollback runs it again to learn the result it compensates. That run goes through the tool
// middleware like the first: it used to call the tool directly with the model's arguments, so
// the charge the live call made at 500 was repeated at 5 and refunded at 5.
func TestSaga_RollbackRerunGoesThroughToolMiddleware(t *testing.T) {
	var calls atomic.Int32
	var charges, refunded []int
	var mu sync.Mutex
	record := func(list *[]int, v int) { mu.Lock(); *list = append(*list, v); mu.Unlock() }
	// book fails only once the live charge has started: a call the failure reaches before it
	// starts never starts, and would leave the rollback's re-run the first call.
	chargeStarted := make(chan struct{})
	charge := CompensatedFunc("charge", "charge the card", Safety{Idempotent: true},
		func(ctx context.Context, in chargeArgs) (string, error) {
			record(&charges, in.Amount)
			if calls.Add(1) == 1 {
				close(chargeStarted)
				<-ctx.Done() // the live call is cut off waiting for its response
				return "", ctx.Err()
			}
			return "ok", nil
		},
		func(_ context.Context, in chargeArgs, _ string) error { record(&refunded, in.Amount); return nil })
	book := Func("book", "book the flight", Safety{}, func(context.Context, struct{}) (string, error) {
		<-chargeStarted
		return "", errors.New("no seats")
	})
	m := &sagaTurns{turns: [][][3]string{{{"c1", "charge", `{"amount":5}`}, {"b1", "book", `{}`}}}}
	_, err := New(m, NewMemStore(), charge, book).UseTool(scaleCharge).RunSaga(context.Background(), "r", "trip")
	var aborted *SagaAborted
	if !errors.As(err, &aborted) || aborted.CompensateErr != nil {
		t.Fatalf("RunSaga = %v, want a clean *SagaAborted", err)
	}
	if len(charges) != 2 || charges[0] != 500 || charges[1] != 500 || len(refunded) != 1 || refunded[0] != 500 {
		t.Fatalf("charges %v, refunds %v; want the re-run to charge 500 like the live call, and a refund of 500", charges, refunded)
	}
}

// Only a compensable call journals its accepted arguments: a rewritten call to a tool with no
// compensator has nothing to undo them with.
func TestSaga_NonCompensableCallJournalsNoArguments(t *testing.T) {
	charge := Func("charge", "charge the card", Safety{}, func(context.Context, chargeArgs) (string, error) { return "ok", nil })
	fail := Func("book", "book the flight", Safety{}, func(context.Context, struct{}) (string, error) {
		return "", errors.New("no seats")
	})
	store := NewMemStore()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{"amount":5}`), ToolTurn("b1", "book", `{}`), TextTurn("done"))
	_, _ = New(m, store, charge, fail).UseTool(scaleCharge).RunSaga(context.Background(), "r", "trip")
	recs, _ := store.History(context.Background(), "r")
	for _, r := range recs {
		if r.Name == sagaArgsStep("c1") {
			t.Fatalf("a call with no compensator journaled its arguments: %+v", r)
		}
	}
}

// failArgsStore fails the write of the accepted-arguments record.
type failArgsStore struct{ *MemStore }

func (s failArgsStore) Do(ctx context.Context, runID, name string, fn func(context.Context) (Record, error)) (Record, error) {
	if name == sagaArgsStep("c1") {
		return Record{}, errors.New("disk full")
	}
	return s.MemStore.Do(ctx, runID, name, fn)
}

// A compensable call whose accepted arguments cannot be journaled does not run: compensation
// could not undo what it would do.
func TestSaga_UnjournaledArgumentsStopTheCall(t *testing.T) {
	charged, _, err := rewrittenChargeSaga(t, failArgsStore{NewMemStore()}, Safety{}, scaleCharge)
	if charged != 0 || !errors.Is(err, ErrStorage) {
		t.Fatalf("charged %d, RunSaga = %v; want no charge and ErrStorage", charged, err)
	}
}

// A retry-safe compensable call cut off inside a middleware, before the tool ran, journaled no
// arguments; the rollback's re-run, through the middleware and in the saga, journals them, so the
// refund undoes the 500 the re-run charged.
func TestSaga_RollbackRerunJournalsTheAcceptedArguments(t *testing.T) {
	var charges, refunds atomic.Int32
	charge := CompensatedFunc("charge", "charge the card", Safety{Idempotent: true},
		func(_ context.Context, in chargeArgs) (string, error) {
			charges.Store(int32(in.Amount))
			return "ok", nil
		},
		func(_ context.Context, in chargeArgs, _ string) error { refunds.Store(int32(in.Amount)); return nil })
	// book fails only once the live charge has started: a call the failure reaches before it
	// starts never starts, and would leave the rollback's re-run the one that stalls.
	chargeStarted := make(chan struct{})
	book := Func("book", "book the flight", Safety{}, func(context.Context, struct{}) (string, error) {
		<-chargeStarted
		return "", errors.New("no seats")
	})
	var live atomic.Bool
	stall := func(next ToolHandler) ToolHandler { // the live call stalls in the middleware, before the tool
		return func(ctx context.Context, tu ToolUse) (json.RawMessage, error) {
			if tu.Name == "charge" && live.CompareAndSwap(false, true) {
				close(chargeStarted)
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return next(ctx, tu)
		}
	}
	m := &sagaTurns{turns: [][][3]string{{{"c1", "charge", `{"amount":5}`}, {"b1", "book", `{}`}}}}
	_, err := New(m, NewMemStore(), charge, book).UseTool(stall, scaleCharge).RunSaga(context.Background(), "r", "trip")
	var aborted *SagaAborted
	if !errors.As(err, &aborted) || aborted.CompensateErr != nil {
		t.Fatalf("RunSaga = %v, want a clean *SagaAborted", err)
	}
	if charges.Load() != 500 || refunds.Load() != 500 {
		t.Fatalf("charged %d, refunded %d; want the refund to undo the 500 the re-run charged", charges.Load(), refunds.Load())
	}
}

// failArgsReadStore fails any History that holds the accepted-arguments record: the read the
// rollback makes after a re-run journaled it.
type failArgsReadStore struct{ *MemStore }

func (s failArgsReadStore) History(ctx context.Context, runID string) ([]Record, error) {
	recs, err := s.MemStore.History(ctx, runID)
	for _, r := range recs {
		if r.Name == sagaArgsStep("c1") {
			return nil, errors.New("read failed")
		}
	}
	return recs, err
}

// When the rollback cannot read back the arguments its re-run accepted, it stops with that error
// rather than compensate with the model's arguments.
func TestSaga_RollbackRerunStopsWhenItCannotReadTheArguments(t *testing.T) {
	var refunds atomic.Int32
	charge := CompensatedFunc("charge", "charge the card", Safety{Idempotent: true},
		func(context.Context, chargeArgs) (string, error) { return "ok", nil },
		func(_ context.Context, in chargeArgs, _ string) error { refunds.Store(int32(in.Amount)); return nil })
	chargeStarted := make(chan struct{}) // book fails once the live charge has started
	book := Func("book", "book the flight", Safety{}, func(context.Context, struct{}) (string, error) {
		<-chargeStarted
		return "", errors.New("no seats")
	})
	var live atomic.Bool
	stall := func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, tu ToolUse) (json.RawMessage, error) {
			if tu.Name == "charge" && live.CompareAndSwap(false, true) {
				close(chargeStarted)
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return next(ctx, tu)
		}
	}
	m := &sagaTurns{turns: [][][3]string{{{"c1", "charge", `{"amount":5}`}, {"b1", "book", `{}`}}}}
	_, err := New(m, failArgsReadStore{NewMemStore()}, charge, book).UseTool(stall, scaleCharge).RunSaga(context.Background(), "r", "trip")
	var aborted *SagaAborted
	if !errors.As(err, &aborted) || aborted.CompensateErr == nil || refunds.Load() != 0 {
		t.Fatalf("RunSaga = %v, refunded %d; want the rollback stopped with the read error and no refund", err, refunds.Load())
	}
}
