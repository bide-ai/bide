package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/agent/storetest"
	"github.com/bide-ai/bide/internal/journaltest"
)

// The store meets every store requirement, through several connection pools on one database, as
// several nodes reach it. And the journal keeps what was written: the suite's Fidelity cases check
// that the record a Journal on it returns on the live path is the record a replay reads back, byte
// for byte, for any content a model or tool produces (HTML characters, whitespace, NUL, U+2028,
// invalid UTF-8, key order, number spelling). So a resumed run rebuilds the conversation the live
// run had, and an audit head computed over this store matches one over any other store. Skips
// without PG_DSN.
func TestPostgres_Store(t *testing.T) {
	storetest.Run(t, func(t *testing.T) agent.Store {
		s, _ := openTestStore(t)
		return s
	})
}

// The table holds exactly the canonical encoding of the record the store hands back, so an audit
// leaf computed from a replayed record is the bytes in the database.
func TestPostgres_PersistsCanonicalBytes(t *testing.T) {
	s, ctx := openTestStore(t)
	j := agenttest.MustJournal(s)
	for i, c := range storetest.Cases() {
		runID := uniqueID(t, fmt.Sprintf("pg-canon-%d-", i))
		live, err := journaltest.Do(ctx, j, runID, "step", func(context.Context) (agent.Record, error) { return c.Record, nil })
		if err != nil {
			t.Fatalf("%s: Do: %v", c.Name, err)
		}
		var data []byte
		if err := s.db.QueryRowContext(ctx, `SELECT data FROM `+s.t.steps+` WHERE run_id = $1 AND name = $2`, runID, "step").Scan(&data); err != nil {
			t.Fatalf("%s: read stored bytes: %v", c.Name, err)
		}
		want, err := agent.EncodeRecord(live)
		if err != nil {
			t.Fatalf("%s: EncodeRecord: %v", c.Name, err)
		}
		if !bytes.Equal(data, want) {
			t.Fatalf("%s: stored %q, but the returned record encodes to %q", c.Name, data, want)
		}
	}
}

// Steps recorded concurrently on one run (Parallel tasks, sibling tool calls) each get their own
// position, so History returns one fixed order every time.
func TestDo_ConcurrentStepsGetDistinctPositions(t *testing.T) {
	s, ctx := openTestStore(t)
	j := agenttest.MustJournal(s)
	runID := uniqueID(t, "pg-seq-")
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := journaltest.Do(ctx, j, runID, fmt.Sprintf("task-%d", i), func(context.Context) (agent.Record, error) {
				return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`1`)}, nil
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var dupes int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT seq FROM `+s.t.steps+` WHERE run_id = $1 GROUP BY seq HAVING count(*) > 1) d`, runID).Scan(&dupes); err != nil {
		t.Fatal(err)
	}
	if dupes != 0 {
		t.Fatalf("%d positions are shared by more than one step, so History's order is not fixed", dupes)
	}
}
