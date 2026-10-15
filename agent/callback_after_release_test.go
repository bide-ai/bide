package agent

import (
	"context"
	"errors"
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
