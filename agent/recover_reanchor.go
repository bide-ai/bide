package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Reanchorer is the optional capability of a store that signs and publishes a run's tree head on
// its own writes and can do so again on demand: audit.AuditedStore. Reanchor signs and publishes a
// head over runID's journal as it is now, if no published head covers it, and writes nothing to the
// journal. Recover and RecoverLoop call it for a run that is waiting on a person (see
// WithReanchor).
type Reanchorer interface {
	Reanchor(ctx context.Context, runID string) error
}

// reanchorOption implements ReanchorOption.
type reanchorOption func(*recoverConfig) error

func (f reanchorOption) applyRecover(c *recoverConfig) error     { return f(c) }
func (f reanchorOption) applyRecoverLoop(c *recoverConfig) error { return f(c) }

// WithReanchor sets the function Recover and RecoverLoop call to sign the journal of a run they
// found waiting on a decision or a halt resolution: a run whose drive ended at an ApprovalPending
// or an OutcomeUnknown (several, when parallel sub-agents wait together). A decision or a halt
// resolution recorded by another process (a CLI writing the database, an approver's tool) is a
// journal write the application's AuditedStore did not make, so no signed tree head covers it
// until the application next writes to that run, which for a rejection may be never. Calling fn for
// each such run on every pass bounds that gap to one pass, for a run the pass drives.
//
// By default both call the Reanchor method of the journal's store, or of the first store beneath it
// through Unwrap that has one (Reanchorer): an AuditedStore needs no option, and a Recover or
// RecoverLoop over one re-signs unless told not to. A fn given here replaces that; a nil fn turns
// re-signing off. fn is called only for a run this pass drove (it holds the run's lease), at most
// once per run per pass, and only for the runs the pass already enumerated as unfinished: it adds
// no listing and no scan. fn must honor its context: each call gets a deadline of
// WithReanchorTimeout (half the lease TTL by default), so a hung anchor cannot hold a run's lease
// and a concurrency slot for longer. An error it returns (the deadline's included) goes to
// WithRecoverErrors, wrapping ErrStorage, and the next pass tries again. fn must be safe for
// concurrent use, as runs are driven concurrently.
func WithReanchor(fn func(ctx context.Context, runID string) error) ReanchorOption {
	return reanchorOption(func(c *recoverConfig) error { c.reanchor, c.reanchorSet = fn, true; return nil })
}

// WithReanchorTimeout bounds each re-sign (see WithReanchor): the context fn gets expires after d.
// It must be positive. The default is half the lease TTL.
func WithReanchorTimeout(d time.Duration) ReanchorOption {
	return reanchorOption(func(c *recoverConfig) error {
		if d <= 0 {
			return fmt.Errorf("reanchor timeout must be positive, got %v: %w", d, ErrConfig)
		}
		c.reanchorTimeout = d
		return nil
	})
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

// waitingRuns returns the runs whose journals hold what a person is deciding: every run err's pauses
// for an approval or a halt resolution name (and its root, when a pause came from a sub-agent). err
// is walked as a tree, since parallel sub-agents that wait together return their pauses joined. It
// returns nil when err holds no such pause.
func waitingRuns(err error) []string {
	var runs []string
	add := func(id string) {
		if id != "" && !slices.Contains(runs, id) {
			runs = append(runs, id)
		}
	}
	var walk func(error)
	walk = func(e error) {
		switch x := e.(type) {
		case nil:
			return
		case *ApprovalPending:
			add(x.RunID)
			add(x.RootRunID)
		case *OutcomeUnknown:
			add(x.RunID)
			add(x.RootRunID)
		case interface{ Unwrap() []error }:
			for _, c := range x.Unwrap() {
				walk(c)
			}
		case interface{ Unwrap() error }:
			walk(x.Unwrap())
		}
	}
	walk(err)
	return runs
}

// reanchorWaiting re-signs each run in runs under fn, each call under a deadline of timeout, and
// returns every failure.
func reanchorWaiting(ctx context.Context, fn func(context.Context, string) error, runs []string, timeout time.Duration) error {
	var errs []error
	for _, id := range runs {
		rctx, cancel := context.WithTimeout(ctx, timeout)
		err := fn(rctx, id)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("re-sign run %s: %w (%w)", id, err, ErrStorage))
		}
	}
	return errors.Join(errs...)
}
