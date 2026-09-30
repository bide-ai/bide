package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// A row whose record names another step (a row edited or copied in the database) is refused on
// read: under the key "x" it holds a record named run:complete, which would mark the unfinished
// run complete.
func TestPostgres_RowWhoseRecordNamesAnotherStepIsRefused(t *testing.T) {
	s, ctx := openTestStore(t)
	run := uniqueID(t, "rowname-")
	if _, err := s.Do(ctx, run, "x", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: []byte(`1`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	forged, err := agent.JournalEntry("run:complete", agent.Record{Kind: agent.StepValue})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE bide_steps SET data = $1 WHERE run_id = $2 AND name = 'x'`, forged, run); err != nil {
		t.Fatal(err)
	}
	if done, err := agent.IsComplete(ctx, s, run); !errors.Is(err, agent.ErrStorage) {
		t.Errorf("IsComplete = %v, %v; want an ErrStorage error", done, err)
	}
	if _, err := s.History(ctx, run); !errors.Is(err, agent.ErrStorage) || !strings.Contains(err.Error(), `"x"`) {
		t.Errorf("History = %v; want ErrStorage naming the row's key", err)
	}
	ran := false
	if _, err := s.Do(ctx, run, "x", func(context.Context) (agent.Record, error) { ran = true; return agent.Record{}, nil }); !errors.Is(err, agent.ErrStorage) {
		t.Errorf("Do(memoized) = %v; want ErrStorage", err)
	}
	if ran {
		t.Error("Do ran fn for a recorded step")
	}
}

// A record with a field this version does not know still reads.
func TestPostgres_UnknownFieldStillReads(t *testing.T) {
	s, ctx := openTestStore(t)
	run := uniqueID(t, "rowname-")
	if _, err := s.Do(ctx, run, "x", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: []byte(`1`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE bide_steps SET data = $1 WHERE run_id = $2 AND name = 'x'`,
		[]byte(`{"name":"x","kind":"value","result":1,"future_field":{"a":1}}`), run); err != nil {
		t.Fatal(err)
	}
	recs, err := s.History(ctx, run)
	if err != nil || len(recs) != 2 || recs[1].Name != "x" { // the journal header, then the record
		t.Fatalf("History = %+v, %v", recs, err)
	}
	if rec, err := s.Do(ctx, run, "x", func(context.Context) (agent.Record, error) { return agent.Record{}, errors.New("must not run") }); err != nil || string(rec.Result) != "1" {
		t.Fatalf("Do = %+v, %v", rec, err)
	}
}
