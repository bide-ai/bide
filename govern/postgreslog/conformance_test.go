package postgreslog_test

import (
	"context"
	"os"
	"testing"

	"github.com/bide-ai/bide/govern"
	"github.com/bide-ai/bide/govern/eventlogtest"
	"github.com/bide-ai/bide/govern/postgreslog"
)

// The Postgres log meets the EventLog contract, with each writer on its own connection pool, as
// separate processes would be.
func TestPostgresLog_Conformance(t *testing.T) {
	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		t.Skip("set PG_DSN to run the Postgres event-log integration test")
	}
	eventlogtest.Run(t, func(t *testing.T) govern.EventLog {
		l, err := postgreslog.Open(context.Background(), dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		return l
	})
}
