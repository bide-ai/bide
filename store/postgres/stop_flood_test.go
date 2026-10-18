package postgres

import (
	"context"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/stopflood"
)

// The Postgres variant of the SQLite guard (see there); each size runs in a schema of its own, dropped
// afterwards. Skips without PG_DSN.
func TestStopFlood_StopStillLandsInBoundedTime(t *testing.T) {
	ctx := context.Background()
	stopflood.Guard(ctx, t, func(t *testing.T) agent.Store {
		dsn, schema, _ := freshSchema(t)
		s, err := Open(ctx, dsn, WithSchema(schema))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	})
}
