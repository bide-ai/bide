package agent

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Every RecoverLoop test runs in a synctest bubble, so pass intervals, lease expiries and drive
// durations run on the bubble's clock and do not depend on how busy the machine is.

// runLoop runs RecoverLoop in the background and returns a function that stops it and reports
// what it returned.
func runLoop(t *testing.T, s *Journal, resume Resumer, opts ...RecoverLoopOption) (stop func() error) {
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
	j := mustJournal(s)
	seedRun(t, s, "r")
	const deadTTL = 150 * time.Millisecond
	died := time.Now()
	if ok, _ := s.AcquireLease(ctx, "r", "dead-worker#0", deadTTL); !ok {
		t.Fatal("setup: the dead worker should hold r")
	}
	drivenAt := make(chan time.Time, 1)
	stop := runLoop(t, j, func(ctx context.Context, id string, _ RunStart) error {
		select {
		case drivenAt <- time.Now():
		default:
		}
		_, err := j.do(ctx, id, runCompleteStep, func(context.Context) (Record, error) { return Record{Kind: StepValue}, nil })
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
	j := mustJournal(s)
	seedRun(t, s.MemStore, "a")
	seedRun(t, s.MemStore, "b")
	if ok, _ := s.AcquireLease(ctx, "b", "dead-worker#0", 100*time.Millisecond); !ok {
		t.Fatal("setup: the dead worker should hold b")
	}
	aStarted, bDriven := make(chan struct{}), make(chan struct{})
	var once sync.Once
	stop := runLoop(t, j, func(ctx context.Context, id string, _ RunStart) error {
		switch id {
		case "a":
			once.Do(func() { close(aStarted) })
			<-ctx.Done() // a long drive: runs until the loop stops
			return ctx.Err()
		default:
			close(bDriven)
			_, err := j.do(ctx, id, runCompleteStep, func(context.Context) (Record, error) { return Record{Kind: StepValue}, nil })
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
	j := mustJournal(s)
	stop := runLoop(t, j, func(context.Context, string, RunStart) error { return nil }, WithLeaseTTL(100*time.Millisecond))
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
	j := mustJournal(s)
	seedRun(t, s, "r")
	started := make(chan struct{})
	var mu sync.Mutex
	returned := false
	var reported []error
	stop := runLoop(t, j, func(ctx context.Context, _ string, _ RunStart) error {
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

// Genuine failures reach the error handler; pauses (a OutcomeUnknown), lost leases and runs held by
// another holder do not, and a run that completes is not driven again.
func TestRecoverLoop_ReportsOnlyGenuineFailures(t *testing.T) {
	synctest.Test(t, testRecoverLoopReportsOnlyGenuineFailures)
}

func testRecoverLoopReportsOnlyGenuineFailures(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	j := mustJournal(s)
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
	stop := runLoop(t, j, func(ctx context.Context, id string, _ RunStart) error {
		mu.Lock()
		drives[id]++
		mu.Unlock()
		switch id {
		case "broken":
			return boom
		case "halted":
			return &OutcomeUnknown{RunRef: RunRef{RunID: id}}
		case "done":
			_, err := j.do(ctx, id, runCompleteStep, func(context.Context) (Record, error) { return Record{Kind: StepValue}, nil })
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
		s    *Journal
		opts []RecoverLoopOption
	}{
		"no Lister":               {mustJournal(noListStore{}), nil},
		"zero TTL":                {memJournal(), []RecoverLoopOption{WithLeaseTTL(0)}},
		"zero interval":           {memJournal(), []RecoverLoopOption{WithRecoverInterval(0)}},
		"zero concurrency":        {memJournal(), []RecoverLoopOption{WithRecoverConcurrency(0)}},
		"zero lapsed concurrency": {memJournal(), []RecoverLoopOption{WithRecoverLapsedConcurrency(0)}},
		"negative interval":       {memJournal(), []RecoverLoopOption{WithRecoverInterval(-time.Second)}},
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
	s := NewMemStore()
	j := mustJournal(s) // lists its runs in order, as the SQL stores do
	for _, id := range []string{"a1", "a2", "a3", "a4", "z"} {
		seedRun(t, s, id)
	}
	zDriven := make(chan struct{})
	var once sync.Once
	stop := runLoop(t, j, func(ctx context.Context, id string, _ RunStart) error {
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

// No drive and no pass starts after the loop's context is cancelled. A select picks at random
// among ready cases, so a check of ctx before a select whose other case is ready (a free slot, a
// tick) does not stop the work that case starts if the cancellation lands between the check and
// the select. lateCancelStore lands it there on every iteration, so before the fix about half of
// them started a drive, and half of the rest started another pass.
//
// Unlike the other RecoverLoop tests, this one runs outside a synctest bubble: its 1ns tick must
// be ready when the pass returns, as it is on a loop that outlasts its interval, and in a bubble
// the clock stands still until every goroutine blocks.
func TestRecoverLoop_NothingStartsAfterCancel(t *testing.T) {
	for iter := range 200 {
		parent, cancel := context.WithCancel(context.Background())
		ctx := &lateCancelCtx{Context: parent, cancel: cancel}
		s := &lateCancelStore{m: NewMemStore(), ctx: ctx}
		seedRun(t, s.m, "r")
		var resumes atomic.Int32
		err := RecoverLoop(ctx, mustJournal(s), func(context.Context, string, RunStart) error {
			resumes.Add(1)
			return nil
		}, WithRecoverInterval(time.Nanosecond))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("iteration %d: RecoverLoop returned %v, want context.Canceled", iter, err)
		}
		if n := resumes.Load(); n != 0 {
			t.Fatalf("iteration %d: %d drives started after the cancellation, want none", iter, n)
		}
		if n := s.lateLists.Load(); n != 0 {
			t.Fatalf("iteration %d: %d passes started after the cancellation, want none", iter, n)
		}
		cancel()
	}
}

// lateCancelCtx is a context whose first Err call after arm is set reports nil and cancels it, so
// the cancellation lands right after the caller's check.
type lateCancelCtx struct {
	context.Context
	cancel context.CancelFunc
	arm    atomic.Bool
}

func (c *lateCancelCtx) Err() error {
	if c.arm.CompareAndSwap(true, false) {
		c.cancel()
		return nil
	}
	return c.Context.Err()
}

// lateCancelStore is a Store that implements Lister and not Leaser. Its first listing yields run r
// with ctx armed, so the pass's check of ctx before it waits for a slot cancels ctx; it counts the
// listings started after that. It reads without ctx, as a store that does not check ctx would (the
// MemStore fails a read on a done ctx), so a drive started after the cancellation reaches resume.
type lateCancelStore struct {
	m         *MemStore
	ctx       *lateCancelCtx
	listed    atomic.Bool
	lateLists atomic.Int32
}

func (l *lateCancelStore) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	return l.m.Insert(ctx, runID, name, data)
}

func (l *lateCancelStore) Get(_ context.Context, runID, name string) (Entry, bool, error) {
	return l.m.Get(context.Background(), runID, name)
}

func (l *lateCancelStore) Load(_ context.Context, runID string, after int64) iter.Seq2[Entry, error] {
	return l.m.Load(context.Background(), runID, after)
}

func (l *lateCancelStore) Runs(ctx context.Context, f RunFilter) iter.Seq2[string, error] {
	if l.listed.Swap(true) {
		l.lateLists.Add(1) // only the first listing comes before the cancellation
		return func(func(string, error) bool) {}
	}
	return func(yield func(string, error) bool) {
		l.ctx.arm.Store(true)
		yield("r", nil)
	}
}

// reanchoringStore is a MemStore that records the runs it was asked to Reanchor, as an
// audit.AuditedStore over it would be asked.
type reanchoringStore struct {
	*MemStore
	mu   sync.Mutex
	runs map[string]int
	err  error
}

func (r *reanchoringStore) Reanchor(_ context.Context, runID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runs == nil {
		r.runs = map[string]int{}
	}
	r.runs[runID]++
	return r.err
}

func (r *reanchoringStore) count(runID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runs[runID]
}

// wrapperOf is a store wrapper that passes everything through, so Reanchor is found only through
// Unwrap.
type wrapperOf struct{ Store }

func (w wrapperOf) Unwrap() Store { return w.Store }

func pausedAt(id string) error {
	return &ApprovalPending{RunRef: RunRef{RunID: id, RootRunID: id}, ToolUseID: "c1", ToolName: "pay"}
}

// A pass that finds a run waiting on a decision (a quorum gate with one of two decisions in, say)
// re-signs it: a decision another process recorded is under a signed head within one pass, not at
// the app's next write to the run.
func TestRecoverLoop_ReanchorsRunsWaitingOnAPerson(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &reanchoringStore{MemStore: NewMemStore()}
		j := mustJournal(wrapperOf{s}) // found through Unwrap
		for _, id := range []string{"gate", "halt", "timer", "done", "interrupt"} {
			seedRun(t, s.MemStore, id)
		}
		stop := runLoop(t, j, func(ctx context.Context, id string, _ RunStart) error {
			switch id {
			case "gate":
				return pausedAt(id)
			case "halt":
				return &OutcomeUnknown{RunRef: RunRef{RunID: id, RootRunID: id}, Op: OpRef{Kind: OpTool, ID: "t1"}}
			case "timer":
				return &TimerPending{RunRef: RunRef{RunID: id, RootRunID: id}, Name: "w", FireAt: time.Now().Add(time.Hour)}
			case "interrupt":
				return &InterruptPending{RunRef: RunRef{RunID: id, RootRunID: id}, Name: "q"}
			}
			return nil
		}, WithRecoverInterval(20*time.Millisecond))
		time.Sleep(100 * time.Millisecond)
		if err := stop(); !errors.Is(err, context.Canceled) {
			t.Fatalf("RecoverLoop returned %v", err)
		}
		for _, id := range []string{"gate", "halt"} {
			if s.count(id) == 0 {
				t.Errorf("run %s waits on a person and was not re-signed", id)
			}
		}
		for _, id := range []string{"timer", "done", "interrupt"} {
			if n := s.count(id); n != 0 {
				t.Errorf("run %s does not wait on a decision or a halt resolution and was re-signed %d times", id, n)
			}
		}
	})
}

// A run another holder leases is that holder's to sign: the pass does not drive it and does not
// re-sign it.
func TestRecoverLoop_DoesNotReanchorALeasedRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &reanchoringStore{MemStore: NewMemStore()}
		j := mustJournal(s)
		seedRun(t, s.MemStore, "r")
		if ok, _ := s.AcquireLease(context.Background(), "r", "other#0", time.Hour); !ok {
			t.Fatal("setup: lease")
		}
		stop := runLoop(t, j, func(context.Context, string, RunStart) error { return pausedAt("r") },
			WithRecoverInterval(20*time.Millisecond))
		time.Sleep(100 * time.Millisecond)
		stop()
		if n := s.count("r"); n != 0 {
			t.Fatalf("re-signed a run it did not drive %d times", n)
		}
	})
}

// A failed re-sign goes to the error handler, wrapping ErrStorage, and does not stop the loop: the
// next pass tries again.
func TestRecoverLoop_ReanchorFailureIsReported(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		boom := errors.New("anchor down")
		s := &reanchoringStore{MemStore: NewMemStore(), err: boom}
		j := mustJournal(s)
		seedRun(t, s.MemStore, "r")
		var mu sync.Mutex
		var got []error
		stop := runLoop(t, j, func(context.Context, string, RunStart) error { return pausedAt("r") },
			WithRecoverInterval(20*time.Millisecond),
			WithRecoverErrors(func(err error) { mu.Lock(); got = append(got, err); mu.Unlock() }))
		time.Sleep(100 * time.Millisecond)
		stop()
		mu.Lock()
		defer mu.Unlock()
		if len(got) < 2 {
			t.Fatalf("got %d reports, want one per pass", len(got))
		}
		if !errors.Is(got[0], boom) || !errors.Is(got[0], ErrStorage) {
			t.Fatalf("report %v does not wrap the anchor's error and ErrStorage", got[0])
		}
	})
}

// WithReanchor replaces the store's own: an application whose signer is not an AuditedStore in the
// journal's chain names the call, and a nil function turns re-signing off.
func TestRecoverLoop_WithReanchor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &reanchoringStore{MemStore: NewMemStore()}
		j := mustJournal(s)
		seedRun(t, s.MemStore, "r")
		var mu sync.Mutex
		var own int
		stop := runLoop(t, j, func(context.Context, string, RunStart) error { return pausedAt("r") },
			WithRecoverInterval(20*time.Millisecond),
			WithReanchor(func(context.Context, string) error { mu.Lock(); own++; mu.Unlock(); return nil }))
		time.Sleep(100 * time.Millisecond)
		stop()
		mu.Lock()
		n := own
		mu.Unlock()
		if n == 0 || s.count("r") != 0 {
			t.Fatalf("own function called %d times, the store's %d; want the function only", n, s.count("r"))
		}

		s2 := &reanchoringStore{MemStore: NewMemStore()}
		seedRun(t, s2.MemStore, "r")
		stop = runLoop(t, mustJournal(s2), func(context.Context, string, RunStart) error { return pausedAt("r") },
			WithRecoverInterval(20*time.Millisecond), WithReanchor(nil))
		time.Sleep(100 * time.Millisecond)
		stop()
		if s2.count("r") != 0 {
			t.Fatal("WithReanchor(nil) did not turn re-signing off")
		}
	})
}

// A re-sign that hangs is cut off at its deadline: it does not hold the run's lease and the loop's
// only slot for as long as the anchor blocks, and the other waiting run is re-signed.
func TestRecoverLoop_HungReanchorDoesNotStarveOtherRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := NewMemStore()
		for _, id := range []string{"a", "b"} {
			seedRun(t, s, id)
		}
		var mu sync.Mutex
		signed := map[string]int{}
		var reports []error
		stop := runLoop(t, mustJournal(s), func(_ context.Context, id string, _ RunStart) error { return pausedAt(id) },
			WithRecoverInterval(20*time.Millisecond), WithRecoverConcurrency(1), WithReanchorTimeout(time.Second),
			WithRecoverErrors(func(err error) { mu.Lock(); reports = append(reports, err); mu.Unlock() }),
			WithReanchor(func(ctx context.Context, id string) error {
				if id == "a" {
					<-ctx.Done() // the anchor hangs
					return ctx.Err()
				}
				mu.Lock()
				signed[id]++
				mu.Unlock()
				return nil
			}))
		time.Sleep(10 * time.Second)
		stop()
		mu.Lock()
		defer mu.Unlock()
		if signed["b"] == 0 {
			t.Fatal("run b was never re-signed: run a's hung re-sign holds the only slot")
		}
		if len(reports) == 0 || !errors.Is(reports[0], context.DeadlineExceeded) || !errors.Is(reports[0], ErrStorage) {
			t.Fatalf("the hung re-sign was reported as %v, want the deadline wrapping ErrStorage", reports)
		}
	})
}

