package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/audit/eventstoretest"
)

// Every handle is a Store of its own on one file, so the suite races appends across connections
// and processes' worth of pools, not within one pool.
func TestSQLite_EventStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	eventstoretest.Run(t, func(t *testing.T) audit.EventStore {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s.EventStore()
	})
}

func TestSQLite_EventStoreShared(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	eventstoretest.Run(t, func(*testing.T) audit.EventStore { return s.EventStore() })
}

// A trail survives closing and reopening the file: a committed Append is durable.
func TestSQLite_EventStoreSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "events.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i, l := range []string{"a", "b", "c"} {
		if err := s.EventStore().Append(ctx, "run", i, []byte(l)); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.EventStore().Load(ctx, "run")
	if err != nil || len(got) != 3 || string(got[2]) != "c" {
		t.Fatalf("after reopen: %q, %v", got, err)
	}
	if err := s.EventStore().Append(ctx, "run", 1, []byte("X")); !errors.Is(err, audit.ErrSeqTaken) {
		t.Fatalf("a different leaf after reopen: %v, want ErrSeqTaken", err)
	}
}

// The events table follows WithTablePrefix, so two prefixes on one file keep separate trails, and
// a file created before the table existed gains it on Open.
func TestSQLite_EventStorePrefixAndMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "events.db")
	a, err := Open(path, WithTablePrefix("a_"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(path, WithTablePrefix("b_"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := a.EventStore().Append(ctx, "run", 0, []byte("a")); err != nil {
		t.Fatal(err)
	}
	if got, err := b.EventStore().Load(ctx, "run"); err != nil || got != nil {
		t.Fatalf("the other prefix sees %q, %v", got, err)
	}
	// Drop the table, as in a file of an earlier version: Open creates it.
	if _, err := a.w.ExecContext(ctx, `DROP TABLE a_events`); err != nil {
		t.Fatal(err)
	}
	a2, err := Open(path, WithTablePrefix("a_"))
	if err != nil {
		t.Fatal(err)
	}
	defer a2.Close()
	if err := a2.EventStore().Append(ctx, "run", 0, []byte("a")); err != nil {
		t.Fatalf("Append on a migrated file: %v", err)
	}
}
