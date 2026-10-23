package relay

import (
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

var errBusy = errors.New("busy")

func isBusy(err error) bool { return errors.Is(err, errBusy) }

// busyQuick gives errBusy the quick retries a Windows sharing violation gets.
func busyQuick(err error) int {
	if isBusy(err) {
		return transientRetries
	}
	return 0
}

// A rename refused for a reason that passes is retried with growing pauses and succeeds when it can; a final
// error is returned at once; a refusal that does not pass stops at the budget with the last error.
func TestRenameWithRetry(t *testing.T) {
	var slept []time.Duration
	sleep := func(d time.Duration) { slept = append(slept, d) }
	total := func() (n time.Duration) {
		for _, d := range slept {
			n += d
		}
		return n
	}

	calls := 0
	err := renameWithRetry(func(string, string) error {
		if calls++; calls <= 3 {
			return errBusy
		}
		return nil
	}, isBusy, sleep, "a", "b")
	if err != nil || calls != 4 || len(slept) != 3 || slept[0] != 5*time.Millisecond || slept[1] != 10*time.Millisecond || slept[2] != 20*time.Millisecond {
		t.Fatalf("transient: err %v, %d calls, pauses %v", err, calls, slept)
	}

	slept, calls = nil, 0
	final := errors.New("disk full")
	if err := renameWithRetry(func(string, string) error { calls++; return final }, isBusy, sleep, "a", "b"); err != final || calls != 1 || len(slept) != 0 {
		t.Fatalf("final error: %v, %d calls, pauses %v", err, calls, slept)
	}

	slept, calls = nil, 0
	err = renameWithRetry(func(string, string) error { calls++; return errBusy }, isBusy, sleep, "a", "b")
	if !errors.Is(err, errBusy) || total() != renameRetryBudget {
		t.Fatalf("never succeeds: %v after pauses totalling %v (budget %v)", err, total(), renameRetryBudget)
	}
	if calls != len(slept)+1 {
		t.Fatalf("%d calls for %d pauses", calls, len(slept))
	}
}

// A save whose rename is refused briefly succeeds, the state file is the new state, and no temporary file is left.
func TestSaveRetriesARefusedRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.state")
	d, c := newDedupe(10), newIDSet(10)
	d.restore(agent.NotifyID("e1"), []string{"s"})
	var fails atomic.Int32
	fails.Store(2)
	var pauses int
	s := &stateStore{path: path, syncDir: func(string) error { return nil }, quick: busyQuick,
		sleep: func(time.Duration) { pauses++ },
		renameFile: func(from, to string) error {
			if fails.Add(-1) >= 0 {
				return errBusy
			}
			return os.Rename(from, to)
		}}
	if err := s.save(d, c, true); err != nil {
		t.Fatalf("a rename refused twice failed the save: %v", err)
	}
	if pauses != 2 {
		t.Fatalf("%d pauses, want 2", pauses)
	}
	n, err := readSeen(t, path)
	if err != nil || n != 1 {
		t.Fatalf("state file: %d entries, %v", n, err)
	}
	if tmp, _ := filepath.Glob(path + ".tmp-*"); len(tmp) != 0 {
		t.Fatalf("temporary files left: %v", tmp)
	}
}

// A save that still fails after the retries returns the error, removes its temporary file and leaves the old state.
func TestSaveGivesUpOnAPersistentRefusal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.state")
	d, c := newDedupe(10), newIDSet(10)
	s := &stateStore{path: path, syncDir: func(string) error { return nil }, quick: busyQuick, sleep: func(time.Duration) {},
		renameFile: func(string, string) error { return errBusy }}
	err := s.save(d, c, true)
	if err == nil || !strings.Contains(err.Error(), "writing the state file") {
		t.Fatalf("err %v", err)
	}
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Fatal("a failed save created the state file")
	}
	if tmp, _ := filepath.Glob(path + ".tmp-*"); len(tmp) != 0 {
		t.Fatalf("temporary files left: %v", tmp)
	}
}

