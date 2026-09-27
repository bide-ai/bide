package postgreslog_test

import (
	"context"
	"os"
	"testing"

	"github.com/blackwell-systems/bide/govern"
	"github.com/blackwell-systems/bide/govern/postgreslog"
)

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
	a := "pg-evlog-a-" + t.Name()
	b := "pg-evlog-b-" + t.Name()

	for _, e := range []string{"e1", "e2", "e3"} {
		if err := l.Append(ctx, a, e); err != nil {
			t.Fatalf("append %s: %v", e, err)
		}
	}
	if err := l.Append(ctx, b, "other"); err != nil {
		t.Fatalf("append other: %v", err)
	}

	got, err := l.Events(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "e1" || got[1] != "e2" || got[2] != "e3" {
		t.Fatalf("entity a events = %v, want [e1 e2 e3] in order", got)
	}
	gotB, err := l.Events(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotB) != 1 || gotB[0] != "other" {
		t.Fatalf("entity b events = %v, want [other] (entities must be isolated)", gotB)
	}
}
