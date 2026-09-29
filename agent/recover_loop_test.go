package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// runLoop runs RecoverLoop in the background and returns a function that stops it and reports
// what it returned.
func runLoop(t *testing.T, s Durable, resume func(context.Context, string) error, opts ...RecoverOption) (stop func() error) {
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
	ctx := context.Background()
	s := NewMemStore()
	seedRun(t, s, "r")
	const deadTTL = 150 * time.Millisecond
	died := time.Now()
	if ok, _ := s.AcquireLease(ctx, "r", "dead-worker#0", deadTTL); !ok {
		t.Fatal("setup: the dead worker should hold r")
	}
	drivenAt := make(chan time.Time, 1)
	stop := runLoop(t, s, func(ctx context.Context, id string) error {
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
	ctx := context.Background()
	s := NewMemStore()
	seedRun(t, s, "a")
	seedRun(t, s, "b")
	if ok, _ := s.AcquireLease(ctx, "b", "dead-worker#0", 100*time.Millisecond); !ok {
		t.Fatal("setup: the dead worker should hold b")
	}
	aStarted, bDriven := make(chan struct{}), make(chan struct{})
	var once sync.Once
	stop := runLoop(t, s, func(ctx context.Context, id string) error {
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
}

// When its context ends, the loop cancels the drives it started, waits for them to return, and
// returns the context's error.
func TestRecoverLoop_WaitsForItsDrivesOnShutdown(t *testing.T) {
	s := NewMemStore()
	seedRun(t, s, "r")
	started := make(chan struct{})
	var mu sync.Mutex
	returned := false
	stop := runLoop(t, s, func(ctx context.Context, _ string) error {
		close(started)
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond) // wind down
		mu.Lock()
		returned = true
		mu.Unlock()
		return ctx.Err()
	}, WithRecoverInterval(time.Hour))
	<-started
	if err := stop(); !errors.Is(err, context.Canceled) {
		t.Fatalf("RecoverLoop returned %v, want context.Canceled", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !returned {
		t.Fatal("RecoverLoop returned while a drive it started was still running")
	}
}

// Genuine failures reach the error handler; pauses (a ResumeHalt), lost leases and runs held by
// another holder do not, and a run that completes is not driven again.
func TestRecoverLoop_ReportsOnlyGenuineFailures(t *testing.T) {
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
	stop := runLoop(t, s, func(ctx context.Context, id string) error {
		mu.Lock()
		drives[id]++
		mu.Unlock()
		switch id {
		case "broken":
			return boom
		case "halted":
			return &ResumeHalt{RunID: id}
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
	ctx := context.Background()
	resume := func(context.Context, string) error { return nil }
	for name, tc := range map[string]struct {
		s    Durable
		opts []RecoverOption
	}{
		"no Lister":         {noListStore{}, nil},
		"zero TTL":          {NewMemStore(), []RecoverOption{WithLeaseTTL(0)}},
		"zero interval":     {NewMemStore(), []RecoverOption{WithRecoverInterval(0)}},
		"zero concurrency":  {NewMemStore(), []RecoverOption{WithRecoverConcurrency(0)}},
		"negative interval": {NewMemStore(), []RecoverOption{WithRecoverInterval(-time.Second)}},
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
