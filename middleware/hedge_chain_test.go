package middleware_test

import (
	"context"
	"errors"
	"sync"
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
	res, err := a.Run(context.Background(), "r", agent.UserText("q"))
	if err != nil {
		t.Fatal(err)
	}
	out := res.Message
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

// sentModel closes sent when a request reaches it, which is after every Before hook of the
// request, a RateLimit's token included, has run.
type sentModel struct {
	agent.Model
	once sync.Once
	sent chan struct{}
}

func (m *sentModel) Stream(ctx context.Context, req agent.Request) (*agent.Stream, error) {
	m.once.Do(func() { close(m.sent) })
	return m.Model.Stream(ctx, req)
}

// gatedModel answers only once gate is closed.
type gatedModel struct {
	agent.Model
	gate <-chan struct{}
}

func (m *gatedModel) Stream(ctx context.Context, req agent.Request) (*agent.Stream, error) {
	select {
	case <-m.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return m.Model.Stream(ctx, req)
}

// lateFor holds a request to m back by d before it reaches the model handler, as a slow
// scheduler wakeup of its goroutine would.
func lateFor(m agent.Model, d time.Duration) agent.Middleware {
	return func(next agent.ModelHandler) agent.ModelHandler {
		return func(ctx context.Context, call agent.ModelCall) (agent.ModelResponse, error) {
			if call.Model == m {
				time.Sleep(d)
			}
			return next(ctx, call)
		}
	}
}

// A rate limiter counts every request actually sent, wherever it sits in the chain: a hedged
// turn that launches a backup sends two requests, and so does a retried one.
func TestRateLimit_CountsEveryRequestSent(t *testing.T) {
	t.Run("hedge", func(t *testing.T) {
		r := middleware.NewRateLimiter(time.Hour, 2)
		// The backup answers only after the primary was sent: a primary that reaches the model
		// handler after the backup won is never sent and rightly takes no token (see
		// "hedge loser not sent" below), so without the gate the turn may send one request.
		primary := &sentModel{Model: &stubModel{text: "primary", delay: 50 * time.Millisecond}, sent: make(chan struct{})}
		backup := &gatedModel{Model: &stubModel{text: "backup", delay: time.Millisecond}, gate: primary.sent}
		a := agenttest.MustNew(
			primary,
			agenttest.MemJournal(),
			// The primary starts late, so the test does not depend on it being scheduled first.
			agent.WithMiddleware(middleware.RateLimit(r), middleware.Hedge(0, backup), lateFor(primary, 20*time.Millisecond)),
		)
		res, err := a.Run(context.Background(), "r", agent.UserText("q"))
		if err != nil {
			t.Fatal(err)
		}
		if res.Message.Text() != "backup" {
			t.Fatalf("answer = %q, want the backup's", res.Message.Text())
		}
		if !exhausted(t, r) {
			t.Fatal("the limiter has a token left: it counted the hedged turn as one request, not two")
		}
	})
	t.Run("hedge loser not sent", func(t *testing.T) {
		r := middleware.NewRateLimiter(time.Hour, 2)
		primary := &stubModel{text: "primary", delay: 50 * time.Millisecond}
		backup := &stubModel{text: "backup", delay: time.Millisecond}
		// The primary reaches the model handler only after the backup won and the turn ended.
		reached := make(chan struct{})
		afterWin := func(next agent.ModelHandler) agent.ModelHandler {
			return func(ctx context.Context, call agent.ModelCall) (agent.ModelResponse, error) {
				if call.Model != agent.Model(primary) {
					return next(ctx, call)
				}
				defer close(reached)
				<-ctx.Done()
				return next(ctx, call)
			}
		}
		a := agenttest.MustNew(
			primary,
			agenttest.MemJournal(),
			agent.WithMiddleware(middleware.RateLimit(r), middleware.Hedge(0, backup), afterWin),
		)
		if _, err := a.Run(context.Background(), "r", agent.UserText("q")); err != nil {
			t.Fatal(err)
		}
		select {
		case <-reached:
		case <-time.After(5 * time.Second):
			t.Fatal("the primary never reached the model handler")
		}
		if atomic.LoadInt32(&primary.started) != 0 {
			t.Fatal("the primary was sent after the turn ended")
		}
		if exhausted(t, r) {
			t.Fatal("the limiter has no token left: it took one for a request that was never sent")
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
		if _, err := a.Run(context.Background(), "r", agent.UserText("q")); err != nil {
			t.Fatal(err)
		}
		if !exhausted(t, r) {
			t.Fatal("the limiter has a token left: it counted the retried turn as one request, not two")
		}
	})
}
