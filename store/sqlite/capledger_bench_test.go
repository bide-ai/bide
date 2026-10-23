package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/capledger"
	"github.com/bide-ai/bide/internal/capledger/capledgertest"
	"github.com/bide-ai/bide/internal/journaltest"
)

// BenchmarkRunsListingWithLedgerRuns decides decision 13 of the caps design (whether the port needs
// RunFilter.ExcludePrefix): every pass over Runs, which Recover and the notifiers make, also meets one
// ledger run per key and window (a per-customer-per-day rule makes one per customer per day), and each
// consumer skips them by IsReservedRun. The cost per pass with 100 ordinary runs and 0, 1,000 and 10,000
// ledger runs, on SQLite.
func BenchmarkRunsListingWithLedgerRuns(b *testing.B) {
	for _, ledgers := range []int{0, 1000, 10000} {
		b.Run(fmt.Sprintf("ledger-runs-%d", ledgers), func(b *testing.B) {
			ctx := context.Background()
			s, err := Open(filepath.Join(b.TempDir(), "runs.db"))
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			j := agenttest.MustJournal(s)
			for i := range 100 {
				if _, err := journaltest.Put(ctx, j, fmt.Sprintf("run-%03d", i), "step", agent.Record{Kind: agent.StepValue, Result: []byte(`1`)}); err != nil {
					b.Fatal(err)
				}
			}
			l := capledgertest.New(j, func() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC) }, 0)
			rule := capledger.Rule{Namespace: "bench", Name: "per_customer", Currency: "USD", Kind: capledger.Day}
			for i := range ledgers {
				a := fmt.Sprintf("a%d", i)
				if _, err := l.Reserve(ctx, capledger.Request{Rule: rule, KeyDigest: capledger.KeyDigest(fmt.Sprintf("customer-%d", i)),
					RunID: "run-000", RootRunID: "run-000", Attempt: a, Marked: true, Amount: 1, Limit: 100}); err != nil {
					b.Fatal(err)
				}
			}
			b.ResetTimer()
			for b.Loop() {
				ordinary, reserved := 0, 0
				for id, err := range s.Runs(ctx, agent.RunFilter{}) {
					if err != nil {
						b.Fatal(err)
					}
					if agent.IsReservedRun(id) {
						reserved++
					} else {
						ordinary++
					}
				}
				if ordinary != 100 || reserved < ledgers {
					b.Fatalf("listed %d ordinary and %d reserved runs", ordinary, reserved)
				}
			}
		})
	}
}
