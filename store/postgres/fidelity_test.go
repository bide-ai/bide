package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// The journal keeps what was written, byte for byte: a tool result's JSON comes back exactly as
// recorded, so an audit head computed over this store matches one over any other store, and a
// replayed step returns what the live step returned. Skips without PG_DSN.
func TestDo_RecordsRoundTripExactly(t *testing.T) {
	s, ctx := openTestStore(t)
	cases := map[string]json.RawMessage{
		"key order":   json.RawMessage(`{"b":1,"a":2}`),
		"NUL in text": json.RawMessage(`"before\u0000after"`),
	}
	for name, result := range cases {
		t.Run(name, func(t *testing.T) {
			runID := uniqueID(t, "pg-fidelity-")
			live, err := s.Do(ctx, runID, "tool", func(context.Context) (agent.Record, error) {
				return agent.Record{Kind: agent.StepToolResult, ToolUseID: "c1", Result: result}, nil
			})
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			hist, err := s.History(ctx, runID)
			if err != nil || len(hist) != 1 {
				t.Fatalf("History = %v, %v", hist, err)
			}
			if string(live.Result) != string(result) || string(hist[0].Result) != string(result) {
				t.Fatalf("recorded %s; live Do returned %s, History returned %s", result, live.Result, hist[0].Result)
			}
		})
	}
}

// Steps recorded concurrently on one run (Parallel tasks, sibling tool calls) each get their own
// position, so History returns one fixed order every time.
func TestDo_ConcurrentStepsGetDistinctPositions(t *testing.T) {
	s, ctx := openTestStore(t)
	runID := uniqueID(t, "pg-seq-")
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Do(ctx, runID, fmt.Sprintf("task-%d", i), func(context.Context) (agent.Record, error) {
				return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`1`)}, nil
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var dupes int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT seq FROM `+stepsTable+` WHERE run_id = $1 GROUP BY seq HAVING count(*) > 1) d`, runID).Scan(&dupes); err != nil {
		t.Fatal(err)
	}
	if dupes != 0 {
		t.Fatalf("%d positions are shared by more than one step, so History's order is not fixed", dupes)
	}
}
