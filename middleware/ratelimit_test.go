package middleware_test

import (
	"context"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/middleware"
)

// An interval of zero is a rate of one call per zero time, which is no limit. The limiter must
// not run out of tokens after the first burst and block every later call until its context ends.
func TestRateLimiter_ZeroIntervalDoesNotBlock(t *testing.T) {
	for _, interval := range []time.Duration{0, -time.Second} {
		r := middleware.NewRateLimiter(interval, 1)
		h := middleware.RateLimit(r)(func(context.Context, agent.Request) (agent.Message, agent.Usage, error) {
			return agent.Message{}, agent.Usage{}, nil
		})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		for i := range 5 {
			if _, _, err := h(ctx, agent.Request{}); err != nil {
				t.Fatalf("interval %v: call %d: %v", interval, i, err)
			}
		}
		cancel()
	}
}

// A call whose context has already ended must not use up a token: it will not make the call it
// was waiting for, so the next live caller would wait for nothing.
func TestRateLimiter_CancelledCallKeepsToken(t *testing.T) {
	r := middleware.NewRateLimiter(time.Hour, 1)
	calls := 0
	h := middleware.RateLimit(r)(func(context.Context, agent.Request) (agent.Message, agent.Usage, error) {
		calls++
		return agent.Message{}, agent.Usage{}, nil
	})
	done, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := h(done, agent.Request{}); err == nil {
		t.Fatal("a call with an ended context succeeded")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, _, err := h(ctx, agent.Request{}); err != nil {
		t.Fatalf("the live call after a cancelled one: %v (the cancelled call used up the token)", err)
	}
	if calls != 1 {
		t.Fatalf("next called %d times, want 1", calls)
	}
}
