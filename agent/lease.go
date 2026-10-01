// lease.go holds run leasing: the Leaser capability, MemStore's in-process leases, and Lease,
// which drives a run under a renewed lease. Recover uses it; see recovery.go.

package agent

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"time"
)

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
// The Store port does not require leasing. MemStore's implementation is in-process (for tests and
// as the reference), SQLite's coordinates the processes sharing one database file, and Postgres's
// the nodes sharing one database, each with an atomic upsert over a leases table.
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
	// ReapLeases deletes every lapsed lease (see RunFilter.LeaseLapsed) whose run the store holds
	// no entry for, or holds an entry named in ended (a finished run), and returns how many it
	// deleted. Each lease is deleted only if it is still lapsed when it is deleted, by the
	// comparison AcquireLease makes, so a lease granted or renewed meanwhile is kept: deleting a
	// lapsed lease changes nothing a holder can observe, since any holder may take it. A holder that
	// dies between its run's last write and its release leaves such a lease; RecoverLoop's lapsed
	// loop calls ReapLeases with the terminal markers on each pass, so these leases do not
	// accumulate and the lapsed listing does not grow with them.
	ReapLeases(ctx context.Context, ended []string) (int, error)
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

// lapsed reports whether runID holds a lease that has expired at now: one AcquireLease would grant
// to any holder (see RunFilter.LeaseLapsed). The caller must hold m.mu.
func (m *MemStore) lapsed(runID string, now time.Time) bool {
	cur, ok := m.leases[runID]
	return ok && !now.Before(cur.expiry)
}

// ReapLeases implements Leaser.
func (m *MemStore) ReapLeases(_ context.Context, ended []string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	now := m.now()
	n := 0
	for id := range m.leases {
		if !m.lapsed(id, now) {
			continue
		}
		r, ok := m.runs[id]
		if ok && !slices.ContainsFunc(ended, func(name string) bool { _, has := r.byName[name]; return has }) {
			continue // an unfinished run: the lapsed loop takes it over
		}
		delete(m.leases, id)
		n++
	}
	return n, nil
}

// AcquireLease implements Leaser.
func (m *MemStore) AcquireLease(_ context.Context, runID, holder string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
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
	m.init()
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

// protocol:lifecycle begin DIdle DRel Tick LeaseNotice

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
func Lease(ctx context.Context, store Durable, runID string, drive func(context.Context) error, opts ...LeaseOption) (bool, error) {
	cfg, err := leaseConfig("Lease", opts, LeaseOption.applyLease)
	if err != nil {
		return false, err
	}
	return leaseRun(ctx, store, runID, drive, cfg)
}

// leaseRun is Lease under a configuration leaseConfig validated: a recovery pass calls it for
// each run with its own, so it builds no options per run.
func leaseRun(ctx context.Context, store Durable, runID string, drive func(context.Context) error, cfg recoverConfig) (bool, error) {
	leaser, ok := capabilityOf[Leaser](store)
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
func leaseToken() string {
	var b [16]byte
	const hex = "0123456789abcdef"
	v := rand.Uint64()
	for i := 15; i >= 0; i-- {
		b[i] = hex[v&0xf]
		v >>= 4
	}
	return string(b[:])
}

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
	renewerDone := make(chan struct{})
	// The renewer starts at the first renewal (issued+ttl/2), not with the drive: a drive that ends
	// before then (most recovery visits: a halted run, a run over since the listing) starts no
	// goroutine. It runs under dctx, which the drive's end cancels.
	renewer := time.AfterFunc(time.Until(issued.Add(ttl/2)), func() {
		defer close(renewerDone)
		if lost := renewLoop(dctx, leaser, runID, owner, ttl, issued); lost != nil {
			cancel(lost) // lost the lease: stop the drive rather than run un-leased
		}
	})
	defer func() {
		cancel(nil)
		if !renewer.Stop() {
			<-renewerDone // it started: wait for it to see dctx done
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

// protocol:lifecycle end

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
