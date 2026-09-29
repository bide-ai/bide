package postgreslog_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/bide-ai/bide/govern"
	"github.com/bide-ai/bide/govern/postgreslog"
)

// uniqueID returns prefix plus the test name and a per-run suffix, so the test can run again
// against the same database or Redis instance without seeing a previous run's records.
func uniqueID(t *testing.T, prefix string) string {
	return fmt.Sprintf("%s%s-%d", prefix, t.Name(), time.Now().UnixNano())
}

// The Postgres adapter satisfies the govern.EventLog port (asserted here so the adapter package
// need not import govern, keeping the coupling one-directional).
var _ govern.EventLog = (*postgreslog.Log)(nil)

func openTestLog(t *testing.T) (*postgreslog.Log, context.Context) {
	t.Helper()
	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		t.Skip("set PG_DSN to run the Postgres event-log integration test")
	}
	ctx := context.Background()
	l, err := postgreslog.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l, ctx
}

// TestPostgresLog_AppendEventsOrder checks events replay in append order and entities are isolated.
func TestPostgresLog_AppendEventsOrder(t *testing.T) {
	l, ctx := openTestLog(t)
	a := uniqueID(t, "pg-evlog-a-")
	b := uniqueID(t, "pg-evlog-b-")

	for i, e := range []string{"e1", "e2", "e3"} {
		pos, err := l.Append(ctx, a, e)
		if err != nil {
			t.Fatalf("append %s: %v", e, err)
		}
		if pos != int64(i) {
			t.Fatalf("append %s returned position %d, want %d", e, pos, i)
		}
	}
	if _, err := l.Append(ctx, b, "other"); err != nil {
		t.Fatalf("append other: %v", err)
	}

	got, err := l.Events(ctx, a, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "e1" || got[1] != "e2" || got[2] != "e3" {
		t.Fatalf("entity a events = %v, want [e1 e2 e3] in order", got)
	}
	if tail, err := l.Events(ctx, a, 1); err != nil || len(tail) != 2 || tail[0] != "e2" || tail[1] != "e3" {
		t.Fatalf("entity a events from position 1 = %v (%v), want [e2 e3]", tail, err)
	}
	gotB, err := l.Events(ctx, b, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotB) != 1 || gotB[0] != "other" {
		t.Fatalf("entity b events = %v, want [other] (entities must be isolated)", gotB)
	}
}
