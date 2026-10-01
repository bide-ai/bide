package agent

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// Finding L1 of model 10 (spec/tla/README.md, "Recovery cost: the pickup bound"): a RecoverLoop
// pass visits every unfinished run it lists, halted ones included, one slot at a time, so a dead
// holder's run listed after halted runs waited for their visits, and a lease that lapsed just
// after the pass tried the run waited for the whole next pass. A dead holder's run must be taken
// over within one interval of its lease lapsing, however many halted runs are listed before it.
//
// The halted runs h1..hH sort before the dead holder's run z, as older runs do; each visit to a
// halted run takes one interval (its resume replays to the halt and pauses again), and the full
// pass drives one run at a time. The dead holder's lease lapses at every phase of the pass, in
// steps of a quarter interval, and the takeover is measured from the lapse to the moment resume
// is called for z, on the bubble's clock.
func TestRecoverLoop_TakesOverALapsedLeaseWithinAnIntervalBehindHaltedRuns(t *testing.T) {
	const (
		interval = 20 * time.Millisecond
		visit    = interval // one halted run's visit
		step     = interval / 4
	)
	for halted := 0; halted <= 3; halted++ {
		for phase := time.Duration(0); phase <= time.Duration(halted+2)*visit; phase += step {
			t.Run(fmt.Sprintf("halted=%d/lapse=+%v", halted, phase), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					if d := lapsedTakeover(t, halted, visit, interval, 100*time.Millisecond+phase); d > interval {
						t.Fatalf("the dead holder's run was taken over %v after its lease lapsed, behind %d halted runs; want within one interval (%v)", d, halted, interval)
					}
				})
			})
		}
	}
}

// lapsedTakeover runs RecoverLoop at concurrency 1 over halted runs h1..hN and a run z whose dead
// holder's lease lapses deadTTL after the loop starts, and returns how long after the lapse the
// loop called resume for z.
func lapsedTakeover(t *testing.T, halted int, visit, interval, deadTTL time.Duration) time.Duration {
	t.Helper()
	ctx := context.Background()
	s := NewMemStore()
	for i := 1; i <= halted; i++ {
		seedRun(t, s, fmt.Sprintf("h%d", i))
	}
	seedRun(t, s, "z")
	if ok, _ := s.AcquireLease(ctx, "z", "dead-worker#0", deadTTL); !ok {
		t.Fatal("setup: the dead worker should hold z")
	}
	lapse := time.Now().Add(deadTTL)
	drivenAt := make(chan time.Time, 1)
	stop := runLoop(t, s, func(ctx context.Context, id string) error {
		if id != "z" {
			time.Sleep(visit)
			return &OutcomeUnknown{RunRef: RunRef{RunID: id}} // halted: stays unfinished
		}
		select {
		case drivenAt <- time.Now():
		default:
		}
		_, err := s.Do(ctx, id, runCompleteStep, func(context.Context) (Record, error) { return Record{Kind: StepValue}, nil })
		return err
	}, WithRecoverInterval(interval), WithRecoverConcurrency(1), WithLeaseTTL(10*interval))
	defer stop()
	select {
	case at := <-drivenAt:
		if at.Before(lapse) {
			t.Fatalf("the loop drove z %v before its dead holder's lease lapsed", lapse.Sub(at))
		}
		return at.Sub(lapse)
	case <-time.After(time.Minute):
		t.Fatal("the loop never took over the dead holder's run")
		return 0
	}
}

// A run the full pass is waiting to start (every slot of the full pass busy) whose lease lapses
// meanwhile is driven by the lapsed loop, and when the full pass gets its slot it sees the run in
// flight and does not try it again: one lease acquisition for the run, and the drive's entry stays
// marked in flight until the drive returns.
func TestRecoverLoop_LoopsDoNotStartARunTheOtherIsDriving(t *testing.T) {
	synctest.Test(t, testRecoverLoopLoopsDoNotStartARunTheOtherIsDriving)
}

func testRecoverLoopLoopsDoNotStartARunTheOtherIsDriving(t *testing.T) {
	ctx := context.Background()
	s := &countingStore{MemStore: NewMemStore()}
	seedRun(t, s.MemStore, "h")
	seedRun(t, s.MemStore, "z")
	if ok, _ := s.MemStore.AcquireLease(ctx, "z", "dead-worker#0", 30*time.Millisecond); !ok {
		t.Fatal("setup: the dead worker should hold z")
	}
	release := make(chan struct{})
	zStarted := make(chan struct{})
	stop := runLoop(t, s, func(ctx context.Context, id string) error {
		if id == "h" {
			time.Sleep(50 * time.Millisecond) // holds the full pass's only slot past z's lapse
			return &OutcomeUnknown{RunRef: RunRef{RunID: id}}
		}
		close(zStarted)
		<-release
		return nil
	}, WithRecoverInterval(20*time.Millisecond), WithRecoverConcurrency(1), WithLeaseTTL(time.Second))
	defer stop()
	<-zStarted                        // the lapsed loop's pass at 40ms
	time.Sleep(30 * time.Millisecond) // the full pass gets its slot back at 50ms and reaches z
	if n := s.acquires("z"); n != 1 {
		t.Fatalf("z was leased %d times while the lapsed loop drove it, want once", n)
	}
	time.Sleep(100 * time.Millisecond) // later passes of both loops
	if n := s.acquires("z"); n != 1 {
		t.Fatalf("z was leased %d times while the lapsed loop drove it, want once", n)
	}
	close(release)
}

