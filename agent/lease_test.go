package agent

import (
	"context"
	"errors"
	"iter"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// seedRun records one step so the run exists (Lister.Runs returns it) and is incomplete
// (no completion marker), i.e. something Recover would try to re-drive.
func seedRun(t *testing.T, s *MemStore, id string) {
	t.Helper()
	// A run recovery drives has a run:start (a run with none is skipped and reported).
	if _, err := mustJournal(s).do(context.Background(), id, runStartStep, func(context.Context) (Record, error) {
		return Record{Kind: StepValue, Result: []byte(`{"input":"go"}`)}, nil
	}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
	if _, err := mustJournal(s).do(context.Background(), id, "seed", func(context.Context) (Record, error) {
		return Record{Kind: StepValue}, nil
	}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

// TestLease_ExclusiveAndExpiry checks the lease semantics with a controlled clock: a live lease is
// exclusive, an expired one is available, renewal fails once expired, and release frees it.
func TestLease_ExclusiveAndExpiry(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	clk := time.Unix(1000, 0)
	s.setNow(func() time.Time { return clk })
	ttl := time.Minute

	if ok, _ := s.AcquireLease(ctx, "r", "A", ttl); !ok {
		t.Fatal("A should acquire a free lease")
	}
	if ok, _ := s.AcquireLease(ctx, "r", "B", ttl); ok {
		t.Fatal("B must not acquire a lease A holds")
	}
	if ok, _ := s.AcquireLease(ctx, "r", "A", ttl); !ok {
		t.Fatal("A reacquiring its own live lease should renew")
	}

	clk = clk.Add(2 * ttl) // expire A's lease
	if ok, _ := s.RenewLease(ctx, "r", "A", ttl); ok {
		t.Fatal("A must not renew an expired lease")
	}
	if ok, _ := s.AcquireLease(ctx, "r", "B", ttl); !ok {
		t.Fatal("B should acquire an expired lease")
	}
	if err := s.ReleaseLease(ctx, "r", "A"); err != nil { // non-holder release is a no-op
		t.Fatalf("release: %v", err)
	}
	if ok, _ := s.AcquireLease(ctx, "r", "A", ttl); ok {
		t.Fatal("A must not acquire while B holds")
	}
	if err := s.ReleaseLease(ctx, "r", "B"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if ok, _ := s.AcquireLease(ctx, "r", "A", ttl); !ok {
		t.Fatal("A should acquire after B released")
	}
}

// TestRecover_SkipsLeasedByOther confirms Recover skips a run another holder currently leases (so
// competing recoverers do not both drive it) and drives and then releases a free run.
func TestRecover_SkipsLeasedByOther(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	j := mustJournal(s)
	seedRun(t, s, "r1")
	seedRun(t, s, "r2")

	if ok, _ := s.AcquireLease(ctx, "r1", "other", time.Hour); !ok {
		t.Fatal("setup: other should lease r1")
	}

	var mu sync.Mutex
	driven := map[string]bool{}
	resume := func(_ context.Context, id string, _ RunStart) error {
		mu.Lock()
		driven[id] = true
		mu.Unlock()
		return nil
	}

	n, err := Recover(ctx, j, resume, WithLeaseHolder("me"), WithLeaseTTL(time.Hour))
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n != 1 {
		t.Fatalf("recovered %d, want 1 (r1 is leased by another holder)", n)
	}
	if driven["r1"] {
		t.Fatal("r1 is leased by another holder and must not be driven")
	}
	if !driven["r2"] {
		t.Fatal("r2 is free and should be driven")
	}
	if ok, _ := s.AcquireLease(ctx, "r2", "someone", time.Hour); !ok {
		t.Fatal("r2's lease should have been released after Recover drove it")
	}
}

// TestLease_PrimarySkipsWhenHeld confirms a primary driver does not drive a run another holder
// currently leases, and drives (then releases) a free one, coordinating through the same lease a
// recoverer uses.
func TestLease_PrimarySkipsWhenHeld(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	j := mustJournal(s)
	if ok, _ := s.AcquireLease(ctx, "x", "worker-2", time.Hour); !ok {
		t.Fatal("setup: worker-2 should hold x")
	}

	ran := false
	driven, err := Lease(ctx, j, "x", func(context.Context) error { ran = true; return nil }, WithLeaseHolder("worker-1"))
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	if driven || ran {
		t.Fatal("worker-1 must not drive a run worker-2 holds")
	}

	driven, err = Lease(ctx, j, "y", func(context.Context) error { ran = true; return nil }, WithLeaseHolder("worker-1"))
	if err != nil || !driven || !ran {
		t.Fatalf("worker-1 should drive the free run y (driven=%v ran=%v err=%v)", driven, ran, err)
	}
	if ok, _ := s.AcquireLease(ctx, "y", "z", time.Hour); !ok {
		t.Fatal("y's lease should be released after the drive")
	}
}

// TestLease_NonLeaserStoreDrives confirms Lease drives unconditionally when the store does not
// implement Leaser (noListStore is a plain Store).
func TestLease_NonLeaserStoreDrives(t *testing.T) {
	ran := false
	driven, err := Lease(context.Background(), mustJournal(noListStore{}), "r", func(context.Context) error { ran = true; return nil })
	if err != nil || !driven || !ran {
		t.Fatalf("a non-Leaser store should drive unconditionally (driven=%v ran=%v err=%v)", driven, ran, err)
	}
}

// Two Lease calls under the same holder name are two drivers, and the lease must exclude one of
// them. A worker that gives its primary driver and its recoverer the same holder (the natural
// reading of WithLeaseHolder: one stable identity per worker) must not have Recover re-drive the
// run its primary is driving, nor have Recover's release free the lease under the primary.
func TestLease_SameHolderDoesNotDriveTwice(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	j := mustJournal(s)
	seedRun(t, s, "r")

	entered, finish := make(chan struct{}), make(chan struct{})
	primaryDone := make(chan error, 1)
	go func() {
		_, err := Lease(ctx, j, "r", func(context.Context) error {
			close(entered)
			<-finish
			return nil
		}, WithLeaseHolder("worker-1"), WithLeaseTTL(time.Hour))
		primaryDone <- err
	}()
	<-entered

	redriven := false
	n, err := Recover(ctx, j, func(context.Context, string, RunStart) error { redriven = true; return nil },
		WithLeaseHolder("worker-1"), WithLeaseTTL(time.Hour))
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if redriven || n != 0 {
		t.Fatalf("Recover under the same holder re-drove a run its primary is driving (recovered %d)", n)
	}
	if ok, _ := s.AcquireLease(ctx, "r", "worker-2", time.Hour); ok {
		t.Fatal("another worker took the lease while the primary was still driving: a same-holder driver released it")
	}
	if ok, _ := s.RenewLease(ctx, "r", "worker-1", time.Hour); ok {
		t.Fatal("a driver that never acquired this lease could renew it by holder name alone")
	}

	close(finish)
	if err := <-primaryDone; err != nil {
		t.Fatalf("primary Lease: %v", err)
	}
	if ok, _ := s.AcquireLease(ctx, "r", "worker-2", time.Hour); !ok {
		t.Fatal("the primary's lease was not released after its drive")
	}
}

// A drive that outlasts several renewal periods keeps its lease: the renewer renews the claim this
// Lease call made, so the drive is never cancelled while it still holds the run. It runs on a
// synctest clock, so how busy the machine is does not decide when the renewer wakes.
func TestLease_RenewsItsOwnClaim(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		driven, err := Lease(context.Background(), memJournal(), "r", func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				return errors.New("the drive was cancelled: its own lease was not renewed")
			case <-time.After(500 * time.Millisecond): // five TTLs
				return nil
			}
		}, WithLeaseHolder("worker-1"), WithLeaseTTL(100*time.Millisecond))
		if err != nil || !driven {
			t.Fatalf("Lease = (%v, %v), want (true, nil)", driven, err)
		}
	})
}

// notHeldLeaser is a MemStore whose RenewLease reports the lease no longer held, as a store does
// once the lease lapsed and another process took it.
type notHeldLeaser struct{ *MemStore }

func (notHeldLeaser) RenewLease(context.Context, string, string, time.Duration) (bool, error) {
	return false, nil
}

// waitForCancel is a drive that runs until its context is cancelled and records the cause.
func waitForCancel(cause *error) func(context.Context) error {
	return func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			*cause = context.Cause(ctx)
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return errors.New("the drive was never cancelled")
		}
	}
}

