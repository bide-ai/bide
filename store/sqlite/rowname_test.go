package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/journaltest"
)

func rownameStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "rowname.db"))
	if err != nil {
		t.Fatal(err)
	}
	j := agenttest.MustJournal(s)
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	if _, err := journaltest.Do(ctx, j, "r", "x", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: []byte(`1`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	return s, ctx
}

// A row whose record names another step (a row edited or copied in the database) is refused on
// read: under the key "x" it holds a record named run:complete, which would mark the unfinished
// run complete.
func TestStore_RowWhoseRecordNamesAnotherStepIsRefused(t *testing.T) {
	s, ctx := rownameStore(t)
	j := agenttest.MustJournal(s)
	forged, err := agent.JournalEntry("run:complete", agent.Record{Kind: agent.StepValue})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.w.ExecContext(ctx, `UPDATE bide_steps SET data = ? WHERE run_id = 'r' AND name = 'x'`, forged); err != nil {
		t.Fatal(err)
	}
	if done, err := agent.IsComplete(ctx, j, "r"); !errors.Is(err, agent.ErrStorage) {
		t.Errorf("IsComplete = %v, %v; want an ErrStorage error", done, err)
	}
	if _, err := j.History(ctx, "r"); !errors.Is(err, agent.ErrStorage) || !strings.Contains(err.Error(), `"x"`) {
		t.Errorf("History = %v; want ErrStorage naming the row's key", err)
	}
	ran := false
	if _, err := journaltest.Do(ctx, j, "r", "x", func(context.Context) (agent.Record, error) { ran = true; return agent.Record{}, nil }); !errors.Is(err, agent.ErrStorage) {
		t.Errorf("Do(memoized) = %v; want ErrStorage", err)
	}
	if ran {
		t.Error("Do ran fn for a recorded step")
	}
}

// A record with a field this version does not know still reads.
func TestStore_UnknownFieldStillReads(t *testing.T) {
	s, ctx := rownameStore(t)
	j := agenttest.MustJournal(s)
	if _, err := s.w.ExecContext(ctx, `UPDATE bide_steps SET data = ? WHERE run_id = 'r' AND name = 'x'`,
		[]byte(`{"name":"x","kind":"value","result":1,"future_field":{"a":1}}`)); err != nil {
		t.Fatal(err)
	}
	recs, err := j.History(ctx, "r")
	if err != nil || len(recs) != 2 || recs[1].Name != "x" { // the journal header, then the record
		t.Fatalf("History = %+v, %v", recs, err)
	}
	if rec, err := journaltest.Do(ctx, j, "r", "x", func(context.Context) (agent.Record, error) { return agent.Record{}, errors.New("must not run") }); err != nil || string(rec.Result) != "1" {
		t.Fatalf("Do = %+v, %v", rec, err)
	}
}
