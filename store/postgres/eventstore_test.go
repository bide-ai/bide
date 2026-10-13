package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/audit/eventstoretest"
)

// Every handle is a Store of its own with its own pool, so the suite races appends across
// connections. Skips without PG_DSN.
func TestPostgres_EventStore(t *testing.T) {
	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		t.Skip("set PG_DSN to run the Postgres integration test")
	}
	eventstoretest.Run(t, func(t *testing.T) audit.EventStore {
		s, err := Open(context.Background(), dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s.EventStore()
	})
}

// One pool shared by every handle: the racing appends then meet on connections of one pool.
func TestPostgres_EventStoreSharedPool(t *testing.T) {
	s, _ := openTestStore(t)
	eventstoretest.Run(t, func(*testing.T) audit.EventStore { return s.EventStore() })
}

// A trail survives a new connection pool: a committed Append is durable.
func TestPostgres_EventStoreSurvivesReopen(t *testing.T) {
	s, ctx := openTestStore(t)
	run := uniqueID(t, "evs-")
	for i, l := range []string{"a", "b", "c"} {
		if err := s.EventStore().Append(ctx, run, i, []byte(l)); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s2, err := Open(ctx, os.Getenv("PG_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, err := s2.EventStore().Load(ctx, run)
	if err != nil || len(got) != 3 || string(got[2]) != "c" {
		t.Fatalf("after reopen: %q, %v", got, err)
	}
	if err := s2.EventStore().Append(ctx, run, 1, []byte("X")); !errors.Is(err, audit.ErrSeqTaken) {
		t.Fatalf("a different leaf after reopen: %v, want ErrSeqTaken", err)
	}
}

// A database from before the event trail has no events table: Open creates it, and a table prefix
// keeps trails apart.
func TestPostgres_EventStorePrefixAndMigration(t *testing.T) {
	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		t.Skip("set PG_DSN to run the Postgres integration test")
	}
	ctx := context.Background()
	prefix := fmt.Sprintf("evt%d_", time.Now().UnixNano())
	a, err := Open(ctx, dsn, WithTablePrefix(prefix+"a_"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(ctx, dsn, WithTablePrefix(prefix+"b_"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	t.Cleanup(func() {
		for _, s := range []*Store{a, b} {
			for _, tbl := range []string{s.t.qEvents, s.t.qSteps, s.t.qLeases, s.t.qVersion} {
				s.db.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+tbl)
			}
			s.db.ExecContext(context.Background(), "DROP FUNCTION IF EXISTS "+s.t.qNextSeq+"(pg_catalog.text)")
		}
	})
	if err := a.EventStore().Append(ctx, "run", 0, []byte("a")); err != nil {
		t.Fatal(err)
	}
	if got, err := b.EventStore().Load(ctx, "run"); err != nil || got != nil {
		t.Fatalf("the other prefix sees %q, %v", got, err)
	}
	if _, err := a.db.ExecContext(ctx, "DROP TABLE "+a.t.qEvents); err != nil {
		t.Fatal(err)
	}
	a2, err := Open(ctx, dsn, WithTablePrefix(prefix+"a_"))
	if err != nil {
		t.Fatalf("Open on a database with no events table: %v", err)
	}
	defer a2.Close()
	if err := a2.EventStore().Append(ctx, "run", 0, []byte("a")); err != nil {
		t.Fatalf("Append on a migrated database: %v", err)
	}
}

// An events table without the unique (run_id, seq) is refused, as the other tables are.
func TestPostgres_EventStoreRefusesTableWithoutUnique(t *testing.T) {
	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		t.Skip("set PG_DSN to run the Postgres integration test")
	}
	ctx := context.Background()
	prefix := fmt.Sprintf("evu%d_", time.Now().UnixNano())
	s, err := Open(ctx, dsn, WithTablePrefix(prefix))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	t.Cleanup(func() {
		for _, tbl := range []string{s.t.qEvents, s.t.qSteps, s.t.qLeases, s.t.qVersion} {
			s.db.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+tbl)
		}
		s.db.ExecContext(context.Background(), "DROP FUNCTION IF EXISTS "+s.t.qNextSeq+"(pg_catalog.text)")
	})
	if _, err := s.db.ExecContext(ctx, "DROP TABLE "+s.t.qEvents); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "CREATE TABLE "+s.t.qEvents+" (run_id text NOT NULL, seq bigint NOT NULL, leaf bytea NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, dsn, WithTablePrefix(prefix)); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("Open with a non-unique events table: %v, want ErrConfig", err)
	}
}