// A drive whose lease is lost, whether taken by another process or not renewable before the
// cutoff, is cancelled with ErrLeaseLost as the cause, and Lease returns the drive's error wrapped
// with it, so a caller can tell a lost lease from a shutdown or a genuine failure.
//
// It runs on a synctest clock: with a real one, a renewer that wakes late on a busy machine (or a
// coarse Windows timer) reaches the cutoff without having tried the store at all.
func TestLease_LostLeaseEndsWithErrLeaseLost(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store func() *Journal
	}{
		{"taken", func() *Journal { return mustJournal(notHeldLeaser{NewMemStore()}) }},
		{"not renewable in time", func() *Journal { return mustJournal(&failingRenewLeaser{MemStore: NewMemStore(), failures: -1}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := tc.store()
				var cause error
				driven, err := Lease(context.Background(), s, "r", waitForCancel(&cause), WithLeaseTTL(40*time.Millisecond))
				if !driven {
					t.Fatal("the drive did not run")
				}
				if !errors.Is(cause, ErrLeaseLost) {
					t.Fatalf("the drive's context was cancelled with cause %v, want ErrLeaseLost", cause)
				}
				if !errors.Is(err, ErrLeaseLost) || !errors.Is(err, context.Canceled) {
					t.Fatalf("Lease returned %v, want the drive's error (context.Canceled) wrapped with ErrLeaseLost", err)
				}
				if _, failing := s.Store().(*failingRenewLeaser); failing && !errors.Is(cause, errStoreDown) {
					t.Fatalf("the cause %v does not carry the store error that kept the lease from being renewed", cause)
				}
			})
		})
	}
}

