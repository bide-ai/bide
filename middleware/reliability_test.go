package middleware_test

import (
	"context"
	"errors"
	"testing"
	"time"

	agent "github.com/blackwell-systems/bide"
	"github.com/blackwell-systems/bide/middleware"
)

// TestRetry_PerAttemptTimeout: a handler that blocks past the per-attempt timeout fails that
// attempt (context.DeadlineExceeded) and is retried; a fast success on a later attempt wins.
func TestRetry_PerAttemptTimeout(t *testing.T) {
	calls := 0
	h := func(ctx context.Context, req agent.Request) (agent.Message, agent.Usage, error) {
		calls++
		if calls == 1 {
			<-ctx.Done() // exceed the per-attempt timeout
			return agent.Message{}, agent.Usage{}, ctx.Err()
		}
		return agent.Message{}, agent.Usage{}, nil
	}
	wrapped := middleware.Retry(2, middleware.WithTimeout(20*time.Millisecond), middleware.WithBackoff(time.Millisecond, time.Millisecond))(h)
	if _, _, err := wrapped(context.Background(), agent.Request{}); err != nil {
		t.Fatalf("expected success after a timed-out first attempt, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 attempts, got %d", calls)
	}
}

// TestRetry_FailFastOnTerminal: WithRetryIf(Retryable) does not retry a terminal 4xx APIError.
func TestRetry_FailFastOnTerminal(t *testing.T) {
	calls := 0
	terminal := &agent.APIError{StatusCode: 400, Err: agent.ErrModel}
	h := func(ctx context.Context, req agent.Request) (agent.Message, agent.Usage, error) {
		calls++
		return agent.Message{}, agent.Usage{}, terminal
	}
	wrapped := middleware.Retry(5, middleware.WithRetryIf(middleware.Retryable))(h)
	_, _, err := wrapped(context.Background(), agent.Request{})
	if !errors.Is(err, agent.ErrModel) {
		t.Fatalf("expected the terminal error back, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("terminal 4xx should not be retried; got %d calls", calls)
	}
}

// TestRetry_RetriesTransient: a 503 APIError is retried and can then succeed.
func TestRetry_RetriesTransient(t *testing.T) {
	calls := 0
	h := func(ctx context.Context, req agent.Request) (agent.Message, agent.Usage, error) {
		calls++
		if calls < 3 {
			return agent.Message{}, agent.Usage{}, &agent.APIError{StatusCode: 503, Err: agent.ErrModel}
		}
		return agent.Message{}, agent.Usage{}, nil
	}
	wrapped := middleware.Retry(5, middleware.WithRetryIf(middleware.Retryable), middleware.WithBackoff(time.Millisecond, time.Millisecond))(h)
	if _, _, err := wrapped(context.Background(), agent.Request{}); err != nil {
		t.Fatalf("expected eventual success on transient 5xx, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 attempts, got %d", calls)
	}
}

// TestRateLimiter_Throttles: with a limiter of 1 token per 30ms and burst 1, three calls take at
// least ~60ms (two waits), and cancellation is respected.
func TestRateLimiter_Throttles(t *testing.T) {
	r := middleware.NewRateLimiter(30*time.Millisecond, 1)
	calls := 0
	h := func(ctx context.Context, req agent.Request) (agent.Message, agent.Usage, error) {
		calls++
		return agent.Message{}, agent.Usage{}, nil
	}
	wrapped := middleware.RateLimit(r)(h)
	start := time.Now()
	for i := 0; i < 3; i++ {
		if _, _, err := wrapped(context.Background(), agent.Request{}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("expected throttling to take >=50ms for 3 calls at 1/30ms, took %s", elapsed)
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls, got %d", calls)
	}

	// Cancellation while waiting returns promptly with the context error.
	drain := middleware.NewRateLimiter(time.Hour, 1)
	_, _, _ = middleware.RateLimit(drain)(h)(context.Background(), agent.Request{}) // consume the one token
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, _, err := middleware.RateLimit(drain)(h)(ctx, agent.Request{}); err == nil {
		t.Fatal("expected context error while waiting on an exhausted limiter")
	}
}
