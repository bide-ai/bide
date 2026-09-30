package postgres

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// BenchmarkPostgresInsert records one new step per iteration in one run, through the step API
// both sides of the redesign share. Skips without PG_DSN.
func BenchmarkPostgresInsert(b *testing.B) {
	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		b.Skip("set PG_DSN to run the Postgres benchmark")
	}
	ctx := context.Background()
	s, err := Open(ctx, dsn)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	run := fmt.Sprintf("bench-%d", time.Now().UnixNano())
	fn := func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: []byte(`{"ok":true}`)}, nil
	}
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		i++
		if _, err := s.Do(ctx, run, fmt.Sprintf("step-%d", i), fn); err != nil {
			b.Fatal(err)
		}
	}
}