// A drive cancelled by its caller (a shutdown) did not lose its lease, and a drive that finished
// despite losing its lease succeeded: neither reports ErrLeaseLost.
func TestLease_ErrLeaseLostOnlyForALostLease(t *testing.T) {
	synctest.Test(t, testErrLeaseLostOnlyForALostLease)
}

func testErrLeaseLostOnlyForALostLease(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var cause error
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	if _, err := Lease(ctx, memJournal(), "r", waitForCancel(&cause), WithLeaseTTL(time.Hour)); errors.Is(err, ErrLeaseLost) || errors.Is(cause, ErrLeaseLost) {
		t.Fatalf("a caller's cancellation was reported as a lost lease (err %v, cause %v)", err, cause)
	}
	driven, err := Lease(context.Background(), mustJournal(notHeldLeaser{NewMemStore()}), "r", func(ctx context.Context) error {
		<-ctx.Done()
		return nil // the work finished anyway
	}, WithLeaseTTL(40*time.Millisecond))
	if !driven || err != nil {
		t.Fatalf("Lease = (%v, %v), want (true, nil) for a drive that finished", driven, err)
	}
}

// Recover counts a run whose lease was lost mid-drive as recovered, not failed: another process
// holds it now and carries it on.
func TestRecover_LostLeaseIsNotAFailure(t *testing.T) {
	synctest.Test(t, testRecoverLostLeaseIsNotAFailure)
}

func testRecoverLostLeaseIsNotAFailure(t *testing.T) {
	s := notHeldLeaser{NewMemStore()}
	j := mustJournal(s)
	seedRun(t, s.MemStore, "r")
	var cause error
	n, err := Recover(context.Background(), j, func(ctx context.Context, _ string, _ RunStart) error { return waitForCancel(&cause)(ctx) },
		WithLeaseTTL(40*time.Millisecond))
	if err != nil {
		t.Fatalf("Recover = %v, want nil: a lost lease is not a failure", err)
	}
	if n != 1 {
		t.Fatalf("Recover recovered %d runs, want 1", n)
	}
}

// ctxLeaser is a MemStore whose ReleaseLease, like a SQL store's, fails on a cancelled context.
type ctxLeaser struct{ *MemStore }

func (l ctxLeaser) ReleaseLease(ctx context.Context, runID, holder string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return l.MemStore.ReleaseLease(ctx, runID, holder)
}

// A drive stopped by cancellation (a shutdown) still releases its lease, so another node can
// take the run at once instead of waiting out the TTL.
func TestLease_ReleasedWhenTheDriveIsCancelled(t *testing.T) {
	store := ctxLeaser{NewMemStore()}
	j := mustJournal(store)
	ctx, cancel := context.WithCancel(context.Background())
	_, _ = Lease(ctx, j, "r1", func(context.Context) error {
		cancel() // shutdown arrives mid-drive
		return context.Canceled
	}, WithLeaseHolder("node-a"), WithLeaseTTL(time.Hour))
	got, err := store.AcquireLease(context.Background(), "r1", "node-b", time.Hour)
	if err != nil || !got {
		t.Fatalf("another node could not take the run after the drive stopped (got=%v, err=%v): the lease was never released", got, err)
	}
}

// blockingRenewLeaser is a MemStore whose RenewLease hangs until its context is done, like a SQL
// store's renewal stuck on a slow or partitioned database, and then takes a moment to return. It
// signals entered when a renewal starts and records whether a renewal was still in flight when the
// lease was released.
type blockingRenewLeaser struct {
	*MemStore
	entered        chan struct{} // closed when the first renewal starts
	once           sync.Once
	inRenew        atomic.Int32 // renewals in flight
	renewAtRelease bool
}

