// recovery.go groups the read side of the durable journal: Recover re-drives in-flight runs
// after a restart, run leasing (Leaser, in lease.go) coordinates a single driver per run under
// HA, and replay (Replay, in replay.go) deterministically re-emits a journaled run. Given a
// store of recorded runs, these bring survivors back to life exactly once.

package agent

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"sync"
	"time"
)

// ===========================================================================
// Recovery: re-drive in-flight runs after a restart
// ===========================================================================

// RecoverOption configures Recover. Lease options apply only when the store implements Leaser.
type RecoverOption func(*recoverConfig)

type recoverConfig struct {
	holder      string
	ttl         time.Duration
	interval    time.Duration // RecoverLoop only; 0 means ttl/2
	concurrency int           // RecoverLoop only
	onError     func(error)   // RecoverLoop only
	intervalSet bool
}

// WithLeaseHolder names the worker that claims leases, as the leases table and logs show it.
// Defaults to a host/pid/random string. The name identifies, it does not grant: each Lease call
// (and each run Recover drives) claims under a token of its own derived from it, so two drivers
// sharing a name still exclude each other, and a restarted worker waits out its predecessor's
// lease (the TTL) like any other process rather than taking it over.
func WithLeaseHolder(id string) RecoverOption { return func(c *recoverConfig) { c.holder = id } }

// WithLeaseTTL sets how long an acquired lease is valid. Lease renews it while a run is driving,
// so a crash lets the lease expire after roughly this long and another process (RecoverLoop)
// takes over. Defaults to 30s. Set it well above the store's round-trip time and the longest pause
// you expect a process to take: renewal starts at half the TTL, and a drive that cannot renew
// within three quarters of it is cancelled with ErrLeaseLost (see Lease). It must be positive:
// Lease, Recover and RecoverLoop return an ErrConfig error otherwise.
func WithLeaseTTL(d time.Duration) RecoverOption { return func(c *recoverConfig) { c.ttl = d } }

// WithRecoverInterval sets how often RecoverLoop starts a recovery pass. Defaults to half the
// lease TTL, so a dead holder's run is taken over within about 1.5 TTLs of its last renewal. It
// must be positive. Only RecoverLoop reads it.
func WithRecoverInterval(d time.Duration) RecoverOption {
	return func(c *recoverConfig) { c.interval, c.intervalSet = d, true }
}

// WithRecoverConcurrency caps how many runs RecoverLoop drives at once. Defaults to 16. It must
// be at least 1. Only RecoverLoop reads it.
func WithRecoverConcurrency(n int) RecoverOption {
	return func(c *recoverConfig) { c.concurrency = n }
}

// WithRecoverErrors sets the function RecoverLoop hands each genuine failure to: a store error
// while enumerating runs or acquiring a lease, or a drive that failed. Pauses and lost leases are
// not failures and are not reported. Without it, RecoverLoop drops failures (the next pass
// retries the run). It may be called from several goroutines, one call at a time. Only
// RecoverLoop reads it.
func WithRecoverErrors(fn func(error)) RecoverOption {
	return func(c *recoverConfig) { c.onError = fn }
}

// leaseConfig applies opts over the defaults and validates the result.
func leaseConfig(opts []RecoverOption) (recoverConfig, error) {
	cfg := recoverConfig{ttl: 30 * time.Second, concurrency: 16}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.ttl <= 0 {
		return cfg, fmt.Errorf("lease TTL must be positive, got %v: %w", cfg.ttl, ErrConfig)
	}
	if cfg.holder == "" {
		cfg.holder = defaultHolder()
	}
	return cfg, nil
}

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

// completedAnswer reports whether recs hold the completion marker and, if so, the run's
// recorded final answer: the last model turn journaled before the marker. Records after the
// marker (written by an older version that re-drove finished runs) are ignored, so the first
// completion's answer stands.
func completedAnswer(recs []Record) (Message, bool) {
	for i, r := range recs {
		if r.Kind != StepValue || r.Name != runCompleteStep {
			continue
		}
		for j := i - 1; j >= 0; j-- {
			if recs[j].Kind == StepModel && recs[j].Message != nil {
				return *recs[j].Message, true
			}
		}
		return Message{}, true
	}
	return Message{}, false
}

