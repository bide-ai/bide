package agent

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"time"
)

// RecoverOption configures Recover. Lease options apply only when the store implements Leaser.
type RecoverOption func(*recoverConfig)

type recoverConfig struct {
	holder string
	ttl    time.Duration
}

// WithLeaseHolder sets the identity this recoverer claims leases under. Defaults to a
// host/pid/random string. Give each process a stable, distinct holder if you want lease ownership
// to survive a restart of the same logical worker.
func WithLeaseHolder(id string) RecoverOption { return func(c *recoverConfig) { c.holder = id } }

// WithLeaseTTL sets how long an acquired lease is valid. Recover renews it while a run is driving,
// so a crash lets the lease expire after roughly this long and another process takes over. Defaults
// to 30s. Set it comfortably above the store's clock skew.
func WithLeaseTTL(d time.Duration) RecoverOption { return func(c *recoverConfig) { c.ttl = d } }

func defaultHolder() string {
	host, _ := os.Hostname()
	return fmt.Sprintf("%s-%d-%d", host, os.Getpid(), rand.Uint64())
}

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
// If the store also implements Leaser, Recover coordinates across processes: it claims an
// exclusive, renewed lease per run before driving it and skips a run another holder currently
// leases, so competing recoverers do not both re-drive the same run. A crash lets the lease expire
// (see WithLeaseTTL) and another process takes over. Without Leaser, Recover drives every
// enumerated run, which is safe under at-most-once memoization but redundant across processes.
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
func Recover(ctx context.Context, store Durable, resume func(ctx context.Context, runID string) error, opts ...RecoverOption) (int, error) {
	lister, ok := store.(Lister)
	if !ok {
		return 0, fmt.Errorf("Recover needs a store that implements Lister to enumerate runs: %w", ErrConfig)
	}
	cfg := recoverConfig{ttl: 30 * time.Second}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.holder == "" {
		cfg.holder = defaultHolder()
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

		// Drive under the run's lease (when the store supports one). A run another holder currently
		// leases is skipped; competing recoverers and live primary drivers coordinate through Lease.
		driven, err := Lease(ctx, store, runID, func(ctx context.Context) error { return resume(ctx, runID) },
			WithLeaseHolder(cfg.holder), WithLeaseTTL(cfg.ttl))
		if !driven {
			if err != nil {
				errs = append(errs, fmt.Errorf("recover run %s: %w", runID, err))
			}
			continue // leased by another holder (err nil) or acquisition failed
		}
		recovered++
		if err != nil && !isPause(err) {
			errs = append(errs, fmt.Errorf("recover run %s: %w", runID, err))
		}
	}
	return recovered, errors.Join(errs...)
}

// Lease runs drive under an exclusive, auto-renewed lease on runID, so a primary driver and a
// recoverer (or two workers) do not drive the same run at once. If the store implements Leaser and
// another holder currently leases the run, drive is NOT called and Lease returns (false, nil). If
// the store does not implement Leaser, drive runs unconditionally. The bool reports whether drive
// ran; the error is the acquisition error (when false) or drive's own error (when true).
//
// A primary driver wraps Agent.Run so a live run and a recoverer never both drive it, using the
// same lease the recoverer respects:
//
//	driven, err := agent.Lease(ctx, store, runID, func(ctx context.Context) error {
//	    _, err := ag.Run(ctx, runID, input)
//	    return err
//	}, agent.WithLeaseHolder("worker-1"))
func Lease(ctx context.Context, store Durable, runID string, drive func(context.Context) error, opts ...RecoverOption) (bool, error) {
	cfg := recoverConfig{ttl: 30 * time.Second}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.holder == "" {
		cfg.holder = defaultHolder()
	}
	leaser, ok := store.(Leaser)
	if !ok {
		return true, drive(ctx) // no leasing available: drive unconditionally
	}
	got, err := leaser.AcquireLease(ctx, runID, cfg.holder, cfg.ttl)
	if err != nil {
		return false, fmt.Errorf("acquire lease %s: %w (%w)", runID, err, ErrStorage)
	}
	if !got {
		return false, nil // another holder is driving it
	}
	err = driveWithRenew(ctx, leaser, runID, cfg, func() error { return drive(ctx) })
	_ = leaser.ReleaseLease(ctx, runID, cfg.holder)
	return true, err
}

// driveWithRenew runs the drive while renewing the lease every ttl/2, so a drive that outlasts the
// TTL keeps its lease instead of letting another process grab a run it is actively driving.
func driveWithRenew(ctx context.Context, leaser Leaser, runID string, cfg recoverConfig, run func() error) error {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(cfg.ttl / 2)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				_, _ = leaser.RenewLease(ctx, runID, cfg.holder, cfg.ttl)
			}
		}
	}()
	err := run()
	close(done)
	return err
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