func (l *blockingRenewLeaser) RenewLease(ctx context.Context, _, _ string, _ time.Duration) (bool, error) {
	l.inRenew.Add(1)
	defer l.inRenew.Add(-1)
	l.once.Do(func() { close(l.entered) })
	<-ctx.Done()
	time.Sleep(20 * time.Millisecond) // a store call takes a moment to return after its cancellation
	return false, ctx.Err()
}

func (l *blockingRenewLeaser) ReleaseLease(ctx context.Context, runID, holder string) error {
	l.renewAtRelease = l.inRenew.Load() > 0
	return l.MemStore.ReleaseLease(ctx, runID, holder)
}

// The renewal goroutine is stopped, with any renewal it has in flight abandoned, before Lease
// releases the lease and returns. Otherwise a renewal stuck on the store outlives the drive (a
// leaked goroutine per Lease call for as long as the store hangs) and can land after the release,
// which a store whose renewal is not conditional on the holder would turn back into a lease.
//
// It runs in a synctest bubble, so the TTL, the renewal schedule and the store's delay run on the
// bubble's fake clock, and the drive ends only once the renewal is in flight: the outcome does not
// depend on how the machine schedules goroutines.
func TestLease_RenewerStopsBeforeRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &blockingRenewLeaser{MemStore: NewMemStore(), entered: make(chan struct{})}
		j := mustJournal(s)
		const ttl = 400 * time.Millisecond
		start := time.Now()
		// The renewal starts at ttl/2 and hangs; the drive ends as soon as it has started, well
		// before the renewal cutoff (3/4 of the TTL) would abandon it, so only stopping the
		// renewer ends it. A renewer that nothing stops leaves the bubble deadlocked, which
		// synctest reports as a failure.
		driven, err := Lease(context.Background(), j, "r", func(context.Context) error {
			<-s.entered
			return nil
		}, WithLeaseHolder("a"), WithLeaseTTL(ttl))
		elapsed := time.Since(start)
		if err != nil || !driven {
			t.Fatalf("Lease = (%v, %v), want (true, nil)", driven, err)
		}
		if want := ttl/2 + 20*time.Millisecond; elapsed != want {
			t.Fatalf("Lease returned %v after it started, want %v: the drive ended at ttl/2 and the stopped renewal took 20ms to return", elapsed, want)
		}
		if s.renewAtRelease {
			t.Fatal("the lease was released while a renewal was still in flight")
		}
		if s.inRenew.Load() > 0 {
			t.Fatal("a renewal is still in flight after Lease returned: the renewal goroutine outlived the drive")
		}
	})
}

// failingRenewLeaser is a MemStore whose RenewLease fails its first failures calls with a store
// error (forever when failures < 0), or hangs until its context is done when hang is set.
type failingRenewLeaser struct {
	*MemStore
	failures int64
	hang     bool
	calls    atomic.Int64
}

var errStoreDown = errors.New("store unreachable")

func (l *failingRenewLeaser) RenewLease(ctx context.Context, runID, holder string, ttl time.Duration) (bool, error) {
	n := l.calls.Add(1)
	if l.hang {
		<-ctx.Done()
		return false, ctx.Err()
	}
	if l.failures < 0 || n <= l.failures {
		return false, errStoreDown
	}
	return l.MemStore.RenewLease(ctx, runID, holder, ttl)
}

// driveFor returns a drive that runs for d unless its context is cancelled first, and reports
// how long after start it stopped and whether it was cancelled.
func driveFor(d time.Duration, stopped *time.Duration, cancelled *bool) func(context.Context) error {
	return func(ctx context.Context) error {
		start := time.Now()
		select {
		case <-ctx.Done():
			*stopped, *cancelled = time.Since(start), true
			return ctx.Err()
		case <-time.After(d):
			*stopped = time.Since(start)
			return nil
		}
	}
}

// A renewal that fails with a store error is retried while there is still time to renew before
// the lease could expire, so one dropped connection does not cancel a drive that still holds its
// lease.
func TestLease_RetriesAFailedRenewal(t *testing.T) { synctest.Test(t, testRetriesAFailedRenewal) }

