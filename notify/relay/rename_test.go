package relay

import (
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

var errBusy = errors.New("busy")

func isBusy(err error) bool { return errors.Is(err, errBusy) }

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
	s := &stateStore{path: path, syncDir: func(string) error { return nil }, retryable: isBusy,
		sleep: func(time.Duration) { pauses++ },
		renameFile: func(from, to string) error {
			if fails.Add(-1) >= 0 {
				return errBusy
			}
			return os.Rename(from, to)
		}}
	if err := s.save(d, c); err != nil {
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
	s := &stateStore{path: path, syncDir: func(string) error { return nil }, retryable: isBusy, sleep: func(time.Duration) {},
		renameFile: func(string, string) error { return errBusy }}
	err := s.save(d, c)
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
	h.state.renameFile = func(from, to string) error {
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
	first := failures()
	waitFor2(t, "a second attempt", func() bool { return failures() > first })
	fail.Store(false)
	waitFor2(t, "the retry to write the state", func() bool {
		n, err := readSeen(t, path)
		return err == nil && n == 3
	})
	h.Close()
}
