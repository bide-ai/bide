package middleware_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/middleware"
)

// Middleware installed inside Hedge wraps every target, backups included, not only the primary.
func TestHedge_InnerMiddlewareWrapsEveryTarget(t *testing.T) {
	primary := &stubModel{text: "primary", delay: 50 * time.Millisecond}
	backup := &stubModel{text: "backup", delay: time.Millisecond}
	var calls atomic.Int32
	count := func(next agent.ModelHandler) agent.ModelHandler {
		return func(ctx context.Context, call agent.ModelCall) (agent.ModelResponse, error) {
			calls.Add(1)
			return next(ctx, call)
		}
	}
	a := agenttest.MustNew(primary, agenttest.MemJournal(), agent.WithMiddleware(middleware.Hedge(0, backup), count))
	out, err := a.Run(context.Background(), "r", "q")
	if err != nil {
		t.Fatal(err)
	}
	if out.Text() != "backup" {
		t.Fatalf("answer = %q, want the backup's", out.Text())
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("inner middleware saw %d calls, want 2 (the primary and the backup)", n)
	}
}

// exhausted reports whether r has no token left: a limited call gives up after a short wait.
func exhausted(t *testing.T, r *middleware.RateLimiter) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := agent.CallModel(ctx, &stubModel{text: "x"}, agent.Request{}, middleware.RateLimit(r))
	return errors.Is(err, context.DeadlineExceeded)
}

// A rate limiter counts every request actually sent, wherever it sits in the chain: a hedged
// turn that launches a backup sends two requests, and so does a retried one.
func TestRateLimit_CountsEveryRequestSent(t *testing.T) {
	t.Run("hedge", func(t *testing.T) {
		r := middleware.NewRateLimiter(time.Hour, 2)
		primary := &stubModel{text: "primary", delay: 50 * time.Millisecond}
		backup := &stubModel{text: "backup", delay: time.Millisecond}
		a := agenttest.MustNew(
			primary,
			agenttest.MemJournal(),
			agent.WithMiddleware(middleware.RateLimit(r), middleware.Hedge(0, backup)),
		)
		if _, err := a.Run(context.Background(), "r", "q"); err != nil {
			t.Fatal(err)
		}
		if !exhausted(t, r) {
			t.Fatal("the limiter has a token left: it counted the hedged turn as one request, not two")
		}
	})
	t.Run("retry", func(t *testing.T) {
		r := middleware.NewRateLimiter(time.Hour, 2)
		m := &billedModel{u: billed, bad: 1}
		a := agenttest.MustNew(
			m,
			agenttest.MemJournal(),
			agent.WithMiddleware(middleware.RateLimit(r), middleware.Retry(1, middleware.WithBackoff(0, 0))),
		)
		if _, err := a.Run(context.Background(), "r", "q"); err != nil {
			t.Fatal(err)
		}
		if !exhausted(t, r) {
			t.Fatal("the limiter has a token left: it counted the retried turn as one request, not two")
		}
	})
}
