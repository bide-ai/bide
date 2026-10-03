package sqlite

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/journalhook"
	"github.com/bide-ai/bide/internal/journaltest"
)

// flipCtx reports itself cancelled after lim Done calls, so a sweep over lim lands the
// cancellation at every point of a Do: in particular after the loser's INSERT and before it
// reloads the winner's record, as when a driver loses its lease mid-step.
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

// Two drivers of one run claim the same attempt marker. A wins; B's insert is ignored. Whatever
// happens to B's context afterwards, B must never be told it won: it would run the side effect
// a second time. It gets A's record or an error.
func TestDo_LoserIsNeverToldItWon(t *testing.T) {
	for lim := int64(0); lim < 80; lim++ {
		p := filepath.Join(t.TempDir(), "j.db")
		a, err := Open(p)
		if err != nil {
			t.Fatal(err)
		}
		j2 := agenttest.MustJournal(a)
		b, err := Open(p)
		if err != nil {
			t.Fatal(err)
		}
		j := agenttest.MustJournal(b)
		ctx := &flipCtx{Context: context.Background(), ch: make(chan struct{})}
		ctx.lim.Store(1 << 40)
		got, err := journaltest.Do(ctx, j, "r", "attempt:x", func(context.Context) (agent.Record, error) {
			if _, _, err := agent.ClaimAttempt(context.Background(), j2, "r", "attempt:x", agent.Record{Kind: agent.StepAttempt}); err != nil {
				t.Fatal(err)
			}
			ctx.n.Store(0)
			ctx.lim.Store(lim)
			return journalhook.WithClaim(agent.Record{Kind: agent.StepAttempt}, "B").(agent.Record), nil
		})
		if err == nil && got.ClaimID() == "B" {
			rec, _, _ := j2.Get(context.Background(), "r", "attempt:x")
			t.Errorf("cancel after %d checks: the losing driver was told it won; the journal holds claim %q", lim, rec.ClaimID())
		}
		b.Close()
		a.Close()
	}
}