func TestWithReanchorTimeout_MustBePositive(t *testing.T) {
	s := NewMemStore()
	err := RecoverLoop(context.Background(), mustJournal(s), func(context.Context, string, RunStart) error { return nil }, WithReanchorTimeout(0))
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("a zero timeout: %v, want ErrConfig", err)
	}
}

// Parallel sub-agents that wait together return their pauses joined: every one is re-signed, with
// the root, and a pause wrapped in another error is found too.
func TestWaitingRuns_WalksJoinedErrors(t *testing.T) {
	err := errors.Join(
		&ApprovalPending{RunRef: RunRef{RunID: "root>sub1", RootRunID: "root"}, ToolUseID: "c1"},
		fmt.Errorf("sub2: %w", &OutcomeUnknown{RunRef: RunRef{RunID: "root>sub2", RootRunID: "root"}}),
		&TimerPending{RunRef: RunRef{RunID: "root>sub3", RootRunID: "root"}},
	)
	got := waitingRuns(err)
	slices.Sort(got)
	if want := []string{"root", "root>sub1", "root>sub2"}; !slices.Equal(got, want) {
		t.Fatalf("waitingRuns = %v, want %v", got, want)
	}
	if waitingRuns(nil) != nil || waitingRuns(errors.New("x")) != nil {
		t.Fatal("a non-pause is a waiting run")
	}
}