func testRetriesAFailedRenewal(t *testing.T) {
	const ttl = 400 * time.Millisecond
	s := &failingRenewLeaser{MemStore: NewMemStore(), failures: 2}
	j := mustJournal(s)
	var stopped time.Duration
	var cancelled bool
	driven, err := Lease(context.Background(), j, "r", driveFor(2*ttl, &stopped, &cancelled),
		WithLeaseHolder("a"), WithLeaseTTL(ttl))
	if !driven || err != nil || cancelled {
		t.Fatalf("Lease = (%v, %v), cancelled after %v: two failed renewals, with time left to retry, cancelled the drive", driven, err, stopped)
	}
	// Renewals are due every ttl/2 and a failed one is retried every ttl/20, so over 2*ttl the
	// renewer tries at 200ms (fails), 220ms (fails), 240ms, 440ms and 640ms; the drive ends at 800ms.
	if n := s.calls.Load(); n != 5 {
		t.Fatalf("RenewLease called %d times, want 5 (two failures, then a success every ttl/2)", n)
	}
}

// A renewal that keeps failing, or hangs, is given up at 3/4 of the TTL after the last renewal
// was issued: the drive is cancelled a quarter of the TTL before any other process could take the
// lease, not when the lease has already lapsed (or never, for a renewal that hangs). On a synctest
// clock the cancellation lands exactly at the cutoff.
func TestLease_GivesUpBeforeTheLeaseCanExpire(t *testing.T) {
	const ttl = 400 * time.Millisecond
	for _, tc := range []struct {
		name  string
		store func() *failingRenewLeaser
		calls int64 // renewals tried before the cutoff
	}{
		// Tries at 200ms and every 20ms after, the last at 280ms.
		{"failing", func() *failingRenewLeaser { return &failingRenewLeaser{MemStore: NewMemStore(), failures: -1} }, 5},
		// One try at 200ms, abandoned at the cutoff.
		{"hanging", func() *failingRenewLeaser { return &failingRenewLeaser{MemStore: NewMemStore(), hang: true} }, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := tc.store()
				j := mustJournal(s)
				var stopped time.Duration
				var cancelled bool
				start := time.Now()
				driven, _ := Lease(context.Background(), j, "r", driveFor(3*ttl, &stopped, &cancelled),
					WithLeaseHolder("a"), WithLeaseTTL(ttl))
				elapsed := time.Since(start)
				if !driven || !cancelled {
					t.Fatalf("the drive was not cancelled (driven=%v) although its lease could not be renewed; it ran %v", driven, stopped)
				}
				if want := ttl * 3 / 4; elapsed != want {
					t.Fatalf("the drive was cancelled %v after the lease was acquired, want exactly the cutoff, %v (before its %v TTL)", elapsed, want, ttl)
				}
				if n := s.calls.Load(); n != tc.calls {
					t.Fatalf("RenewLease called %d times before the cutoff, want %d", n, tc.calls)
				}
			})
		})
	}
}

// A lease TTL that is not positive is a configuration error, reported before anything is driven.
// The renewer ticks every ttl/2, and time.NewTicker panics on a non-positive interval inside the
// renewal goroutine, where no caller can recover it: the whole process dies.
func TestLease_RejectsNonPositiveTTL(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Second, time.Nanosecond} {
		ctx := context.Background()
		s := &countingLister{MemStore: NewMemStore()}
		j := mustJournal(s)
		ran := false
		driven, err := Lease(ctx, j, "r", func(context.Context) error { ran = true; return nil }, WithLeaseTTL(ttl))
		if ttl <= 0 {
			if !errors.Is(err, ErrConfig) || driven || ran {
				t.Fatalf("ttl %v: Lease = (%v, %v), ran=%v; want (false, ErrConfig) and no drive", ttl, driven, err, ran)
			}
			seedRun(t, s.MemStore, "r")
			if _, err := Recover(ctx, j, func(context.Context, string, RunStart) error { ran = true; return nil }, WithLeaseTTL(ttl)); !errors.Is(err, ErrConfig) || ran || s.listed {
				t.Fatalf("ttl %v: Recover err = %v, ran=%v, listed runs=%v; want ErrConfig before touching the store", ttl, err, ran, s.listed)
			}
			continue
		}
		// A positive TTL too short to halve still drives (and renews) without panicking.
		if err != nil || !driven || !ran {
			t.Fatalf("ttl %v: Lease = (%v, %v), ran=%v; want the drive to run", ttl, driven, err, ran)
		}
	}
}

// countingLister is a MemStore that records whether its runs were listed.
type countingLister struct {
	*MemStore
	listed bool
}

func (l *countingLister) Runs(ctx context.Context, f RunFilter) iter.Seq2[string, error] {
	l.listed = true
	return l.MemStore.Runs(ctx, f)
}
