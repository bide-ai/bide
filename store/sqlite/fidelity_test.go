package sqlite

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/storetest"
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

// The store meets every store requirement, through several handles on one file.
func TestSQLite_Store(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	storetest.Run(t, func(t *testing.T) agent.Store {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	})
}

// The in-memory store, one connection for every role, meets them too.
func TestSQLite_MemoryStore(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	storetest.Run(t, func(*testing.T) agent.Store { return s })
}

// The live record the transitional Do returns is the record a replay reads back, for any content.
func TestSQLite_Fidelity(t *testing.T) {
	storetest.RunDurable(t, func(t *testing.T) agent.Durable { return openFileStore(t) })
}

// The journal holds exactly the canonical encoding of the record it hands back, so an audit leaf
// computed from a replayed record is the bytes on disk.
func TestSQLite_PersistsCanonicalBytes(t *testing.T) {
	s := openFileStore(t)
	ctx := context.Background()
	for i, c := range storetest.Cases() {
		runID := fmt.Sprintf("canon-%d", i)
		live, err := s.Do(ctx, runID, "step", func(context.Context) (agent.Record, error) { return c.Record, nil })
		if err != nil {
			t.Fatalf("%s: Do: %v", c.Name, err)
		}
		var data []byte
		if err := s.r.QueryRowContext(ctx, `SELECT data FROM bide_steps WHERE run_id = ? AND name = ?`, runID, "step").Scan(&data); err != nil {
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
