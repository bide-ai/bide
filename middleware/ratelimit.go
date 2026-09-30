package middleware

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/bide-ai/bide/agent"
)

// RateLimiter is a token-bucket limiter shared across calls: it allows one token every
// `interval` with a burst up to `burst`. It is dependency-free (no background goroutine): tokens
// accrue lazily from elapsed time. Safe for concurrent use. Share one limiter across model and
// tool middleware to cap a whole agent, or use separate limiters per surface. Waiters are not
// served in arrival order: a call arriving as a token frees can take it ahead of one already
// waiting.
type RateLimiter struct {
	mu       sync.Mutex
	interval time.Duration
	burst    float64
	tokens   float64
	last     time.Time
}

// NewRateLimiter allows one call every `interval` (so rate = 1/interval), bursting up to `burst`
// calls. A burst < 1 is treated as 1. Starts full so the first `burst` calls proceed immediately.
// An interval <= 0 sets no limit: every call proceeds at once.
func NewRateLimiter(interval time.Duration, burst int) *RateLimiter {
	if burst < 1 {
		burst = 1
	}
	return &RateLimiter{interval: interval, burst: float64(burst), tokens: float64(burst)}
}

// wait blocks until a token is available or ctx is done, then consumes one token. A call whose
// ctx has ended takes no token, since it will not make the call the token is for.
func (r *RateLimiter) wait(ctx context.Context) error {
	if r.interval <= 0 {
		return ctx.Err() // no limit; a zero interval would otherwise never refill the bucket
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		r.mu.Lock()
		now := time.Now()
		if r.last.IsZero() {
			r.last = now
		}
		r.tokens += float64(now.Sub(r.last)) / float64(r.interval)
		if r.tokens > r.burst {
			r.tokens = r.burst
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

// RateLimit throttles model requests through the shared limiter, blocking (respecting context
// cancellation) until a token is available. Proactive throttling complements Retry's reactive
// Retry-After backoff: it keeps you under the provider's ceiling instead of bouncing off it.
//
// It takes a token for every request the call sends, wherever it sits in the chain: each attempt
// of a Retry below it and each target a Hedge below it launches waits for its own token, through
// a hook it adds to the call (see agent.ModelCallHook). Outside an agent, call the model through
// agent.CallModel, whose model handler runs the hooks too.
func RateLimit(r *RateLimiter) agent.Middleware {
	hook := agent.ModelCallHook{Before: func(ctx context.Context, _ agent.ModelCall) error { return r.wait(ctx) }}
	return func(next agent.ModelHandler) agent.ModelHandler {
		return func(ctx context.Context, call agent.ModelCall) (agent.ModelResponse, error) {
			return next(ctx, call.AddHook(hook))
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
