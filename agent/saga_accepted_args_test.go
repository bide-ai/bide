package agent

import (
	"context"
	"encoding/json"
	"errors"
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
		if r.Name == "@saga/args/c1" {
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
		if r.Name == "@saga/args/c1" {
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