// A run whose lease lapsed while this process still drives it (a drive that ignores the
// cancellation its lost lease caused) is not started again by the lapsed loop.
func TestRecoverLoop_LapsedLoopSkipsARunItsProcessIsDriving(t *testing.T) {
	synctest.Test(t, testRecoverLoopLapsedLoopSkipsARunItsProcessIsDriving)
}

func testRecoverLoopLapsedLoopSkipsARunItsProcessIsDriving(t *testing.T) {
	s := &failRenewStore{countingStore{MemStore: NewMemStore()}}
	seedRun(t, s.MemStore, "r")
	release := make(chan struct{})
	started := make(chan struct{})
	stop := runLoop(t, s, func(ctx context.Context, id string) error {
		close(started)
		<-release // ignores ctx: still driving after the lease lapsed
		return nil
	}, WithRecoverInterval(20*time.Millisecond), WithLeaseTTL(100*time.Millisecond))
	defer stop()
	<-started
	time.Sleep(300 * time.Millisecond) // the lease lapsed at 100ms; lapsed passes every 20ms since
	if !s.MemStore.lapsedNow("r") {
		t.Fatal("setup: r's lease should have lapsed")
	}
	if n := s.acquires("r"); n != 1 {
		t.Fatalf("r was leased %d times while this process drove it, want once", n)
	}
	close(release)
}

// failRenewStore is a countingStore whose renewals fail, so a drive's lease lapses at its TTL.
type failRenewStore struct{ countingStore }

func (*failRenewStore) RenewLease(context.Context, string, string, time.Duration) (bool, error) {
	return false, errors.New("renewal refused")
}

// lapsedNow reports whether runID's lease has lapsed on m's clock.
func (m *MemStore) lapsedNow(runID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	return m.lapsed(runID, m.now())
}

// Over a store that implements Lister but not Leaser there are no leases to lapse, and RecoverLoop
// runs no lapsed loop: every listing is the full pass's.
func TestRecoverLoop_NoLapsedLoopWithoutLeaser(t *testing.T) {
	synctest.Test(t, testRecoverLoopNoLapsedLoopWithoutLeaser)
}

func testRecoverLoopNoLapsedLoopWithoutLeaser(t *testing.T) {
	s := &unleasedStore{m: NewMemStore()}
	stop := runLoop(t, s, func(context.Context, string) error { return nil }, WithRecoverInterval(10*time.Millisecond))
	time.Sleep(55 * time.Millisecond)
	_ = stop()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.full != 6 || s.lapsed != 0 {
		t.Fatalf("%d full and %d lapsed listings in 55ms, want 6 and 0", s.full, s.lapsed)
	}
}

// unleasedStore is a Durable that implements Lister and not Leaser, counting its listings.
type unleasedStore struct {
	m            *MemStore
	mu           sync.Mutex
	full, lapsed int
}

func (u *unleasedStore) Do(ctx context.Context, runID, name string, fn func(context.Context) (Record, error)) (Record, error) {
	return u.m.Do(ctx, runID, name, fn)
}

func (u *unleasedStore) History(ctx context.Context, runID string) ([]Record, error) {
	return u.m.History(ctx, runID)
}

func (u *unleasedStore) Runs(ctx context.Context, f RunFilter) iter.Seq2[string, error] {
	u.mu.Lock()
	if f.LeaseLapsed {
		u.lapsed++
	} else {
		u.full++
	}
	u.mu.Unlock()
	return u.m.Runs(ctx, f)
}

// The lapsed loop drives only the unfinished runs whose lease lapsed: while the full pass's only
// slot is taken, it leaves alone a run nobody leases, a run whose lease is live, and a finished run
// whose holder died before releasing its lease.
func TestRecoverLoop_LapsedLoopDrivesOnlyLapsedUnfinishedRuns(t *testing.T) {
	synctest.Test(t, testRecoverLoopLapsedLoopDrivesOnlyLapsedUnfinishedRuns)
}