// Recover re-drives the runs that were in flight when the process died, in one pass. It enumerates
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
// (see WithLeaseTTL), and the next pass of another process takes the run over: Recover is one
// pass, so run RecoverLoop to keep passing. Without Leaser, Recover drives every
// enumerated run, which is safe under at-most-once memoization but redundant across processes.
//
// A pause is a SUCCESS, not a failure. A re-driven run that is still waiting returns a Pause
// (*ApprovalPending, *InterruptPending, *TimerPending, *SignalPending, *OutcomeUnknown);
// Recover detects it with IsPause and does NOT record it as an error: it means "recovered,
// still waiting", and the run resumes later when its condition is met (a human approves, an
// interrupt is answered, a timer fires). Only a genuine error (a model
// or storage fault, a bad tool) is joined into the returned error. A run whose lease was lost
// mid-drive (ErrLeaseLost) is not joined either: another process holds it now and carries it on.
//
// resume is deployment POLICY, not a mechanism the SDK can supply: it knows which agent drives
// a run and any Waker or clock to bind onto the context (a Waker-bound resume rebuilds the timer
// set for sleeping runs, since Sleep re-registers its wake on replay). The run's input and entry
// point (Run or RunSaga) are in its journal (see RecordedStart), and a resume with another input
// or entry point is ErrConfig. A typical resume is:
//
//	func(ctx context.Context, runID string) error {
//	    start, ok, err := agent.RecordedStart(ctx, store, runID)
//	    if err != nil {
//	        return err
//	    }
//	    if !ok {
//	        start = startFor(runID) // your own record, for a run not driven under this version
//	    }
//	    ctx = agent.WithWaker(ctx, w)
//	    if start.Saga {
//	        _, err = a.RunSaga(ctx, runID, start.Input)
//	    } else {
//	        _, err = a.Run(ctx, runID, start.Input)
//	    }
//	    return err
//	}
//
// Recover skips a sub-agent's run (IsSubRun): its root run drives it, and re-running the root
// resumes it. It skips a session's journal and turn runs (IsSessionRun) too: a turn is seeded with
// the transcript before its message, which only the session holds, and only the session records
// its answer, so an unfinished turn resumes when its message is sent again (Send with the same
// input, or the redelivered SendOnce). resume should no-op any other runID it does not own;
// Recover re-drives every other incomplete run it enumerates.
func Recover(ctx context.Context, store Durable, resume func(ctx context.Context, runID string) error, opts ...RecoverOption) (int, error) {
	lister, ok := capabilityOf[Lister](store)
	if !ok {
		return 0, fmt.Errorf("Recover needs a store that implements Lister (itself or through Unwrap) to enumerate runs: %w", ErrConfig)
	}
	cfg, err := leaseConfig(opts)
	if err != nil {
		return 0, err
	}

	var recovered int
	var errs []error
	for runID, err := range lister.Runs(ctx, recoverFilter) {
		if err != nil {
			errs = append(errs, fmt.Errorf("list runs for recovery: %w (%w)", err, ErrStorage))
			break
		}
		if !recoverable(runID) {
			continue
		}
		driven, err := recoverRun(ctx, store, runID, resume, cfg)
		if driven {
			recovered++
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return recovered, errors.Join(errs...)
}

// endOfRunMarkers are the journal names of the terminal markers: a run that completed, a saga
// that aborted and finished its rollback, and a cancelled run are over.
var endOfRunMarkers = []string{runCompleteStep, runAbortedStep, runCancelledStep}

// recoverFilter is the runs a recovery pass enumerates: those holding no terminal marker. A SQL
// store evaluates the filter in its query, so a pass reads none of the finished runs.
var recoverFilter = RunFilter{ExcludeHolding: endOfRunMarkers}

// runEnded reports whether runID holds an entry named by a terminal marker (see endOfRunMarkers):
// recoverFilter's test, applied to one run. Like the filter, it asks only whether the entry exists,
// so it reads no header and decodes nothing: a run in a format this version cannot read that is
// not over still reaches resume, which refuses it. Over a Journal it costs one point read (Store.Get)
// per marker, stopping at the first it finds; over another Durable, one History.
func runEnded(ctx context.Context, store Durable, runID string) (bool, error) {
	if j := journalOf(store); j != nil {
		for _, name := range endOfRunMarkers {
			if _, ok, err := j.store.Get(ctx, runID, name); err != nil || ok {
				if err != nil {
					return false, storageErr(fmt.Sprintf("read step %q of run %s", name, runID), err)
				}
				return true, nil
			}
		}
		return false, nil
	}
	recs, err := store.History(ctx, runID)
	if err != nil {
		return false, fmt.Errorf("load history %s: %w (%w)", runID, err, ErrStorage)
	}
	for _, r := range recs {
		if slices.Contains(endOfRunMarkers, r.Name) {
			return true, nil
		}
	}
	return false, nil
}

// recoverable reports whether a run the recovery filter admits is one a recovery pass drives: not a
// sub-agent's run (its root's re-run resumes it) or a session's (the session resumes it).
func recoverable(runID string) bool { return !IsSubRun(runID) && !IsSessionRun(runID) }

// recoverRun drives runID under its lease (when the store supports one) and reports whether it
// drove it and the genuine failure, if any. A run another holder currently leases is skipped;
// competing recoverers and live primary drivers coordinate through Lease. A pause and a lost lease
// are not failures.
//
// The pass listed runID before it held the lease, and another driver may have finished the run
// since (while the pass waited for a slot, or drove the runs listed before it). So under the lease,
// before resume, recoverRun checks the terminal markers again and leaves a run that is over
// undriven. The check cannot miss a finish: a driver records its marker before it releases its
// lease, and this drive holds the lease from before the check until after resume returns.
func recoverRun(ctx context.Context, store Durable, runID string, resume func(ctx context.Context, runID string) error, cfg recoverConfig) (bool, error) {
	var ended bool
	driven, err := Lease(ctx, store, runID, func(ctx context.Context) error {
		over, err := runEnded(ctx, store, runID)
		if err != nil || over {
			ended = over
			return err
		}
		return resume(ctx, runID)
	}, WithLeaseHolder(cfg.holder), WithLeaseTTL(cfg.ttl))
	if ended {
		return false, nil
	}
	if err != nil && (!driven || !IsPause(err) && !errors.Is(err, ErrLeaseLost)) {
		return driven, fmt.Errorf("recover run %s: %w", runID, err)
	}
	return driven, nil
}

// RecoverLoop re-drives in-flight runs until ctx is done, so a run whose holder dies is taken over
// automatically: Recover is a single pass, and a run another holder leases at that moment is left
// for a later one. Each pass, started every WithRecoverInterval (half the lease TTL by default),
// enumerates the store's runs as Recover does and drives each incomplete run it can lease, so a
// dead holder's run is picked up within about one interval of its lease expiring (the TTL after
// the holder's last renewal).
//
// Runs are driven concurrently, up to WithRecoverConcurrency at once (16 by default), so one long
// drive does not hold up the others; a run this loop is already driving is not started again. A
// pass that finds every slot busy waits for one, so each pass reaches every run it listed; the
// next pass starts when this one has started all of its drives and the interval has elapsed. Genuine failures go
// to the WithRecoverErrors handler, and the run is retried on the next pass; pauses and lost leases
// are not failures (see Recover).
// A run that failed because its Waker could not schedule a wake (an error wrapping ErrStorage)
// recorded nothing for the sleeping call, so the next pass reaches the Sleep again and schedules
// again.
//
// Run it once per process, for the life of the process, with the same resume Recover takes:
//
//	go func() {
//	    err := agent.RecoverLoop(ctx, store, resume,
//	        agent.WithLeaseHolder("worker-1"),
//	        agent.WithRecoverErrors(func(err error) { log.Print(err) }))
//	    // err is ctx's error once ctx is done
//	}()
//
// A configuration error (a store that does not implement Lister, a non-positive TTL, interval or
// concurrency) is returned at once. Otherwise RecoverLoop returns ctx's error when ctx is done,
// after the drives it started (whose contexts derive from ctx) have returned.
func RecoverLoop(ctx context.Context, store Durable, resume func(ctx context.Context, runID string) error, opts ...RecoverOption) error {
	lister, ok := capabilityOf[Lister](store)
	if !ok {
		return fmt.Errorf("RecoverLoop needs a store that implements Lister (itself or through Unwrap) to enumerate runs: %w", ErrConfig)
	}
	cfg, err := leaseConfig(opts)
	if err != nil {
		return err
	}
	if !cfg.intervalSet {
		cfg.interval = max(cfg.ttl/2, 1)
	}
	if cfg.interval <= 0 {
		return fmt.Errorf("recover interval must be positive, got %v: %w", cfg.interval, ErrConfig)
	}
	if cfg.concurrency < 1 {
		return fmt.Errorf("recover concurrency must be at least 1, got %d: %w", cfg.concurrency, ErrConfig)
	}

	var reportMu sync.Mutex
	report := func(err error) {
		if cfg.onError == nil || ctx.Err() != nil {
			return // no handler, or a failure caused by the shutdown itself
		}
		reportMu.Lock()
		defer reportMu.Unlock()
		cfg.onError(err)
	}
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		inFlight = map[string]bool{}
		slots    = make(chan struct{}, cfg.concurrency)
	)
	defer wg.Wait()
	pass := func() {
		for runID, err := range lister.Runs(ctx, recoverFilter) {
			if err != nil {
				report(fmt.Errorf("list runs for recovery: %w (%w)", err, ErrStorage))
				return
			}
			if ctx.Err() != nil {
				return
			}
			if !recoverable(runID) {
				continue
			}
			mu.Lock()
			busy := inFlight[runID]
			mu.Unlock()
			if busy {
				continue // this loop is driving it already
			}
			// Wait for a free slot rather than leave the rest of the list to the next pass: the next
			// pass starts from the top again, so runs that stay incomplete on every pass (halted
			// ones) would take the slots each time and starve the runs listed after them.
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			mu.Lock()
			inFlight[runID] = true
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					mu.Lock()
					delete(inFlight, runID)
					mu.Unlock()
					<-slots
				}()
				if _, err := recoverRun(ctx, store, runID, resume, cfg); err != nil {
					report(err)
				}
			}()
		}
	}

	t := time.NewTicker(cfg.interval)
	defer t.Stop()
	for {
		pass()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}