// Recover, the single pass, re-signs a waiting run as RecoverLoop does, and WithReanchor(nil)
// turns it off for it too.
func TestRecover_Reanchors(t *testing.T) {
	resume := func(context.Context, string, RunStart) error { return pausedAt("r") }
	s := &reanchoringStore{MemStore: NewMemStore()}
	seedRun(t, s.MemStore, "r")
	if _, err := Recover(context.Background(), mustJournal(s), resume); err != nil || s.count("r") != 1 {
		t.Fatalf("Recover: %v, re-signed %d times; want once", err, s.count("r"))
	}
	if _, err := Recover(context.Background(), mustJournal(s), resume, WithReanchor(nil)); err != nil || s.count("r") != 1 {
		t.Fatalf("Recover with WithReanchor(nil): %v, re-signed %d times in all; want still once", err, s.count("r"))
	}
}

// Nested and wrapped joins are all walked, and a chain that loops through Unwrap ends.
func TestWaitingRuns_NestedJoinsAndCycles(t *testing.T) {
	p := func(id string) error {
		return &ApprovalPending{RunRef: RunRef{RunID: id, RootRunID: "root"}, ToolUseID: "c"}
	}
	err := fmt.Errorf("drive: %w", errors.Join(p("a"), fmt.Errorf("x: %w, y: %w", errors.Join(p("b"), errors.New("e")), p("c"))))
	got := waitingRuns(err)
	for _, id := range []string{"a", "b", "c", "root"} {
		if !slices.Contains(got, id) {
			t.Errorf("waitingRuns = %v, misses %s", got, id)
		}
	}
	loop := &cycleErr{}
	loop.next = loop
	if got := waitingRuns(loop); got != nil {
		t.Fatalf("a cycle gave %v", got)
	}
	deep := error(p("deep"))
	for range maxWaitingDepth + 5 {
		deep = fmt.Errorf("w: %w", deep)
	}
	if got := waitingRuns(deep); got != nil {
		t.Fatalf("a pause deeper than the cap was found: %v", got)
	}
}

