package middleware

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	agent "github.com/dayna/go-agents"
)

// RateLimiter is a token-bucket limiter shared across calls: it allows one token every
// `interval` with a burst up to `burst`. It is dependency-free (no background goroutine): tokens
// accrue lazily from elapsed time. Safe for concurrent use. Share one limiter across model and
// tool middleware to cap a whole agent, or use separate limiters per surface.
type RateLimiter struct {
	mu       sync.Mutex
	interval time.Duration
	burst    float64
	tokens   float64
	last     time.Time
}

// NewRateLimiter allows one call every `interval` (so rate = 1/interval), bursting up to `burst`
// calls. A burst < 1 is treated as 1. Starts full so the first `burst` calls proceed immediately.
func NewRateLimiter(interval time.Duration, burst int) *RateLimiter {
	if burst < 1 {
		burst = 1
	}
	return &RateLimiter{interval: interval, burst: float64(burst), tokens: float64(burst)}
}

// wait blocks until a token is available or ctx is done, then consumes one token.
func (r *RateLimiter) wait(ctx context.Context) error {
	for {
		r.mu.Lock()
		now := time.Now()
		if r.last.IsZero() {
			r.last = now
		}
		if r.interval > 0 {
			r.tokens += float64(now.Sub(r.last)) / float64(r.interval)
			if r.tokens > r.burst {
				r.tokens = r.burst
			}
		}
		r.last = now
		if r.tokens >= 1 {
			r.tokens--
			r.mu.Unlock()
			return nil
		}
		wait := time.Duration((1 - r.tokens) * float64(r.interval))
		r.mu.Unlock()
		if wait <= 0 {
			wait = time.Millisecond
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// RateLimit throttles model calls through the shared limiter, blocking (respecting context
// cancellation) until a token is available. Proactive throttling complements Retry's reactive
// Retry-After backoff: it keeps you under the provider's ceiling instead of bouncing off it.
func RateLimit(r *RateLimiter) agent.Middleware {
	return func(next agent.ModelHandler) agent.ModelHandler {
		return func(ctx context.Context, req agent.Request) (agent.Message, agent.Usage, error) {
			if err := r.wait(ctx); err != nil {
				return agent.Message{}, agent.Usage{}, err
			}
			return next(ctx, req)
		}
	}
}

// ToolRateLimit throttles tool calls through the shared limiter.
func ToolRateLimit(r *RateLimiter) agent.ToolMiddleware {
	return func(next agent.ToolHandler) agent.ToolHandler {
		return func(ctx context.Context, tu agent.ToolUse) (json.RawMessage, error) {
			if err := r.wait(ctx); err != nil {
				return nil, err
			}
			return next(ctx, tu)
		}
	}
}
