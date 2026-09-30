// recovery.go groups the read side of the durable journal: Recover re-drives in-flight runs
// after a restart, run leasing (Leaser) coordinates a single driver per run under HA, and
// replay deterministically re-emits a journaled run. Given a store of recorded runs, these
// bring survivors back to life exactly once.

package agent

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
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
// A pause is a SUCCESS, not a failure. A re-driven run that is still waiting returns one
// of the durable pause signals (*PendingApproval, *Interrupted, *Sleeping, *Awaiting, *ResumeHalt);
// Recover detects those with errors.As and does NOT record them as errors: they mean
// "recovered, still waiting", and the run resumes later when its condition is met (a
// human approves, an interrupt is answered, a timer fires). Only a genuine error (a model
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
// resumes it. resume should no-op any other runID it does not own; Recover re-drives every
// other incomplete run it enumerates.
func Recover(ctx context.Context, store Durable, resume func(ctx context.Context, runID string) error, opts ...RecoverOption) (int, error) {
	lister, ok := Capability[Lister](store)
	if !ok {
		return 0, fmt.Errorf("Recover needs a store that implements Lister (itself or through Unwrap) to enumerate runs: %w", ErrConfig)
	}
	cfg, err := leaseConfig(opts)
	if err != nil {
		return 0, err
	}

	runIDs, err := lister.Runs(ctx)
	if err != nil {
		return 0, fmt.Errorf("list runs for recovery: %w (%w)", err, ErrStorage)
	}

	var recovered int
	var errs []error
	for _, runID := range runIDs {
		if ok, err := recoverable(ctx, store, runID); err != nil {
			errs = append(errs, err)
			continue
		} else if !ok {
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

// recoverable reports whether runID still needs driving: it is not a sub-agent's run (its root's
// re-run resumes it) and has neither completed nor finished rolling back an aborted saga.
func recoverable(ctx context.Context, store Durable, runID string) (bool, error) {
	if IsSubRun(runID) {
		return false, nil
	}
	complete, err := IsComplete(ctx, store, runID)
	if err != nil || complete {
		return false, err // finished before the crash: nothing to re-drive
	}
	aborted, err := hasValueStep(ctx, store, runID, runAbortedStep)
	if err != nil || aborted {
		return false, err // a saga that aborted and finished its rollback: over
	}
	return true, nil
}

// recoverRun drives runID under its lease (when the store supports one) and reports whether it
// drove it and the genuine failure, if any. A run another holder currently leases is skipped;
// competing recoverers and live primary drivers coordinate through Lease. A pause and a lost lease
// are not failures.
func recoverRun(ctx context.Context, store Durable, runID string, resume func(ctx context.Context, runID string) error, cfg recoverConfig) (bool, error) {
	driven, err := Lease(ctx, store, runID, func(ctx context.Context) error { return resume(ctx, runID) },
		WithLeaseHolder(cfg.holder), WithLeaseTTL(cfg.ttl))
	if err != nil && (!driven || !isPause(err) && !errors.Is(err, ErrLeaseLost)) {
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
	lister, ok := Capability[Lister](store)
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
		runIDs, err := lister.Runs(ctx)
		if err != nil {
			report(fmt.Errorf("list runs for recovery: %w (%w)", err, ErrStorage))
			return
		}
		for _, runID := range runIDs {
			if ctx.Err() != nil {
				return
			}
			mu.Lock()
			busy := inFlight[runID]
			mu.Unlock()
			if busy {
				continue // this loop is driving it already
			}
			if ok, err := recoverable(ctx, store, runID); err != nil {
				report(err)
				continue
			} else if !ok {
				continue
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

// Lease runs drive under an exclusive, auto-renewed lease on runID, so a primary driver and a
// recoverer (or two workers) do not drive the same run at once. If the store implements Leaser and
// another holder currently leases the run, drive is NOT called and Lease returns (false, nil). If
// neither the store nor a store it wraps implements Leaser (see Capability), drive runs
// unconditionally. The bool reports whether drive ran; the error is the acquisition error (when
// false) or drive's own error (when true).
//
// If the lease is lost while drive runs (see driveWithRenew for the renewal schedule), drive's
// context is cancelled with ErrLeaseLost as its cause (context.Cause), and a drive that then
// returns an error has it wrapped with ErrLeaseLost, so errors.Is(err, ErrLeaseLost) tells a lost
// lease from a shutdown or a genuine failure. A drive that returns nil succeeded regardless.
//
// A primary driver wraps Agent.Run so a live run and a recoverer never both drive it, using the
// same lease the recoverer respects:
//
//	driven, err := agent.Lease(ctx, store, runID, func(ctx context.Context) error {
//	    _, err := ag.Run(ctx, runID, input)
//	    return err
//	}, agent.WithLeaseHolder("worker-1"))
func Lease(ctx context.Context, store Durable, runID string, drive func(context.Context) error, opts ...RecoverOption) (bool, error) {
	cfg, err := leaseConfig(opts)
	if err != nil {
		return false, err
	}
	leaser, ok := Capability[Leaser](store)
	if !ok {
		return true, drive(ctx) // no leasing available: drive unconditionally
	}
	// The lease is claimed under a token of this call's own, not the bare holder name: a Leaser
	// grants a holder's own live lease again (a renewal), so two drivers sharing a name (a worker's
	// primary and its recoverer, or a restarted worker and its stalled predecessor) would otherwise
	// both hold the lease, renew it for each other, and release it from under each other.
	owner := cfg.holder + "#" + leaseToken()
	issued := time.Now() // the lease cannot expire before issued+ttl (see driveWithRenew)
	got, err := leaser.AcquireLease(ctx, runID, owner, cfg.ttl)
	if err != nil {
		return false, fmt.Errorf("acquire lease %s: %w (%w)", runID, err, ErrStorage)
	}
	if !got {
		return false, nil // another holder is driving it
	}
	// Release on return, including if drive panics or ctx was cancelled (a shutdown): the
	// release must still reach the store, so another node can take the run at once.
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = leaser.ReleaseLease(rctx, runID, owner)
	}()
	return true, driveWithRenew(ctx, leaser, runID, owner, cfg.ttl, issued, drive)
}

// leaseToken returns a random token that makes each Lease call's claim its own.
func leaseToken() string { return fmt.Sprintf("%016x", rand.Uint64()) }

// driveWithRenew runs the drive while renewing the lease, so a drive that outlasts the TTL keeps
// its lease. The drive is given a derived context that is cancelled if the lease is lost or the
// parent context is done, so a node that loses its lease stops driving at its next cancellation
// check. That is best effort, not mutual exclusion: a node stalled past the TTL (a long GC pause,
// a suspended VM, a partition from the store) wakes still driving and can take a step before its
// renewer notices. At-most-once does not depend on this; the exclusive attempt claim
// (ClaimAttempt) stops two overlapping drivers from both running a side effect.
//
// The renewal schedule, measured on this process's monotonic clock from the moment the last
// successful acquisition or renewal was issued (issued):
//
//   - The lease cannot expire before issued+ttl. The store sets the expiry from its own clock
//     while it serves the call, which is after the call was issued, and only the store compares
//     expiries, so the offset between the clocks does not matter.
//   - A renewal is first attempted at issued+ttl/2.
//   - A renewal that fails with an error (the store is briefly unreachable, say) is retried every
//     ttl/20 until the cutoff, issued+3*ttl/4. Each attempt is abandoned at the cutoff, so a
//     renewal that hangs cannot hold the drive past it.
//   - A renewal that reports the lease is no longer held cancels the drive at once, and so does
//     reaching the cutoff without a successful renewal.
//
// So the drive is cancelled at least ttl/4 before any other process can take the lease. That
// quarter is the margin for the cancellation to reach the drive and for the store's clock to run
// at a slightly different rate than this one; it does not cover a process that is stalled through
// the cutoff, which is the case the attempt claim exists for.
//
// When the drive returns (or panics), the renewer is stopped, abandoning any renewal it has in
// flight, and waited for before driveWithRenew returns, so no renewal outlives the drive or races
// the release that follows it.
func driveWithRenew(ctx context.Context, leaser Leaser, runID, owner string, ttl time.Duration, issued time.Time, run func(context.Context) error) error {
	dctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	rctx, stopRenew := context.WithCancel(ctx)
	renewerDone := make(chan struct{})
	defer func() {
		stopRenew()
		<-renewerDone
	}()
	go func() {
		defer close(renewerDone)
		if lost := renewLoop(rctx, leaser, runID, owner, ttl, issued); lost != nil {
			cancel(lost) // lost the lease: stop the drive rather than run un-leased
		}
	}()
	err := run(dctx)
	if cause := context.Cause(dctx); err != nil && errors.Is(cause, ErrLeaseLost) {
		return fmt.Errorf("%w: %w", cause, err)
	}
	return err
}

// renewLoop keeps owner's lease on runID renewed on the schedule driveWithRenew documents. It
// returns nil when ctx is done (the drive finished) and an error wrapping ErrLeaseLost when the
// lease is lost.
func renewLoop(ctx context.Context, leaser Leaser, runID, owner string, ttl time.Duration, issued time.Time) error {
	retry := max(ttl/20, 1)
	for {
		if !sleepUntil(ctx, issued.Add(ttl/2)) {
			return nil
		}
		cutoff := issued.Add(ttl - ttl/4)
		var lastErr error
		for {
			at := time.Now()
			if !at.Before(cutoff) {
				if lastErr == nil {
					lastErr = errors.New("no renewal was attempted before the cutoff: the process did not run in time")
				}
				return fmt.Errorf("lease on run %s not renewed within 3/4 of its %v TTL: %w: %w", runID, ttl, ErrLeaseLost, lastErr)
			}
			actx, cancel := context.WithDeadline(ctx, cutoff)
			ok, err := leaser.RenewLease(actx, runID, owner, ttl)
			cancel()
			if ctx.Err() != nil {
				return nil
			}
			if err == nil && ok {
				issued = at
				break
			}
			if err == nil {
				return fmt.Errorf("lease on run %s is no longer held (it lapsed and may have been taken): %w", runID, ErrLeaseLost)
			}
			lastErr = err
			next := time.Now().Add(retry)
			if next.After(cutoff) {
				next = cutoff
			}
			if !sleepUntil(ctx, next) {
				return nil
			}
		}
	}
}

// sleepUntil waits until t or until ctx is done, and reports whether it reached t.
func sleepUntil(ctx context.Context, t time.Time) bool {
	timer := time.NewTimer(time.Until(t))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// isPause reports whether err is a durable pause signal (the run recovered and is still
// waiting) rather than a genuine failure.
func isPause(err error) bool {
	var (
		pa  *PendingApproval
		itr *Interrupted
		slp *Sleeping
		awt *Awaiting
		rh  *ResumeHalt
	)
	return errors.As(err, &pa) || errors.As(err, &itr) || errors.As(err, &slp) || errors.As(err, &awt) || errors.As(err, &rh)
}

// ===========================================================================
// Leasing: single-driver coordination under HA
// ===========================================================================

// Leaser is the optional capability that coordinates driving a run across processes. Without it,
// several processes recovering against a shared store all re-drive the same in-flight runs. That is
// safe but wasteful: side effects stay at-most-once because each non-idempotent call is guarded by
// an exclusive attempt claim (ClaimAttempt), but the drivers duplicate model calls and a loser of a
// claim halts with ResumeHalt. A store that implements Leaser lets a driver take a time-bounded lease
// on a run so normally only the holder drives it; a dead holder's lease expires and another process
// takes over, which is the high-availability property. Recover uses it automatically when the store
// provides it. A lease is an efficiency and liveness mechanism, not the safety one: no lease can
// guarantee mutual exclusion against a holder that stalls past its TTL, which is why the claim, not
// the lease, is what keeps side effects at-most-once.
//
// The base Durable contract does not require leasing, and MemStore's implementation is in-process
// (for tests and as the reference); the cross-process payoff is a shared backend (store/postgres)
// implementing this with an atomic upsert over a leases table.
type Leaser interface {
	// AcquireLease claims runID for holder until now+ttl. It returns true if granted (the run is
	// unleased or the live lease is already holder's, which renews it), false if another holder
	// currently holds a live lease. An expired lease is available to any holder.
	AcquireLease(ctx context.Context, runID, holder string, ttl time.Duration) (bool, error)
	// RenewLease extends holder's lease on runID, returning false if holder no longer holds it (it
	// expired or was taken). A driver whose work outlasts ttl renews to keep the lease.
	RenewLease(ctx context.Context, runID, holder string, ttl time.Duration) (bool, error)
	// ReleaseLease relinquishes runID if held by holder (a no-op otherwise) so another process can
	// take it immediately rather than waiting for expiry.
	ReleaseLease(ctx context.Context, runID, holder string) error
}

// memLease is one in-memory lease: the current holder and when it expires.
type memLease struct {
	holder string
	expiry time.Time
}

// heldByOther reports whether the lease for runID is currently held by someone other than
// holder. The caller must hold m.mu.
func (m *MemStore) heldByOther(runID, holder string, now time.Time) bool {
	cur, ok := m.leases[runID]
	return ok && cur.holder != holder && now.Before(cur.expiry)
}

// AcquireLease implements Leaser.
func (m *MemStore) AcquireLease(_ context.Context, runID, holder string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if m.heldByOther(runID, holder, now) {
		return false, nil
	}
	m.leases[runID] = memLease{holder: holder, expiry: now.Add(ttl)}
	return true, nil
}

// RenewLease implements Leaser.
func (m *MemStore) RenewLease(_ context.Context, runID, holder string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	cur, ok := m.leases[runID]
	if !ok || cur.holder != holder || !now.Before(cur.expiry) {
		return false, nil // no longer ours (expired or taken)
	}
	m.leases[runID] = memLease{holder: holder, expiry: now.Add(ttl)}
	return true, nil
}

// ReleaseLease implements Leaser.
func (m *MemStore) ReleaseLease(_ context.Context, runID, holder string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.leases[runID]; ok && cur.holder == holder {
		delete(m.leases, runID)
	}
	return nil
}

// ===========================================================================
// Replay: deterministic re-emission of a journaled run
// ===========================================================================

// Replay returns a Model that re-emits the model outputs recorded in `source` for runID,
// in order, instead of calling a live LLM. Run an agent with it against a FRESH store to
// deterministically re-execute a past run:
//
//   - time-travel debugging: step through exactly what happened, offline and free;
//   - regression tests: capture a production run, replay it in CI (pairs with
//     testing/synctest), assert behavior didn't drift;
//   - evals over real traffic: the journal IS a golden dataset.
//
// Because the journal captures every model output across the whole (possibly nested)
// tree, the replay is exact. Spend is replayed too: a turn reports the usage the original turn
// discarded (failed attempts, losing hedge targets) along with its own, and a model call that
// failed for good in the original fails again at the same point, reporting the same usage, so
// the replayed run's Result.Spend, journal and WithTokenBudget stops match the original's. A
// middleware in the replaying agent that retries such a failure moves on to the next recorded
// response instead. This is only possible because the durable substrate records
// a complete, replayable history in the first place.
func Replay(ctx context.Context, source Durable, runID string) (Model, error) {
	recs, err := source.History(ctx, runID)
	if err != nil {
		return nil, err
	}
	var turns []recordedTurn
	for _, r := range recs {
		switch {
		case r.Kind == StepModel && r.Message != nil:
			t := recordedTurn{msg: *r.Message}
			if r.Usage != nil {
				t.usage = *r.Usage
			}
			if r.DiscardedUsage != nil {
				t.discarded = *r.DiscardedUsage
			}
			turns = append(turns, t)
		case strings.HasPrefix(r.Name, spendStepPrefix) && r.DiscardedUsage != nil:
			turns = append(turns, recordedTurn{usage: *r.DiscardedUsage, failed: true})
		}
	}
	return &replayModel{turns: turns}, nil
}

// recordedTurn is one journaled model call: its message, the usage it reported, and the usage
// of the requests its turn discarded. A failed one is a model call that failed for good, with the
// usage its requests reported.
type recordedTurn struct {
	msg              Message
	usage, discarded Usage
	failed           bool
}

// errReplayedFailure fails a replayed model call where the original call failed.
var errReplayedFailure = errors.New("replay: the recorded model call failed here")

type replayModel struct {
	turns []recordedTurn
	i     int
}

func (m *replayModel) Stream(ctx context.Context, _ Request) (*Stream, error) {
	if m.i >= len(m.turns) {
		return nil, fmt.Errorf("replay: %w", ErrNoRecordedOutput)
	}
	t := m.turns[m.i]
	m.i++

	if t.failed {
		// The usage, then the failure: the stream reports what the original call spent.
		ch := make(chan Emit, 2)
		ch <- Emit{Event: Finish{Reason: "stop", Usage: t.usage}}
		ch <- Emit{Err: errReplayedFailure}
		close(ch)
		return NewStream(ch), nil
	}
	if t.discarded != (Usage{}) {
		// The original turn's other requests were billed too: report them to the run as spend.
		for _, h := range modelHooks(ctx) {
			if h.After != nil {
				h.After(t.discarded)
			}
		}
	}
	evs := emitsFor(t.msg, t.usage)
	ch := make(chan Emit, len(evs))
	for _, e := range evs {
		ch <- e
	}
	close(ch)
	return NewStream(ch), nil
}

// emitsFor converts an assistant Message and the call's usage back into the stream events
// that would have produced them (the inverse of msgBuilder). Every message msgBuilder produces round-trips.
// A message it cannot produce may not: an unsigned thinking block stays open until a
// redacted block or the end of the stream, so one followed by text or another thinking
// block merges with it.
//
// The Finish carries u and a reason derived from the message: "tool_use" when it has tool
// calls, else "stop". The provider's own reason (such as a length cutoff) is not journaled:
// a ModelHandler returns only the message and usage, and middleware may answer with a
// message no stream produced.
func emitsFor(msg Message, u Usage) []Emit {
	var out []Emit
	idx := 0
	for _, p := range msg.Parts {
		switch v := p.(type) {
		case Reasoning:
			if v.Redacted != "" {
				out = append(out, Emit{Event: ReasoningDelta{Redacted: v.Redacted}})
				continue
			}
			// A block with no text and no signature still opens a thinking block.
			if v.Text != "" || v.Signature == "" {
				out = append(out, Emit{Event: ReasoningDelta{Text: v.Text}})
			}
			if v.Signature != "" {
				out = append(out, Emit{Event: ReasoningDelta{Signature: v.Signature}})
			}
		case Text:
			out = append(out, Emit{Event: TextDelta{Text: v.Text}})
		case ToolUse:
			out = append(out, Emit{Event: ToolCallDelta{Index: idx, ID: v.ID, Name: v.Name, ArgsFragment: v.Args, Signature: v.Signature}})
			idx++
		}
	}
	reason := "stop"
	if idx > 0 {
		reason = "tool_use"
	}
	return append(out, Emit{Event: Finish{Reason: reason, Usage: u}})
}