type cycleErr struct{ next error }

func (e *cycleErr) Error() string { return "cycle" }
func (e *cycleErr) Unwrap() error { return e.next }

// A recovery pass lists the runs that are not over, then leases and drives each in turn. Another
// driver can finish a listed run while the pass waits (for a drive slot, or for the drive before
// it); the pass must then see, under the lease, that the run is over and leave it alone, rather
// than call resume for a finished run.

// completeUnderLease drives runID to completion as another holder would: under its own lease,
// through Agent.Run, which writes run:complete and then releases the lease.
func completeUnderLease(t *testing.T, s *MemStore, runID string) {
	t.Helper()
	driven, err := Lease(context.Background(), mustJournal(s), runID, func(ctx context.Context) error {
		_, err := mustNew(NewScriptedModel(TextTurn("done")), mustJournal(s)).Run(ctx, runID, UserText("go"))
		return err
	}, WithLeaseHolder("other"))
	if err != nil || !driven {
		t.Fatalf("other holder's drive of %s: driven=%v err=%v", runID, driven, err)
	}
	if done, err := IsComplete(context.Background(), mustJournal(s), runID); err != nil || !done {
		t.Fatalf("run %s not complete after the other holder's drive: %v %v", runID, done, err)
	}
}

// endUnderLease ends runID as another holder would: under its own lease, it records the terminal
// marker name and then releases the lease.
func endUnderLease(t *testing.T, s *MemStore, runID, name string) {
	t.Helper()
	driven, err := Lease(context.Background(), mustJournal(s), runID, func(ctx context.Context) error {
		_, err := mustJournal(s).put(ctx, runID, name, Record{Kind: StepValue})
		return err
	}, WithLeaseHolder("other"))
	if err != nil || !driven {
		t.Fatalf("other holder's %s of %s: driven=%v err=%v", name, runID, driven, err)
	}
}

