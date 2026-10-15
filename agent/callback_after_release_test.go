package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// RecoverLoop gives a user's error handler a failure only after the run's slot and its in-flight mark
// are released: a handler that waits (here for the next run to be driven, with one slot) must not
// hold up the very work it is waiting for.
func TestRecoverLoop_TheErrorHandlerRunsAfterTheRunsSlotIsReleased(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	j := memJournal()
	for _, run := range []string{"a", "b"} {
		if _, _, err := j.putEntry(ctx, run, runStartStep, Record{Kind: StepValue, Result: []byte("{}")}); err != nil {
			t.Fatal(err)
		}
	}
	bDriven := make(chan struct{})
	resume := func(_ context.Context, run string, _ RunStart) error {
		if run == "b" {
			select {
			case <-bDriven:
			default:
				close(bDriven)
			}
			return nil
		}
		return errors.New("a fails")
	}
	handled := make(chan error, 1)
	handler := func(error) {
		select {
		case <-bDriven:
			handled <- nil
		case <-time.After(5 * time.Second):
			handled <- errors.New("the handler waited 5s for the next run to be driven: it ran while holding the only slot")
		}
	}
	go func() {
		_ = RecoverLoop(ctx, j, resume, WithRecoverConcurrency(1), WithRecoverInterval(time.Hour), WithRecoverErrors(handler))
	}()
	if err := <-handled; err != nil {
		t.Fatal(err)
	}
}

// A flush called from inside the WithNotifyErrors handler would wait for the very round that is
// calling the handler: it returns an error at once instead. The round finishes, and a flush from
// anywhere else then settles.
func TestNotifier_AFlushFromTheErrorHandlerFailsAtOnceInsteadOfDeadlocking(t *testing.T) {
	var e *notifyEngine
	type outcome struct {
		err  error
		took time.Duration
	}
	got := make(chan outcome, 1)
	handler := func(error) {
		start := time.Now()
		err := e.flush(context.Background()) // no deadline: a deadlock would hang here
		select {
		case got <- outcome{err, time.Since(start)}:
		default:
		}
	}
	sink := &fakeSink{fail: 1000}
	trail := NewMemNotifyTrail()
	e = testEngine(t, memJournal(), sink, trail, resolvedClock(), "p1", WithNotifyErrors(handler), WithNotifyRetry(time.Hour, time.Hour))
	e.w.submit(haltEvent())
	select {
	case o := <-got:
		if !errors.Is(o.err, ErrConfig) || !strings.Contains(o.err.Error(), "WithNotifyErrors") || o.took > time.Second {
			t.Fatalf("flush from the handler = %v after %v, want an ErrConfig naming WithNotifyErrors at once", o.err, o.took)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the flush from the handler hung")
	}
	// The round finished: it is no longer in flight, and a flush from outside the handler settles.
	fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.flush(fctx); errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the round never finished: %v", err)
	}
	if recs, _ := trail.Load(context.Background(), "notify:r"); len(recs) < 2 {
		t.Fatalf("trail = %v: the round did not run", recKinds(recs))
	}
	if sink.count() == 0 {
		t.Fatal("nothing was delivered")
	}
}