// A save that fails is not forgotten: the state stays dirty and is written when the file can be written again,
// with no further event, and a Close writes it too.
func TestFailedSaveIsRetriedWithoutAnotherEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.state")
	var logs []string
	var lmu sync.Mutex
	sink := &funcSink{name: "s"}
	h, err := New(Config{Secrets: []string{secret}, Sinks: []Sink{sink}, StatePath: path, StateFlushDelay: 20 * time.Millisecond,
		Logf: func(f string, a ...any) {
			if strings.Contains(f, "memory of forwarded and cleared events") { // the failed-save line
				lmu.Lock()
				logs = append(logs, f)
				lmu.Unlock()
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	var fail atomic.Bool
	fail.Store(true)
	var tries atomic.Int32
	h.state.renameFile = func(from, to string) error {
		tries.Add(1)
		if fail.Load() {
			return errors.New("refused")
		}
		return os.Rename(from, to)
	}
	h.retryDelay = 30 * time.Millisecond
	srv := httptest.NewServer(h)
	t.Cleanup(func() { srv.Close(); h.Close() })
	rg := &rig{srv: srv}
	sendEvents(t, rg, 3, "e")
	failures := func() int {
		lmu.Lock()
		defer lmu.Unlock()
		return len(logs)
	}
	waitFor2(t, "the failed save to be logged", func() bool { return failures() > 0 })
	if n := seenInFile(t, path); n != 0 {
		t.Fatalf("file holds %d entries while every rename is refused", n)
	}
	// The retry runs again while the file stays unwritable: the state is still wanted.
	first := tries.Load()
	waitFor2(t, "a second attempt", func() bool { return tries.Load() > first })
	fail.Store(false)
	waitForSeen(t, path, 3)
	h.Close()
}

// fakeTimers stands in for the Handler's timer: after records each wait and holds the callback, and fire runs the
// newest pending one, as the clock reaching it would. Nothing here depends on elapsed time, so a coarse timer
// or a slow runner cannot change the outcome.
type fakeTimers struct {
	mu    sync.Mutex
	waits []time.Duration
	live  []*fakeTimer
}

type fakeTimer struct {
	f       func()
	stopped bool
	fired   bool
	t       *fakeTimers
}

func (f *fakeTimer) Stop() bool {
	f.t.mu.Lock()
	defer f.t.mu.Unlock()
	was := !f.stopped && !f.fired
	f.stopped = true
	return was
}

func (ft *fakeTimers) after(d time.Duration, f func()) stopper {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	ft.waits = append(ft.waits, d)
	x := &fakeTimer{f: f, t: ft}
	ft.live = append(ft.live, x)
	return x
}

// fire runs the pending timer's callback and reports whether there was one.
func (ft *fakeTimers) fire() bool {
	ft.mu.Lock()
	var run *fakeTimer
	for _, x := range ft.live {
		if !x.stopped && !x.fired {
			run = x
		}
	}
	if run != nil {
		run.fired = true
	}
	ft.mu.Unlock()
	if run == nil {
		return false
	}
	run.f()
	return true
}

func (ft *fakeTimers) recorded() []time.Duration {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return append([]time.Duration(nil), ft.waits...)
}

// A save that is refused for a passing reason does not hold up the event path: persist makes one attempt and
// returns without sleeping, the retries are scheduled on a timer at 5 ms doubling, and the state is written when
// the refusal passes, with no log line for the passing refusals. The timer is a fake: the test asserts on the
// recorded waits and the sleep hook, not on elapsed time.
func TestTransientRefusalIsRetriedOnATimerNotInLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.state")
	var logged atomic.Int32
	sink := &funcSink{name: "s"}
	h, err := New(Config{Secrets: []string{secret}, Sinks: []Sink{sink}, StatePath: path, StateFlushDelay: -1,
		Logf: func(f string, a ...any) {
			if strings.Contains(f, "memory of forwarded and cleared events") {
				logged.Add(1)
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	ft := &fakeTimers{}
	h.afterFunc = ft.after
	var fails atomic.Int32
	fails.Store(3)
	var slept, attempts atomic.Int32
	h.state.quick = busyQuick
	h.state.sleep = func(time.Duration) { slept.Add(1) }
	h.state.renameFile = func(from, to string) error {
		attempts.Add(1)
		if fails.Add(-1) >= 0 {
			return errBusy
		}
		return os.Rename(from, to)
	}
	h.seen.restore(agent.NotifyID("e1"), []string{"s"})
	h.persist(true)
	if n, w := attempts.Load(), ft.recorded(); slept.Load() != 0 || n != 1 || len(w) != 1 {
		t.Fatalf("persist made %d attempts, slept %d times and scheduled %v: want one attempt, no sleep, one timer (the retry is in line)", n, slept.Load(), w)
	}
	for ft.fire() {
	}
	want := []time.Duration{5 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond}
	if got := ft.recorded(); !slices.Equal(got, want) {
		t.Fatalf("retry waits %v, want %v", got, want)
	}
	if slept.Load() != 0 {
		t.Fatalf("the retries slept %d times: they run in line", slept.Load())
	}
	waitForSeen(t, path, 1)
	if logged.Load() != 0 {
		t.Fatalf("%d log lines for refusals that passed", logged.Load())
	}
}

// A save that keeps failing for a reason that does not pass is retried with a growing wait, logged once for the
// streak, and its recovery is logged, so a permanent fault is neither a busy loop nor a flood. The timer is a
// fake, so the test asserts on the recorded waits (doubling from the retry delay), not on elapsed time.
func TestPermanentFailureBacksOffAndLogsOncePerStreak(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.state")
	var failedLines, recoveredLines atomic.Int32
	sink := &funcSink{name: "s"}
	h, err := New(Config{Secrets: []string{secret}, Sinks: []Sink{sink}, StatePath: path, StateFlushDelay: -1,
		Logf: func(f string, a ...any) {
			switch {
			case strings.Contains(f, "memory of forwarded and cleared events"):
				failedLines.Add(1)
			case strings.Contains(f, "written again"):
				recoveredLines.Add(1)
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	ft := &fakeTimers{}
	h.afterFunc = ft.after
	h.retryDelay = 10 * time.Millisecond
	var fail atomic.Bool
	fail.Store(true)
	h.state.renameFile = func(from, to string) error {
		if fail.Load() {
			return errors.New("refused")
		}
		return os.Rename(from, to)
	}
	h.seen.restore(agent.NotifyID("e1"), []string{"s"})
	h.persist(true)
	for range 5 {
		if !ft.fire() {
			t.Fatal("no retry was scheduled while the save kept failing")
		}
	}
	want := []time.Duration{10, 20, 40, 80, 160, 320}
	for i := range want {
		want[i] *= time.Millisecond
	}
	if got := ft.recorded(); !slices.Equal(got, want) {
		t.Fatalf("retry waits %v, want a doubling wait %v", got, want)
	}
	if n := failedLines.Load(); n != 1 {
		t.Fatalf("%d failure lines for one streak, want 1", n)
	}
	fail.Store(false)
	if !ft.fire() {
		t.Fatal("no retry was scheduled")
	}
	waitForSeen(t, path, 1)
	if n := recoveredLines.Load(); n != 1 {
		t.Fatalf("%d recovery lines, want 1", n)
	}
}

// While a save is refused for a passing reason, nothing sleeps with the state mutex held: a save makes one attempt
// and the retries run on a timer. This asserts the structure, not a duration: the sleep hook (which the in-line
// retry of startup and Close uses, and a save under h.pmu must not) is never called, and the event path (persist)
// returns from both an urgent and an ordinary call while the rename keeps being refused. A duration here would
// measure the saves' own file I/O (CreateTemp, write, fsync), which is slow on a loaded Windows runner: with
// StateFlushDelay < 0 the second persist saves in line itself.
func TestRetryDoesNotHoldTheEventPath(t *testing.T) {
	sink := &funcSink{name: "s"}
	h, err := New(Config{Secrets: []string{secret}, Sinks: []Sink{sink}, StatePath: filepath.Join(t.TempDir(), "relay.state"), StateFlushDelay: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	var slept, renames atomic.Int32
	h.state.sleep = func(time.Duration) { slept.Add(1) }
	h.state.renameFile = func(string, string) error { renames.Add(1); return errBusy }
	h.state.quick = busyQuick // a refusal that never passes: a real fault read as a passing one
	var wg sync.WaitGroup
	for _, urgent := range []bool{true, false, true, false} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.persist(urgent)
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("persist did not return while the rename was refused")
	}
	if renames.Load() == 0 {
		t.Fatal("no save reached the rename: the test would pass without exercising the retry path")
	}
	if n := slept.Load(); n != 0 {
		t.Fatalf("a save slept %d times: its retries run in line, under the state mutex", n)
	}
}