func TestRecoverLoop_DoesNotResumeARunCompletedWhileItWaited(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := NewMemStore()
		j := mustJournal(s)
		for _, id := range []string{"a", "b"} {
			seedRun(t, s, id)
		}
		first, release := make(chan string, 1), make(chan struct{})
		var mu sync.Mutex
		var resumedComplete []string
		stop := runLoop(t, j, func(ctx context.Context, id string, _ RunStart) error {
			if done, _ := IsComplete(ctx, j, id); done {
				mu.Lock()
				resumedComplete = append(resumedComplete, id)
				mu.Unlock()
				return nil
			}
			select {
			case first <- id: // the first run driven holds the only slot until released
				<-release
			default:
			}
			return nil
		}, WithLeaseHolder("w"), WithRecoverConcurrency(1))
		held := <-first
		synctest.Wait() // the pass has listed the other run as unfinished and waits for the slot
		other := "a"
		if held == "a" {
			other = "b"
		}
		completeUnderLease(t, s, other)
		close(release)
		synctest.Wait() // the pass has finished; the next waits on the (fake) ticker
		if err := stop(); err != nil && err != context.Canceled {
			t.Fatal(err)
		}
		if len(resumedComplete) > 0 {
			t.Errorf("RecoverLoop called resume for %v after it completed", resumedComplete)
		}
	})
}

