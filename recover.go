package agent

import (
	"context"
	"errors"
	"fmt"
)

// IsComplete reports whether runID has reached its terminal answer, by checking the
// journal for the durable completion marker the agent loop records at the end of a run
// (see runCompleteStep). A crash-recovery supervisor uses it to skip finished runs.
func IsComplete(ctx context.Context, store Durable, runID string) (bool, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return false, fmt.Errorf("load history %s: %w (%w)", runID, err, ErrStorage)
	}
	for _, r := range recs {
		if r.Kind == StepValue && r.Name == runCompleteStep {
			return true, nil
		}
	}
	return false, nil
}

// Recover re-drives the runs that were in flight when the process died. It enumerates
// every run the store holds (via Lister), skips the ones already marked complete, and
// calls resume for each remaining run to push it forward. It returns how many runs it
// re-drove and the joined genuine failures (nil if none).
//
// The store must implement Lister; a store that cannot enumerate its runs (the base
// Durable contract does not require it) yields an ErrConfig-wrapped error.
//
// A pause is a SUCCESS, not a failure. A re-driven run that is still waiting returns one
// of the durable pause signals (*PendingApproval, *Interrupted, *Sleeping, *ResumeHalt);
// Recover detects those with errors.As and does NOT record them as errors: they mean
// "recovered, still waiting", and the run resumes later when its condition is met (a
// human approves, an interrupt is answered, a timer fires). Only a genuine error (a model
// or storage fault, a bad tool) is joined into the returned error.
//
// resume is deployment POLICY, not a mechanism the SDK can supply: it alone knows a run's
// original input and any Waker or clock to bind onto the context (a Waker-bound resume
// rebuilds the timer set for sleeping runs, since Sleep re-registers its wake on replay).
// A typical resume is:
//
//	func(ctx context.Context, runID string) error {
//	    _, err := agent.Run(agent.WithWaker(ctx, w), runID, inputFor(runID))
//	    return err
//	}
//
// resume should no-op a runID it does not own: a sub-agent run is driven by its parent, so
// re-driving one directly is redundant (harmless under at-most-once memoization, but the
// parent already replays it). Recover re-drives every incomplete run it enumerates; let
// resume decide which ones it is responsible for.
func Recover(ctx context.Context, store Durable, resume func(ctx context.Context, runID string) error) (int, error) {
	lister, ok := store.(Lister)
	if !ok {
		return 0, fmt.Errorf("Recover needs a store that implements Lister to enumerate runs: %w", ErrConfig)
	}
	runIDs, err := lister.Runs(ctx)
	if err != nil {
		return 0, fmt.Errorf("list runs for recovery: %w (%w)", err, ErrStorage)
	}

	var recovered int
	var errs []error
	for _, runID := range runIDs {
		complete, err := IsComplete(ctx, store, runID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if complete {
			continue // finished before the crash: nothing to re-drive
		}
		recovered++
		if err := resume(ctx, runID); err != nil && !isPause(err) {
			errs = append(errs, fmt.Errorf("recover run %s: %w", runID, err))
		}
	}
	return recovered, errors.Join(errs...)
}

// isPause reports whether err is a durable pause signal (the run recovered and is still
// waiting) rather than a genuine failure.
func isPause(err error) bool {
	var (
		pa  *PendingApproval
		itr *Interrupted
		slp *Sleeping
		rh  *ResumeHalt
	)
	return errors.As(err, &pa) || errors.As(err, &itr) || errors.As(err, &slp) || errors.As(err, &rh)
}
