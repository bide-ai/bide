package audit_test

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/capledger"
	"github.com/bide-ai/bide/internal/capledger/capledgertest"
)

// BenchmarkCapLedgerReserve is the cost of one cap reservation (docs/design/untrusted-content-and-
// amount-caps.md, section 6.5, "Cost, scale and the run-listing consumers"): the intent, the index
// entry, the slot and the audit record, in a ledger that already holds preload slots, on a MemStore
// and on an AuditedStore with and without WithMinInterval. The audited store anchors a run's head on
// every growth unless WithMinInterval holds growth back, so the ledger's cost there is the anchoring
// of a run that grows with every reservation.
// measured is the number of reservations one iteration makes.
const measured = 100

func BenchmarkCapLedgerReserve(b *testing.B) {
	for _, store := range []struct {
		name string
		mk   func(testing.TB) agent.Store
	}{
		{"mem", func(testing.TB) agent.Store { return agent.NewMemStore() }},
		{"audited", func(tb testing.TB) agent.Store { return auditedForBench(tb, 0) }},
		{"audited-mininterval-1m", func(tb testing.TB) agent.Store { return auditedForBench(tb, time.Minute) }},
	} {
		for _, preload := range []int{0, 1000} {
			b.Run(fmt.Sprintf("%s/preload-%d", store.name, preload), func(b *testing.B) {
				ctx := context.Background()
				j := agenttest.MustJournal(store.mk(b))
				l := capledgertest.New(j, func() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC) }, 0)
				rule := capledger.Rule{Namespace: "bench", Name: "total", Currency: "USD", Kind: capledger.Day}
				reserve := func(i int) {
					a := fmt.Sprintf("a%d", i)
					if _, err := l.Reserve(ctx, capledger.Request{Rule: rule, KeyDigest: capledger.KeyDigest(""), RunID: "run-" + a,
						RootRunID: "run", ToolUseID: a, AttemptID: "1", Marked: true, Amount: 1, Limit: 1 << 40}); err != nil {
						b.Fatal(err)
					}
				}
				for i := range preload {
					reserve(i)
				}
				// A fixed batch, so the ledger's size is known while it is measured: it grows from preload to
				// preload+measured slots (at most 10% for preload 1000), whatever b.N is. Run it with
				// -benchtime=1x: one batch per sub-benchmark; ns/reserve and B/reserve are per reservation.
				b.ReportAllocs()
				b.ResetTimer()
				for i := preload; i < preload+measured; i++ {
					reserve(i)
				}
				b.StopTimer()
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/measured, "ns/reserve")
				b.ReportMetric(float64(preload+measured), "slots-at-end")
			})
		}
	}
}

func auditedForBench(tb testing.TB, minInterval time.Duration) *audit.AuditedStore {
	tb.Helper()
	_, priv, _ := ed25519.GenerateKey(nil)
	s, err := audit.NewAuditedStore(agent.NewMemStore(), edS(priv), audit.NewMemAnchorLog())
	if err != nil {
		tb.Fatal(err)
	}
	if minInterval > 0 {
		s = s.WithMinInterval(minInterval)
	}
	tb.Cleanup(func() { _ = s.Flush(context.Background()) })
	return s
}