func testRecoverLoopLapsedLoopDrivesOnlyLapsedUnfinishedRuns(t *testing.T) {
	ctx := context.Background()
	s := &countingStore{MemStore: NewMemStore()}
	for _, id := range []string{"a", "done", "live", "none"} {
		seedRun(t, s.MemStore, id)
	}
	if _, err := s.Do(ctx, "done", runCompleteStep, func(context.Context) (Record, error) { return Record{Kind: StepValue}, nil }); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.MemStore.AcquireLease(ctx, "done", "dead-worker#0", time.Millisecond); !ok {
		t.Fatal("setup: the dead worker should hold done")
	}
	if ok, _ := s.MemStore.AcquireLease(ctx, "live", "other-worker#0", time.Hour); !ok {
		t.Fatal("setup: the other worker should hold live")
	}
	release := make(chan struct{})
	stop := runLoop(t, s, func(ctx context.Context, id string) error {
		if id == "a" {
			<-release // holds the full pass's only slot
		}
		return nil
	}, WithRecoverInterval(20*time.Millisecond), WithRecoverConcurrency(1))
	defer stop()
	time.Sleep(200 * time.Millisecond) // ten passes of the lapsed loop
	for _, id := range []string{"done", "live", "none"} {
		if n := s.acquires(id); n != 0 {
			t.Errorf("run %s was leased %d times while the full pass was busy, want none: the lapsed loop drove it", id, n)
		}
	}
	close(release)
}

// MemStore's lapsed listing agrees with AcquireLease at the expiry instant: a lease is lapsed from
// the moment another holder can take it.
func TestMemStore_LeaseLapsedAtExpiry(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	seedRun(t, s, "r")
	t0 := time.Unix(1000, 0)
	s.setNow(func() time.Time { return t0 })
	if ok, _ := s.AcquireLease(ctx, "r", "dead#0", time.Second); !ok {
		t.Fatal("setup: dead#0 should hold r")
	}
	list := func() []string {
		var out []string
		for id, err := range s.Runs(ctx, RunFilter{LeaseLapsed: true}) {
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, id)
		}
		return out
	}
	s.setNow(func() time.Time { return t0.Add(time.Second - 1) })
	if got := list(); len(got) != 0 {
		t.Fatalf("a nanosecond before expiry, Runs(LeaseLapsed) = %v, want none", got)
	}
	s.setNow(func() time.Time { return t0.Add(time.Second) })
	if got := list(); len(got) != 1 {
		t.Fatalf("at expiry, Runs(LeaseLapsed) = %v, want [r]", got)
	}
	if ok, _ := s.AcquireLease(ctx, "r", "new#0", time.Second); !ok {
		t.Fatal("at expiry AcquireLease refused a lease Runs(LeaseLapsed) listed as lapsed")
	}
}

// A pass skips a run this process is already driving without waiting for a slot: with the only
// slot held by that run's long drive, the full pass still ends at once and the next one starts on
// schedule.
func TestRecoverLoop_PassDoesNotWaitForASlotForARunInFlight(t *testing.T) {
	synctest.Test(t, testRecoverLoopPassDoesNotWaitForASlotForARunInFlight)
}

func testRecoverLoopPassDoesNotWaitForASlotForARunInFlight(t *testing.T) {
	s := &countingStore{MemStore: NewMemStore()}
	seedRun(t, s.MemStore, "a")
	release := make(chan struct{})
	stop := runLoop(t, s, func(ctx context.Context, id string) error {
		<-release
		return nil
	}, WithRecoverInterval(20*time.Millisecond), WithRecoverConcurrency(1))
	defer stop()
	time.Sleep(105 * time.Millisecond)
	s.mu.Lock()
	lists := s.lists
	s.mu.Unlock()
	if lists != 6 {
		t.Fatalf("%d full passes in 105ms with a 20ms interval, want 6: a pass waited for a slot for the run already in flight", lists)
	}
	close(release)
}

// A failed ReapLeases is a genuine failure: it goes to the WithRecoverErrors handler as a storage
// error, and the lapsed loop still lists and drives the lapsed runs.
func TestRecoverLoop_ReportsAFailedReap(t *testing.T) {
	synctest.Test(t, testRecoverLoopReportsAFailedReap)
}

func testRecoverLoopReportsAFailedReap(t *testing.T) {
	s := &failReapStore{countingStore{MemStore: NewMemStore()}}
	seedRun(t, s.MemStore, "z")
	if ok, _ := s.MemStore.AcquireLease(context.Background(), "z", "dead-worker#0", time.Millisecond); !ok {
		t.Fatal("setup: the dead worker should hold z")
	}
	var mu sync.Mutex
	var errs []error
	driven := make(chan struct{})
	var once sync.Once
	stop := runLoop(t, s, func(context.Context, string) error {
		once.Do(func() { close(driven) })
		return &OutcomeUnknown{RunRef: RunRef{RunID: "z"}}
	}, WithRecoverInterval(20*time.Millisecond), WithRecoverErrors(func(err error) {
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}))
	defer stop()
	<-driven
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(errs) == 0 || !errors.Is(errs[0], ErrStorage) || !errors.Is(errs[0], errReap) {
		t.Fatalf("reported %v, want the reap failure wrapped as ErrStorage", errs)
	}
}

var errReap = errors.New("reap refused")

// failReapStore is a countingStore whose ReapLeases fails.
type failReapStore struct{ countingStore }

func (*failReapStore) ReapLeases(context.Context, []string) (int, error) { return 0, errReap }
