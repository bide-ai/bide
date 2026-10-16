package sqlite_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/filegate"
	"github.com/bide-ai/bide/store/sqlite"
)

// Writers that insert in a tight loop, each on its own Store over one file, take turns: no writer
// waits through another's run of writes. SQLite's busy handler alone is not a queue, so a writer
// that has just released the lock wins it back, and one unlucky writer waits for everyone else's
// whole run (the SQLITE_BUSY an application meets on a slow disk, where that run outlasts the busy
// timeout). The commit order is the file's own (the step's Seq), so a writer's turn is not read
// through the scheduler; checkTurns says what is asserted of it.
func TestWritersOnOneFileTakeTurns(t *testing.T) {
	const writers, each = 24, 60
	opens := map[string]func(t *testing.T, path string) *sqlite.Store{
		"Open": func(t *testing.T, path string) *sqlite.Store {
			s, err := sqlite.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
			return s
		},
		// A db the caller opened: the file is found from the db, not a path.
		"New": func(t *testing.T, path string) *sqlite.Store {
			db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(30000)&_pragma=journal_mode(WAL)")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			s, err := sqlite.New(context.Background(), db)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
			return s
		},
	}
	for name, open := range opens {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "turns.db")
			stores := make([]*sqlite.Store, writers)
			for i := range stores {
				stores[i] = open(t, path)
			}
			owner := make([]int, writers*each+1)
			burst(t, path, writers, func(w int) {
				for i := range each {
					e, _, err := stores[w].Insert(context.Background(), "run", fmt.Sprintf("w%d-%d", w, i), []byte("x"))
					if err != nil {
						t.Errorf("writer %d write %d: %v", w, i, err)
						return
					}
					owner[e.Seq] = w
				}
			})
			if !t.Failed() {
				checkTurns(t, owner[1:], writers)
			}
		})
	}
}

// The audit trail's Append takes turns too. The order is read back from the file (see commitOrder),
// not recorded by the writers after Append returns.
func TestEventAppendersOnOneFileTakeTurns(t *testing.T) {
	const writers, each = 24, 40
	path := filepath.Join(t.TempDir(), "events.db")
	var es []audit.EventStore
	for range writers {
		s, err := sqlite.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		es = append(es, s.EventStore())
	}
	order := commitOrder(t, path)
	// Each writer appends to a run of its own, so that no append is refused for a taken position.
	// All of them still share the file's one write lock.
	burst(t, path, writers, func(w int) {
		for i := range each {
			if err := es[w].Append(context.Background(), fmt.Sprintf("run-%d", w), i, []byte("leaf")); err != nil {
				t.Errorf("writer %d append %d: %v", w, i, err)
				return
			}
		}
	})
	if t.Failed() {
		return
	}
	idx := map[string]int{}
	var owners []int
	for _, r := range order() {
		if _, ok := idx[r]; !ok {
			idx[r] = len(idx)
		}
		owners = append(owners, idx[r])
	}
	if len(owners) != writers*each {
		t.Fatalf("%d commits recorded, want %d", len(owners), writers*each)
	}
	checkTurns(t, owners, writers)
}

// burst runs write(w) for each of writers goroutines so that they all start queued: it takes the
// file's turn (the gate every Store on the file queues on), starts the writers, waits until every one
// is waiting for it, and only then lets them go. Without that the writers' overlap is up to the
// scheduler (on one core a writer can finish its run before the next has started, and no queue is
// exercised at all).
func burst(t *testing.T, path string, writers int, write func(w int)) {
	t.Helper()
	g := filegate.For(path)
	defer g.Close()
	leave, err := g.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			write(w)
		}()
	}
	deadline := time.Now().Add(time.Minute)
	for g.Waiting() < writers {
		if t.Failed() || time.Now().After(deadline) {
			leave()
			wg.Wait()
			t.Fatalf("%d of %d writers are queued on the file's gate: the writes do not take turns on it", g.Waiting(), writers)
		}
		time.Sleep(time.Millisecond)
	}
	leave()
	wg.Wait()
}

// checkTurns fails if writers do not take turns after a burst that started with all of them queued
// (see burst). The gate is a first-come first-served queue and a writer that has committed joins the
// back of it, so the first writers commits are every writer once, in the order they queued, and
// after that a commit follows one of the same writer only when the others have not asked again yet.
// Both are read from the order the file committed in, never from when a goroutine ran. Without the
// queue the writer that just released the lock takes it again from the others asleep in SQLite's
// busy handler: the first commits repeat writers and about 95% of all commits are repeats; the
// limit on the share is a quarter, against at most a few per cent seen with the queue.
func checkTurns(t *testing.T, owner []int, writers int) {
	t.Helper()
	first := map[int]bool{}
	for _, w := range owner[:writers] {
		first[w] = true
	}
	repeats := 0
	for i := 1; i < len(owner); i++ {
		if owner[i] == owner[i-1] {
			repeats++
		}
	}
	t.Logf("%d writers in the first %d commits; %d of %d commits follow a commit of the same writer", len(first), writers, repeats, len(owner))
	if len(first) != writers {
		t.Errorf("only %d of %d writers committed in the first %d commits: the queue did not serve each writer once", len(first), writers, writers)
	}
	if repeats*4 > len(owner) {
		t.Errorf("%d of %d commits follow a commit of the same writer (limit a quarter): writers are not taking turns", repeats, len(owner))
	}
}
