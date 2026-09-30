package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// slowArgsStore holds the write of c1's accepted arguments until its context is cancelled (a
// slow store under a saga sibling's failure), and reports when that write has begun.
type slowArgsStore struct {
	*MemStore
	writing chan struct{}
}

func (s slowArgsStore) Do(ctx context.Context, runID, name string, fn func(context.Context) (Record, error)) (Record, error) {
	if name == sagaArgsStep("c1") {
		close(s.writing)
		<-ctx.Done()
		return Record{}, ctx.Err()
	}
	return s.MemStore.Do(ctx, runID, name, fn)
}

// F4. The accepted-arguments write now comes after enterTool, so its failure counts as reached.
// When that write is cut off by the saga's own cancellation (a sibling step failed), the tool was
// provably never called, yet its claim stays live: the rollback cannot tell the step never ran and
// stops on an unknown outcome for it. Before round 4 the refusal recorded the claim as never
// started and the saga aborted cleanly.
func TestRev117d_CancelledArgsWriteLeavesRollbackOnUnknownOutcome(t *testing.T) {
	var charges atomic.Int32
	charge := CompensatedFunc("charge", "charge the card", Safety{},
		func(context.Context, chargeArgs) (string, error) { charges.Add(1); return "ok", nil },
		func(context.Context, chargeArgs, string) error { return nil })
	store := slowArgsStore{NewMemStore(), make(chan struct{})}
	book := Func("book", "book the flight", Safety{}, func(context.Context, struct{}) (string, error) {
		<-store.writing // fails while charge's accepted arguments are being written
		return "", errors.New("no seats")
	})
	m := &sagaTurns{turns: [][][3]string{{{"c1", "charge", `{"amount":5}`}, {"b1", "book", `{}`}}}}
	_, err := New(m, store, charge, book).UseTool(scaleCharge).RunSaga(context.Background(), "r", "trip")
	var aborted *SagaAborted
	if !errors.As(err, &aborted) {
		t.Fatalf("RunSaga = %v, want *SagaAborted", err)
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

func (w safetyWrap) Spec() ToolSpec { s := SpecOf(w.Tool); s.Safety = Safety{}; return s }
func (w safetyWrap) Unwrap() Tool   { return w.Tool }

// (c) New refuses a wrapper that overrides the Safety of a sub-agent it wraps, as SubAgent refuses
// WithSafety: the sub-run's own calls carry their safety.
func TestRev117d_NewRefusesSafetyOverrideOverASubAgent(t *testing.T) {
	sub := New(NewScriptedModel(TextTurn("x")), NewMemStore())
	var calls atomic.Int32
	_, err := New(&countingModel{n: &calls}, NewMemStore(), safetyWrap{SubAgent("delegate", "", sub)}).Run(context.Background(), "r1", "go")
	if !errors.Is(err, ErrConfig) || calls.Load() != 0 {
		t.Fatalf("Run = %v after %d model calls; want ErrConfig before any", err, calls.Load())
	}
}
