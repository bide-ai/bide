package agent

import (
	"context"
	"errors"
	"iter"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// Every RecoverLoop test runs in a synctest bubble, so pass intervals, lease expiries and drive
// durations run on the bubble's clock and do not depend on how busy the machine is.

// runLoop runs RecoverLoop in the background and returns a function that stops it and reports
// what it returned.
func runLoop(t *testing.T, s Durable, resume Resumer, opts ...RecoverLoopOption) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RecoverLoop(ctx, s, resume, opts...) }()
	return func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("RecoverLoop did not return after its context was cancelled")
			return nil
		}
	}
}

// A run whose holder died is taken over by the loop once the dead holder's lease expires, with no
// further call from the caller, and not before.
func TestRecoverLoop_TakesOverAfterTheHolderDies(t *testing.T) {
	synctest.Test(t, testRecoverLoopTakesOverAfterTheHolderDies)
}

func testRecoverLoopTakesOverAfterTheHolderDies(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	seedRun(t, s, "r")
	const deadTTL = 150 * time.Millisecond
	died := time.Now()
	if ok, _ := s.AcquireLease(ctx, "r", "dead-worker#0", deadTTL); !ok {
		t.Fatal("setup: the dead worker should hold r")
	}
	drivenAt := make(chan time.Time, 1)
	stop := runLoop(t, s, func(ctx context.Context, id string, _ RunStart) error {
		select {
		case drivenAt <- time.Now():
		default:
		}
		_, err := s.Do(ctx, id, runCompleteStep, func(context.Context) (Record, error) { return Record{Kind: StepValue}, nil })
		return err
	}, WithRecoverInterval(20*time.Millisecond))
	defer stop()

	select {
	case at := <-drivenAt:
		if at.Sub(died) < deadTTL {
			t.Fatalf("the loop drove r %v after its holder died, before the holder's %v lease expired", at.Sub(died), deadTTL)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the loop never took over the dead holder's run")
	}
}

// One long drive does not hold up the rest: while run a is still being driven, the loop takes
// over run b once b's dead holder's lease expires.
func TestRecoverLoop_LongDriveDoesNotBlockOthers(t *testing.T) {
	synctest.Test(t, testRecoverLoopLongDriveDoesNotBlockOthers)
}

func testRecoverLoopLongDriveDoesNotBlockOthers(t *testing.T) {
	ctx := context.Background()
	s := &countingStore{MemStore: NewMemStore()}
	seedRun(t, s.MemStore, "a")
	seedRun(t, s.MemStore, "b")
	if ok, _ := s.AcquireLease(ctx, "b", "dead-worker#0", 100*time.Millisecond); !ok {
		t.Fatal("setup: the dead worker should hold b")
	}
	aStarted, bDriven := make(chan struct{}), make(chan struct{})
	var once sync.Once
	stop := runLoop(t, s, func(ctx context.Context, id string, _ RunStart) error {
		switch id {
		case "a":
			once.Do(func() { close(aStarted) })
			<-ctx.Done() // a long drive: runs until the loop stops
			return ctx.Err()
		default:
			close(bDriven)
			_, err := s.Do(ctx, id, runCompleteStep, func(context.Context) (Record, error) { return Record{Kind: StepValue}, nil })
			return err
		}
	}, WithRecoverInterval(20*time.Millisecond))
	defer stop()

	<-aStarted
	select {
	case <-bDriven:
	case <-time.After(3 * time.Second):
		t.Fatal("run b was never driven while run a's drive was still running")
	}
	time.Sleep(100 * time.Millisecond) // more passes while a is still in flight
	if n := s.acquires("a"); n != 1 {
		t.Fatalf("the loop tried to lease run a %d times while it was driving it, want once", n)
	}
}

// countingStore is a MemStore that counts lease acquisitions per run and listings: the full
// pass's (lists) and the lapsed loop's (lapsedLists) apart.
type countingStore struct {
	*MemStore
	mu          sync.Mutex
	acquired    map[string]int
	lists       int
	lapsedLists int
}

func (c *countingStore) AcquireLease(ctx context.Context, runID, holder string, ttl time.Duration) (bool, error) {
	c.mu.Lock()
	if c.acquired == nil {
		c.acquired = map[string]int{}
	}
	c.acquired[runID]++
	c.mu.Unlock()
	return c.MemStore.AcquireLease(ctx, runID, holder, ttl)
}

func (c *countingStore) Runs(ctx context.Context, f RunFilter) iter.Seq2[string, error] {
	c.mu.Lock()
	if f.LeaseLapsed {
		c.lapsedLists++
	} else {
		c.lists++
	}
	c.mu.Unlock()
	return c.MemStore.Runs(ctx, f)
}

func (c *countingStore) acquires(runID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.acquired[runID]
}

// Without WithRecoverInterval, a pass of each loop starts every half lease TTL.
func TestRecoverLoop_DefaultIntervalIsHalfTheTTL(t *testing.T) {
	synctest.Test(t, testRecoverLoopDefaultIntervalIsHalfTheTTL)
}

func testRecoverLoopDefaultIntervalIsHalfTheTTL(t *testing.T) {
	s := &countingStore{MemStore: NewMemStore()}
	stop := runLoop(t, s, func(context.Context, string, RunStart) error { return nil }, WithLeaseTTL(100*time.Millisecond))
	time.Sleep(525 * time.Millisecond)
	_ = stop()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lists != 11 || s.lapsedLists != 11 {
		t.Fatalf("%d full and %d lapsed passes in 525ms with a 100ms TTL, want 11 of each (one every 50ms, from 0ms to 500ms)", s.lists, s.lapsedLists)
	}
}

// When its context ends, the loop cancels the drives it started, waits for them to return, and
// returns the context's error.
func TestRecoverLoop_WaitsForItsDrivesOnShutdown(t *testing.T) {
	synctest.Test(t, testRecoverLoopWaitsForItsDrivesOnShutdown)
}

func testRecoverLoopWaitsForItsDrivesOnShutdown(t *testing.T) {
	s := NewMemStore()
	seedRun(t, s, "r")
	started := make(chan struct{})
	var mu sync.Mutex
	returned := false
	var reported []error
	stop := runLoop(t, s, func(ctx context.Context, _ string, _ RunStart) error {
		close(started)
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond) // wind down
		mu.Lock()
		returned = true
		mu.Unlock()
		return ctx.Err()
	}, WithRecoverInterval(time.Hour), WithRecoverErrors(func(err error) {
		mu.Lock()
		reported = append(reported, err)
		mu.Unlock()
	}))
	<-started
	if err := stop(); !errors.Is(err, context.Canceled) {
		t.Fatalf("RecoverLoop returned %v, want context.Canceled", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !returned {
		t.Fatal("RecoverLoop returned while a drive it started was still running")
	}
	if len(reported) != 0 {
		t.Fatalf("the shutdown's own cancellation was reported as a failure: %v", reported)
	}
}

// Genuine failures reach the error handler; pauses (a ResumeHalt), lost leases and runs held by
// another holder do not, and a run that completes is not driven again.
func TestRecoverLoop_ReportsOnlyGenuineFailures(t *testing.T) {
	synctest.Test(t, testRecoverLoopReportsOnlyGenuineFailures)
}

func testRecoverLoopReportsOnlyGenuineFailures(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	for _, id := range []string{"broken", "halted", "done", "held"} {
		seedRun(t, s, id)
	}
	if ok, _ := s.AcquireLease(ctx, "held", "other#0", time.Hour); !ok {
		t.Fatal("setup: other should hold held")
	}
	boom := errors.New("boom")
	var mu sync.Mutex
	var reported []error
	drives := map[string]int{}
	stop := runLoop(t, s, func(ctx context.Context, id string, _ RunStart) error {
		mu.Lock()
		drives[id]++
		mu.Unlock()
		switch id {
		case "broken":
			return boom
		case "halted":
			return &OutcomeUnknown{RunRef: RunRef{RunID: id}}
		case "done":
			_, err := s.Do(ctx, id, runCompleteStep, func(context.Context) (Record, error) { return Record{Kind: StepValue}, nil })
			return err
		}
		return errors.New("drove a run another holder leases")
	}, WithRecoverInterval(10*time.Millisecond), WithRecoverErrors(func(err error) {
		mu.Lock()
		reported = append(reported, err)
		mu.Unlock()
	}))
	time.Sleep(200 * time.Millisecond) // many passes
	if err := stop(); !errors.Is(err, context.Canceled) {
		t.Fatalf("RecoverLoop returned %v, want context.Canceled", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(reported) == 0 {
		t.Fatal("the failing run's error never reached the error handler")
	}
	for _, err := range reported {
		if !errors.Is(err, boom) {
			t.Fatalf("the error handler got %v, want only the failing run's error", err)
		}
	}
	if drives["broken"] < 2 || drives["halted"] < 2 {
		t.Fatalf("drives = %v: an incomplete run should be retried on every pass", drives)
	}
	if drives["done"] != 1 {
		t.Fatalf("the completed run was driven %d times, want once", drives["done"])
	}
	if drives["held"] != 0 {
		t.Fatalf("a run another holder leases was driven %d times", drives["held"])
	}
}

// Misconfiguration is reported at once instead of looping.
func TestRecoverLoop_RejectsBadConfig(t *testing.T) {
	synctest.Test(t, testRecoverLoopRejectsBadConfig)
}

func testRecoverLoopRejectsBadConfig(t *testing.T) {
	ctx := context.Background()
	resume := func(context.Context, string, RunStart) error { return nil }
	for name, tc := range map[string]struct {
		s    Durable
		opts []RecoverLoopOption
	}{
		"no Lister":               {noListStore{}, nil},
		"zero TTL":                {NewMemStore(), []RecoverLoopOption{WithLeaseTTL(0)}},
		"zero interval":           {NewMemStore(), []RecoverLoopOption{WithRecoverInterval(0)}},
		"zero concurrency":        {NewMemStore(), []RecoverLoopOption{WithRecoverConcurrency(0)}},
		"zero lapsed concurrency": {NewMemStore(), []RecoverLoopOption{WithRecoverLapsedConcurrency(0)}},
		"negative interval":       {NewMemStore(), []RecoverLoopOption{WithRecoverInterval(-time.Second)}},
	} {
		done := make(chan error, 1)
		go func() { done <- RecoverLoop(ctx, tc.s, resume, tc.opts...) }()
		select {
		case err := <-done:
			if !errors.Is(err, ErrConfig) {
				t.Errorf("%s: RecoverLoop = %v, want ErrConfig", name, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: RecoverLoop kept running instead of reporting its configuration error", name)
		}
	}
}

// Runs that stay incomplete on every pass (halted ones, say) do not starve the runs listed after
// them when every drive slot is taken: each pass reaches every run.
func TestRecoverLoop_EveryPassReachesEveryRun(t *testing.T) {
	synctest.Test(t, testRecoverLoopEveryPassReachesEveryRun)
}

func testRecoverLoopEveryPassReachesEveryRun(t *testing.T) {
	s := NewMemStore() // lists its runs in order, as the SQL stores do
	for _, id := range []string{"a1", "a2", "a3", "a4", "z"} {
		seedRun(t, s, id)
	}
	zDriven := make(chan struct{})
	var once sync.Once
	stop := runLoop(t, s, func(ctx context.Context, id string, _ RunStart) error {
		if id == "z" {
			once.Do(func() { close(zDriven) })
			return nil
		}
		time.Sleep(5 * time.Millisecond)
		return &OutcomeUnknown{RunRef: RunRef{RunID: id}} // stays incomplete: re-driven every pass
	}, WithRecoverInterval(10*time.Millisecond), WithRecoverConcurrency(1))
	defer stop()
	select {
	case <-zDriven:
	case <-time.After(3 * time.Second):
		t.Fatal("run z was never driven: the halted runs listed before it took the only slot on every pass")
	}
}
