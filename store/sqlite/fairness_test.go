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
// timeout). Between two writes of one writer, every other writer, each having one write outstanding
// at a time, gets about one; the bound is generous against scheduling noise and far below the
// hundreds of writes one writer makes in a row without the queue (see checkTurns for the bound).
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

// checkTurns fails if a writer is absent from more than 2*writers consecutive commits. The gate is a
// first-come first-served queue and each writer has one write outstanding, so between two writes of
// one writer at most writers-1 others commit, provided it asks again promptly. One lap of slack
// covers a writer the scheduler holds back between its commit and its next request, during which the
// others may go round once more (the measurements, taken from the commit order in the file, are
// exactly writers-1 on a machine at load 100). Without the queue the gaps measured were 75 to 565 writes, over
// the limit.
func checkTurns(t *testing.T, owner []int, writers int) {
	t.Helper()
	maxGap := 2 * writers
	last := make([]int, writers)
	for i := range last {
		last[i] = -1
	}
	worst := 0
	defer func() { t.Logf("longest wait between two writes of one writer: %d others' writes (fair queue: %d at most when every writer is prompt)", worst, writers-1) }()
	for pos, w := range owner {
		if last[w] >= 0 {
			worst = max(worst, pos-last[w]-1)
		}
		if last[w] >= 0 && pos-last[w] > maxGap {
			t.Fatalf("writer %d waited through %d writes between two of its own (limit %d): writers are not taking turns", w, pos-last[w]-1, maxGap)
		}
		last[w] = pos
	}
}
