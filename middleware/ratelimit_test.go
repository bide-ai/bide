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
