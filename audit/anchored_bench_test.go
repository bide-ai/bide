package audit_test

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/journaltest"
)

// BenchmarkAnchoredInsert records one new step per iteration through the audited store, in a run
// that already holds 1,000 records, anchoring each: the cost of anchoring as a run grows.
func BenchmarkAnchoredInsert(b *testing.B) {
	ctx := context.Background()
	_, priv, _ := ed25519.GenerateKey(nil)
	s := agenttest.MustJournal(mustAuditedStore(b, agenttest.MemJournal(), priv, audit.NewMemAnchorLog()))
	fn := func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: []byte(`1`)}, nil
	}
	for i := range 1000 {
		if _, err := journaltest.Do(ctx, s, "run", fmt.Sprintf("seed-%d", i), fn); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		i++
		if _, err := journaltest.Do(ctx, s, "run", fmt.Sprintf("step-%d", i), fn); err != nil {
			b.Fatal(err)
		}
	}
}
