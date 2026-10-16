package sqlite_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/bide-ai/bide/audit"
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
			var wg sync.WaitGroup
			for w := range writers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := range each {
						e, _, err := stores[w].Insert(context.Background(), "run", fmt.Sprintf("w%d-%d", w, i), []byte("x"))
						if err != nil {
							t.Errorf("writer %d write %d: %v", w, i, err)
							return
						}
						owner[e.Seq] = w
					}
				}()
			}
			wg.Wait()
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
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				if err := es[w].Append(context.Background(), fmt.Sprintf("run-%d", w), i, []byte("leaf")); err != nil {
					t.Errorf("writer %d append %d: %v", w, i, err)
					return
				}
			}
		}()
	}
	wg.Wait()
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

// checkTurns fails if writers keep winning the lock back. With the gate (a first-come first-served
// queue, each writer having one write outstanding) a writer that has just committed joins the back
// of the queue, so the next commit is another writer's whenever one is waiting: the share of commits
// that follow a commit of the same writer is small (at most a few per cent on an idle machine and
// 10% on one at load 100, where a writer the scheduler holds back leaves the queue short). Without
// the gate the writer that just released the lock takes it again from the others asleep in SQLite's
// busy handler, and about 95% of commits are repeats. The test reads the share over every commit,
// not the longest wait of one writer: that depends on when the scheduler runs that one goroutine
// (a wait of 343 writes was seen on a loaded machine with the gate in place), which the queue does
// not control. The limit is half.
func checkTurns(t *testing.T, owner []int, writers int) {
	t.Helper()
	repeats := 0
	for i := 1; i < len(owner); i++ {
		if owner[i] == owner[i-1] {
			repeats++
		}
	}
	t.Logf("%d of %d commits follow a commit of the same writer (%d writers)", repeats, len(owner), writers)
	if repeats*2 > len(owner) {
		t.Fatalf("%d of %d commits follow a commit of the same writer (limit one half): writers are not taking turns", repeats, len(owner))
	}
}