func TestRecover_DoesNotResumeARunCompletedWhileItWaited(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := NewMemStore()
		j := mustJournal(s)
		for _, id := range []string{"a", "b"} {
			seedRun(t, s, id)
		}
		var resumedComplete []string
		n, err := Recover(context.Background(), j, func(ctx context.Context, id string, _ RunStart) error {
			if done, _ := IsComplete(ctx, j, id); done {
				resumedComplete = append(resumedComplete, id)
				// The documented resume: Run replays a finished run and returns its answer.
				before, _ := j.History(ctx, id)
				_, err := mustNew(NewScriptedModel(), j).Run(ctx, id, UserText("go"))
				after, _ := j.History(ctx, id)
				if len(after) != len(before) {
					t.Errorf("replaying completed run %s wrote %d records", id, len(after)-len(before))
				}
				return err
			}
			if id == "a" {
				completeUnderLease(t, s, "b") // b completes while this pass drives a
			}
			return nil
		}, WithLeaseHolder("w"))
		if err != nil {
			t.Fatal(err)
		}
		if len(resumedComplete) > 0 {
			t.Errorf("Recover called resume for %v after it completed", resumedComplete)
		}
		if n != 1 {
			t.Errorf("Recover = %d, want 1: b was over before the pass drove it", n)
		}
	})
}

