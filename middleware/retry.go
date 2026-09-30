package middleware

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/bide-ai/bide/agent"
)

const (
	defaultBackoffBase = 200 * time.Millisecond
	defaultBackoffMax  = 10 * time.Second
)

// RetryOption configures optional behavior of Retry.
type RetryOption func(*retryConfig)

type retryConfig struct {
	base    time.Duration
	max     time.Duration
	timeout time.Duration    // per-attempt deadline; 0 = none
	retryIf func(error) bool // nil = retry every error
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

// WithTimeout bounds each individual attempt with its own deadline: the call gets a
// context that is cancelled after d, so a hung model or tool call fails that attempt
// (and is retried) instead of blocking the run forever. The parent context still governs
// overall cancellation. 0 (default) means no per-attempt timeout.
func WithTimeout(d time.Duration) RetryOption {
	return func(c *retryConfig) { c.timeout = d }
}

// WithRetryIf classifies which errors are worth retrying: pred returns true to retry,
// false to fail fast. Use it to stop burning attempts (and tokens) on terminal errors
// such as HTTP 4xx auth/validation failures, while still retrying transient ones. The
// default (nil) retries every error. Context cancellation always stops the loop
// regardless. See Retryable for a ready-made classifier.
func WithRetryIf(pred func(error) bool) RetryOption {
	return func(c *retryConfig) { c.retryIf = pred }
}

// Retryable is a ready-made classifier for WithRetryIf: it retries transient failures and
// fails fast on terminal ones. It retries *agent.RateLimited, a per-attempt timeout
// (context.DeadlineExceeded), and *agent.APIError with a 5xx or 408 status. It does not retry
// parent cancellation (context.Canceled), 4xx API errors (auth, validation), or failures the
// same request would repeat: agent.ErrConfig (a request the adapter refused to build),
// agent.ErrQuotaExhausted (used-up quota or credit, which no wait lifts),
// agent.ErrResponseTooLarge, and agent.ErrTruncatedToolArgs (a tool call cut off by the
// response's output token limit, which the same request hits again). It does retry
// agent.ErrStreamProtocol (a stream that broke its provider's event protocol, a fault of that
// one response) and agent.ErrToolUseIDReused (a fresh generation can issue new ids). Errors it
// cannot classify (e.g. raw network errors, a provider's mid-stream server error) are retried,
// since those are usually transient.
//
//	agent.New(model, store, tools...).Use(middleware.Retry(3, middleware.WithRetryIf(middleware.Retryable)))
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, agent.ErrConfig) || errors.Is(err, agent.ErrQuotaExhausted) || errors.Is(err, agent.ErrResponseTooLarge) ||
		errors.Is(err, agent.ErrTruncatedToolArgs) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var rl *agent.RateLimited
	if errors.As(err, &rl) {
		return true
	}
	var ae *agent.APIError
	if errors.As(err, &ae) {
		return ae.StatusCode >= 500 || ae.StatusCode == 408
	}
	return true
}

// checkRetryCount refuses a negative retry count. The middleware constructors return no error, so
// the handler returns it on every call, before calling anything: a loop of n+1 attempts would
// otherwise run no attempt at all and return an empty success.
func checkRetryCount(name string, n int) error {
	if n < 0 {
		return fmt.Errorf("middleware.%s: retry count %d is negative; want 0 or more: %w", name, n, agent.ErrConfig)
	}
	return nil
}

// attempt runs fn under a per-attempt timeout if configured, returning its error.
func (cfg retryConfig) run(ctx context.Context, fn func(context.Context) error) error {
	if cfg.timeout <= 0 {
		return fn(ctx)
	}
	actx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()
	return fn(actx)
}

// Retry retries the model call up to n additional times on error, respecting
// context cancellation. Between attempts it sleeps with exponential backoff
// (full jitter, base×2^attempt, capped at max). When the error is
// *agent.RateLimited it sleeps for min(RetryAfter, max) instead.
//
// Default backoff: base=200ms, max=10s. Override with WithBackoff.
//
// n must be at least 0 (0 calls the model once). With n < 0 every call fails with an error
// wrapping agent.ErrConfig without calling the model (see checkRetryCount).
//
// Each attempt passes the call on unchanged, so every attempt's requests run the hooks middleware
// outside Retry added (a RateLimit token, Cost's spend) and are numbered by the turn's shared
// counter (agent.ModelCall.Attempt).
//
// Streaming: a streaming caller (Agent.Stream) sees each attempt's deltas live. When an attempt
// that streamed deltas fails and Retry calls the model again, the stream emits
// agent.TurnRestarted before the next attempt's deltas, so the caller can discard the failed
// attempt's partial text. Retry has no code for this: the agent's model handler does it.
func Retry(n int, opts ...RetryOption) agent.Middleware {
	cfg := retryConfig{base: defaultBackoffBase, max: defaultBackoffMax}
	for _, o := range opts {
		o(&cfg)
	}

	return func(next agent.ModelHandler) agent.ModelHandler {
		return func(ctx context.Context, call agent.ModelCall) (agent.ModelResponse, error) {
			if err := checkRetryCount("Retry", n); err != nil {
				return agent.ModelResponse{}, err
			}
			var (
				resp agent.ModelResponse
				err  error
			)
			for attempt := 0; attempt <= n; attempt++ {
				if ctx.Err() != nil {
					return resp, ctx.Err()
				}
				err = cfg.run(ctx, func(actx context.Context) error {
					resp, err = next(actx, call)
					return err
				})
				if err == nil {
					return resp, nil
				}
				// Fail fast on a terminal error or on parent cancellation.
				if cfg.retryIf != nil && !cfg.retryIf(err) {
					return resp, err
				}
				if ctx.Err() != nil {
					return resp, ctx.Err()
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
						return resp, ctx.Err()
					}
				}
			}
			return resp, err
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
