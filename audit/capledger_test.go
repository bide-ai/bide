package audit_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/capledger"
	"github.com/bide-ai/bide/internal/capledger/capledgertest"
)

// C10: the cap ledger on an AuditedStore, with and without WithMinInterval: goroutines reserving against
// one window never overshoot, and after a Flush the run's head is published and the ledger reads back.
func TestCapLedger_OnAnAuditedStore(t *testing.T) {
	for _, minInterval := range []time.Duration{0, time.Minute} {
		t.Run(fmt.Sprintf("min-interval-%s", minInterval), func(t *testing.T) {
			ctx := context.Background()
			s := auditedForBench(t, minInterval)
			j := agenttest.MustJournal(s)
			now := func() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC) }
			rule := capledger.Rule{Namespace: "audited", Name: "total", Currency: "USD", Kind: capledger.Day}
			const n, limit = 24, 10
			var wg sync.WaitGroup
			var ok, over atomic.Int32
			for i := range n {
				wg.Add(1)
				go func() {
					defer wg.Done()
					a := fmt.Sprintf("a%d", i)
					_, err := capledgertest.New(j, now, 0).Reserve(ctx, capledger.Request{Rule: rule, KeyDigest: capledger.KeyDigest(""),
						RunID: "run-" + a, RootRunID: "run", Attempt: a, Marked: true, Amount: 1, Limit: limit})
					switch {
					case err == nil:
						ok.Add(1)
					case errors.Is(err, capledger.ErrOverLimit):
						over.Add(1)
					default:
						t.Errorf("%s: %v", a, err)
					}
				}()
			}
			wg.Wait()
			if ok.Load() != limit || over.Load() != n-limit {
				t.Fatalf("%d reserved, %d refused; want %d and %d", ok.Load(), over.Load(), limit, n-limit)
			}
			if err := s.Flush(ctx); err != nil {
				t.Fatalf("Flush: %v", err)
			}
			l := capledgertest.New(j, now, 0)
			live, err := l.Live(ctx, l.LedgerRun(rule, "2026-10-22", capledger.KeyDigest("")))
			if err != nil || len(live) != limit {
				t.Fatalf("live = %d slots, %v; want %d", len(live), err, limit)
			}
		})
	}
}
