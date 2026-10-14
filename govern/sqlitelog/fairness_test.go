package sqlitelog_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/bide-ai/bide/govern/sqlitelog"
)

// Writers that append in a tight loop, each on its own handle to one file, take turns: no writer
// waits through another's run of appends. SQLite's busy handler alone is not a queue, so a writer
// that has just released the lock wins it back, and one unlucky writer waits for everyone else's
// whole run (the SQLITE_BUSY an application meets on a slow disk, where that run outlasts
// busy_timeout). Between two appends of one writer, every other writer, each having one append
// outstanding at a time, gets about one; the bound below is generous against scheduling noise and
// far below the hundreds of appends one writer makes in a row without the queue.
func TestAppend_WritersOnOneFileTakeTurns(t *testing.T) {
	const writers, each = 24, 60
	path := filepath.Join(t.TempDir(), "turns.db")
	ctx := context.Background()
	logs := make([]*sqlitelog.Log, writers)
	for i := range logs {
		l, err := sqlitelog.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		logs[i] = l
	}
	owner := make([]int, writers*each) // owner[pos] is the writer whose append got position pos
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				ev := fmt.Sprintf("w%d-%d", w, i)
				pos, err := logs[w].Append(ctx, "e", "id-"+ev, ev)
				if err != nil {
					t.Errorf("writer %d append %d: %v", w, i, err)
					return
				}
				owner[pos] = w
			}
		}()
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	last := make([]int, writers)
	for i := range last {
		last[i] = -1
	}
	const maxGap = 3 * writers
	for pos, w := range owner {
		if last[w] >= 0 && pos-last[w] > maxGap {
			t.Fatalf("writer %d waited through %d appends between two of its own (limit %d): writers are not taking turns", w, pos-last[w]-1, maxGap)
		}
		last[w] = pos
	}
}
