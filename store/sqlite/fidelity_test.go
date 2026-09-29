package sqlite

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/durabletest"
)

func openFileStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// The live record Do returns is the record a replay reads back, for any content.
func TestSQLite_Fidelity(t *testing.T) {
	durabletest.Run(t, func(t *testing.T) agent.Durable { return openFileStore(t) })
}

// The journal holds exactly the canonical encoding of the record it hands back, so an audit leaf
// computed from a replayed record is the bytes on disk.
func TestSQLite_PersistsCanonicalBytes(t *testing.T) {
	s := openFileStore(t)
	ctx := context.Background()
	for i, c := range durabletest.Cases() {
		runID := fmt.Sprintf("canon-%d", i)
		live, err := s.Do(ctx, runID, "step", func(context.Context) (agent.Record, error) { return c.Record, nil })
		if err != nil {
			t.Fatalf("%s: Do: %v", c.Name, err)
		}
		var data []byte
		if err := s.db.QueryRowContext(ctx, `SELECT data FROM steps WHERE run_id = ? AND name = ?`, runID, "step").Scan(&data); err != nil {
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
