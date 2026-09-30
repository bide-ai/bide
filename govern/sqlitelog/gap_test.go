package sqlitelog_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/govern/sqlitelog"
)

// An entity's positions are dense: the event at position p is the p-th event read. A log whose
// rows skip a position (a row deleted or its seq rewritten outside this adapter) cannot say what
// happened at the missing position, so Events refuses it with ErrProtocol rather than return a
// list whose indices are not the positions, which a governor would fold as if they were.
func TestEvents_RefusesAGapInPositions(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "events.db")
	l, err := sqlitelog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for i, e := range []string{"e0", "e1", "e2"} {
		if pos, err := l.Append(ctx, "x", "id"+e, e); err != nil || pos != int64(i) {
			t.Fatalf("Append %s = %d, %v", e, pos, err)
		}
	}
	// A position before the first is the first: every event, from position 0.
	if evs, err := l.Events(ctx, "x", -1); err != nil || len(evs) != 3 || evs[0] != "e0" {
		t.Fatalf("Events(from -1) = %q, %v; want [e0 e1 e2]", evs, err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `DELETE FROM events WHERE entity = 'x' AND seq = 1`); err != nil {
		t.Fatal(err)
	}
	for _, from := range []int64{0, 1} {
		evs, err := l.Events(ctx, "x", from)
		if !errors.Is(err, agent.ErrProtocol) {
			t.Errorf("Events(from %d) = %q, %v; want ErrProtocol for the missing position 1", from, evs, err)
		}
	}
	if evs, err := l.Events(ctx, "x", 2); err != nil || len(evs) != 1 || evs[0] != "e2" {
		t.Errorf("Events(from 2) = %q, %v; want [e2]", evs, err)
	}
}
