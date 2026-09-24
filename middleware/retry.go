package middleware

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	agent "github.com/dayna/go-agents"
)

const (
	defaultBackoffBase = 200 * time.Millisecond
	defaultBackoffMax  = 10 * time.Second
)

// RetryOption configures optional behavior of Retry.
type RetryOption func(*retryConfig)

type retryConfig struct {
	base time.Duration
	max  time.Duration
}

// WithBackoff sets the initial backoff duration and the maximum cap. Between
// retries the sleep doubles each attempt (full jitter applied) and is capped at
// max. If the error is *agent.RateLimited the Retry-After hint overrides the
// computed backoff (capped at max).
func WithBackoff(base, max time.Duration) RetryOption {
	return func(c *retryConfig) {
		c.base = base
		c.max = max
	}
}

// Retry retries the model call up to n additional times on error, respecting
// context cancellation. Between attempts it sleeps with exponential backoff
// (full jitter, base×2^attempt, capped at max). When the error is
// *agent.RateLimited it sleeps for min(RetryAfter, max) instead.
//
// Default backoff: base=200ms, max=10s. Override with WithBackoff.
func Retry(n int, opts ...RetryOption) agent.Middleware {
	cfg := retryConfig{base: defaultBackoffBase, max: defaultBackoffMax}
	for _, o := range opts {
		o(&cfg)
	}

	return func(next agent.ModelHandler) agent.ModelHandler {
		return func(ctx context.Context, req agent.Request) (agent.Message, agent.Usage, error) {
			var (
				msg agent.Message
				u   agent.Usage
				err error
			)
			for attempt := 0; attempt <= n; attempt++ {
				if ctx.Err() != nil {
					return msg, u, ctx.Err()
				}
				msg, u, err = next(ctx, req)
				if err == nil {
					return msg, u, nil
				}
				if attempt == n {
					break
				}

				// Compute how long to sleep before the next attempt.
				d := sleepDuration(err, attempt, cfg)
				if d > 0 {
					select {
					case <-time.After(d):
					case <-ctx.Done():
						return msg, u, ctx.Err()
					}
				}
			}
			return msg, u, err
		}
	}
}

// sleepDuration returns the duration to sleep before attempt+1. If err is
// *agent.RateLimited the Retry-After hint wins (capped at cfg.max). Otherwise
// full-jitter exponential backoff is used.
func sleepDuration(err error, attempt int, cfg retryConfig) time.Duration {
	var rl *agent.RateLimited
	if errors.As(err, &rl) && rl.RetryAfter > 0 {
		return min(rl.RetryAfter, cfg.max)
	}

	// Exponential backoff: base * 2^attempt, capped at max.
	back := cfg.base
	for range attempt {
		back = min(back*2, cfg.max)
	}
	if back <= 0 {
		return 0
	}
	// Full jitter: uniform in [0, back).
	return time.Duration(rand.Int64N(int64(back)))
}
