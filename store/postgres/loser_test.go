package postgres

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/journalhook"
	"github.com/bide-ai/bide/internal/journaltest"
)

// flipCtx reports itself cancelled after lim Done calls, so a sweep over lim lands the
// cancellation at every point of a Do, including after the loser's INSERT and before it reloads
// the winner's record.
type flipCtx struct {
	context.Context
	n, lim atomic.Int64
	ch     chan struct{}
	once   sync.Once
}

func (c *flipCtx) Done() <-chan struct{} {
	if c.n.Add(1) > c.lim.Load() {
		c.once.Do(func() { close(c.ch) })
	}
	return c.ch
}

func (c *flipCtx) Err() error {
	select {
	case <-c.ch:
		return context.Canceled
	default:
		return nil
	}
}

// Two nodes claim the same attempt marker. A wins; B's insert conflicts. Whatever happens to B's
// context afterwards, B must never be told it won. Skips without PG_DSN.
func TestDo_LoserIsNeverToldItWon(t *testing.T) {
	a, _ := openTestStore(t)
	j := agenttest.MustJournal(a)
	b, err := Open(context.Background(), os.Getenv("PG_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	j2 := agenttest.MustJournal(b)
	defer b.Close()
	for lim := int64(0); lim < 60; lim++ {
		runID := uniqueID(t, "pg-loser-")
		ctx := &flipCtx{Context: context.Background(), ch: make(chan struct{})}
		ctx.lim.Store(1 << 40)
		got, err := journaltest.Do(ctx, j2, runID, "attempt:x", func(context.Context) (agent.Record, error) {
			if _, _, err := agent.ClaimAttempt(context.Background(), j, runID, "attempt:x", agent.Record{Kind: agent.StepAttempt}); err != nil {
				t.Fatal(err)
			}
			ctx.n.Store(0)
			ctx.lim.Store(lim)
			return journalhook.WithClaim(agent.Record{Kind: agent.StepAttempt}, "B").(agent.Record), nil
		})
		if err == nil && got.ClaimID() == "B" {
			t.Errorf("cancel after %d checks: the losing node was told it won", lim)
		}
	}
}
