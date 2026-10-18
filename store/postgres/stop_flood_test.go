package postgres

import (
	"context"
	"os"
	"testing"

	"github.com/bide-ai/bide/internal/stopflood"
)

// The Postgres variant of the SQLite guard: 10k junk entries under a stop's generation names must not push
// SetStop or State() past stopflood.Bound (measured: about 0.15 s). Skips without PG_DSN.
func TestStopFlood_StopStillLandsInBoundedTime(t *testing.T) {
	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		t.Skip("set PG_DSN to run the Postgres stop flood guard")
	}
	ctx := context.Background()
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	stopflood.Run(ctx, t, s, stopflood.Junk)
}
