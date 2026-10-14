package agent

import (
	"context"
	"errors"
	"fmt"
)

// Reanchorer is the optional capability of a store that signs and publishes a run's tree head on
// its own writes and can do so again on demand: audit.AuditedStore. Reanchor signs and publishes a
// head over runID's journal as it is now, if no published head covers it, and writes nothing to the
// journal. RecoverLoop calls it for a run that is waiting on a person (see WithReanchor).
type Reanchorer interface {
	Reanchor(ctx context.Context, runID string) error
}

// WithReanchor sets the function RecoverLoop calls to sign the journal of a run it found waiting on
// a decision or a halt resolution: a run whose drive ended at an ApprovalPending or an
// OutcomeUnknown. A decision or a halt resolution recorded by another process (a CLI writing the
// database, an approver's tool) is a journal write the application's AuditedStore did not make, so
// no signed tree head covers it until the application next writes to that run, which for a
// rejection may be never. Calling fn for each such run on every pass bounds that gap to one pass.
//
// By default RecoverLoop calls the Reanchor method of the journal's store, or of the first store
// beneath it through Unwrap that has one (Reanchorer): an AuditedStore needs no option. A fn given
// here replaces that; a nil fn turns re-signing off. fn is called only for a run this pass drove
// (it holds the run's lease), at most once per run per pass, and only for the runs the pass
// enumerated as unfinished: it adds no listing and no scan. An error it returns goes to
// WithRecoverErrors, wrapping ErrStorage, and the next pass tries again. fn must be safe for
// concurrent use, as runs are driven concurrently.
func WithReanchor(fn func(ctx context.Context, runID string) error) RecoverLoopOption {
	return recoverLoopOption(func(c *recoverConfig) { c.reanchor, c.reanchorSet = fn, true })
}

// reanchorFunc is the function a pass re-signs runs with: the one WithReanchor set, else the
// store's Reanchorer, else nil.
func (c recoverConfig) reanchorFunc(store *Journal) func(context.Context, string) error {
	if c.reanchorSet {
		return c.reanchor
	}
	if r, ok := Capability[Reanchorer](store.store); ok {
		return r.Reanchor
	}
	return nil
}

// waitingRuns returns the runs whose journals hold what a person is deciding: the run err paused
// (and its root, when the pause came from a sub-agent). It returns nil unless err is a pause for an
// approval or a halt resolution, the two a person answers with a signed decision or a recorded
// resolution.
func waitingRuns(err error) []string {
	var ref RunRef
	if ap, ok := errors.AsType[*ApprovalPending](err); ok {
		ref = ap.RunRef
	} else if ou, ok := errors.AsType[*OutcomeUnknown](err); ok {
		ref = ou.RunRef
	} else {
		return nil
	}
	runs := []string{ref.RunID}
	if ref.RootRunID != "" && ref.RootRunID != ref.RunID {
		runs = append(runs, ref.RootRunID)
	}
	return runs
}

// reanchorWaiting re-signs each run in runs under fn and returns the first failure.
func reanchorWaiting(ctx context.Context, fn func(context.Context, string) error, runs []string) error {
	var errs []error
	for _, id := range runs {
		if err := fn(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("re-sign run %s: %w (%w)", id, err, ErrStorage))
		}
	}
	return errors.Join(errs...)
}
