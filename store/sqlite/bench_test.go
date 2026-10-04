package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/journaltest"
)

// BenchmarkSQLiteInsert records one new step per iteration in one run of an on-disk store, through
// the step API both sides of the redesign share.
func BenchmarkSQLiteInsert(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	j := agenttest.MustJournal(s)
	defer s.Close()
	ctx := context.Background()
	fn := func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: []byte(`{"ok":true}`)}, nil
	}
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		i++
		if _, err := journaltest.Do(ctx, j, "run", fmt.Sprintf("step-%d", i), fn); err != nil {
			b.Fatal(err)
		}
	}
}