// Every marker that ends a run, not only run:complete, is checked under the lease.
func TestRecover_DoesNotResumeARunEndedWhileItWaited(t *testing.T) {
	for _, name := range []string{runCompleteStep, runAbortedStep, runCancelledStep} {
		for path, wrap := range map[string]func(*MemStore) *Journal{
			"journal": func(s *MemStore) *Journal { return mustJournal(s) },
		} {
			t.Run(name+"/"+path, func(t *testing.T) {
				s := NewMemStore()
				for _, id := range []string{"a", "b"} {
					seedRun(t, s, id)
				}
				var resumed []string
				n, err := Recover(context.Background(), wrap(s), func(ctx context.Context, id string, _ RunStart) error {
					resumed = append(resumed, id)
					if id == "a" {
						endUnderLease(t, s, "b", name) // b ends while this pass drives a
					}
					return nil
				}, WithLeaseHolder("w"))
				if err != nil {
					t.Fatal(err)
				}
				if len(resumed) != 1 || resumed[0] != "a" || n != 1 {
					t.Errorf("Recover resumed %v (reported %d), want only a: b held %s before the pass leased it", resumed, n, name)
				}
			})
		}
	}
}

// The markers the check reads are the ones the engine writes to end a run, and the ones the
// listing excludes.
func TestRecoverFilter_HoldsEveryEndOfRunMarker(t *testing.T) {
	want := []string{runCompleteStep, runAbortedStep, runCancelledStep}
	if len(recoverFilter.ExcludeHolding) != len(want) {
		t.Fatalf("recoverFilter.ExcludeHolding = %v, want %v", recoverFilter.ExcludeHolding, want)
	}
	for i, n := range want {
		if recoverFilter.ExcludeHolding[i] != n {
			t.Fatalf("recoverFilter.ExcludeHolding = %v, want %v", recoverFilter.ExcludeHolding, want)
		}
	}
}

// failGet fails every Get of the entry named name.
type failGet struct {
	unwrapStore
	name string
}

func (f failGet) Get(ctx context.Context, runID, name string) (Entry, bool, error) {
	if name == f.name {
		return Entry{}, false, errors.New("read failed")
	}
	return f.unwrapStore.Get(ctx, runID, name)
}

// A run whose markers the check cannot read is not driven: the failure is a storage error, and the
// next pass retries the run.
func TestRecover_DoesNotResumeARunItCannotCheck(t *testing.T) {
	for _, name := range []string{runCompleteStep, runAbortedStep, runCancelledStep} {
		t.Run(name, func(t *testing.T) {
			s := NewMemStore()
			seedRun(t, s, "a")
			j, err := NewJournal(failGet{unwrapStore{s}, name})
			if err != nil {
				t.Fatal(err)
			}
			resumed := 0
			n, err := Recover(context.Background(), j, func(context.Context, string, RunStart) error { resumed++; return nil }, WithLeaseHolder("w"))
			if !errors.Is(err, ErrStorage) || resumed != 0 {
				t.Errorf("Recover = %d, %v, resumed %d times; want an ErrStorage error and no resume", n, err, resumed)
			}
			if n != 0 {
				t.Errorf("Recover = %d re-driven, want 0: resume was never called", n)
			}
		})
	}
}

// A run in a journal format this version cannot read is refused with *JournalVersionError, not
// reported as a storage failure.
func TestRecover_ForeignFormatIsNotAStorageError(t *testing.T) {
	for path, wrap := range map[string]func(*MemStore) *Journal{
		"journal": func(s *MemStore) *Journal { return mustJournal(s) },
	} {
		t.Run(path, func(t *testing.T) {
			s := NewMemStore()
			hdr := []byte(`{"name":"@journal","kind":"header","format":"bide.journal.v999","salt":"AAAAAAAAAAAAAAAAAAAAAA=="}`)
			if _, _, err := s.Insert(context.Background(), "a", headerStep, hdr); err != nil {
				t.Fatal(err)
			}
			d := wrap(s)
			_, err := Recover(context.Background(), d, func(ctx context.Context, id string, _ RunStart) error {
				_, err := mustNew(NewScriptedModel(TextTurn("x")), d).Run(ctx, id, UserText("go"))
				return err
			}, WithLeaseHolder("w"))
			var jv *JournalVersionError
			if !errors.As(err, &jv) || errors.Is(err, ErrStorage) {
				t.Errorf("Recover = %v; want a *JournalVersionError that is not ErrStorage", err)
			}
		})
	}
}
