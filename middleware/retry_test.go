package middleware

import (
	"context"
	"errors"
	"testing"
	"time"

	agent "github.com/dayna/go-agents"
)

// fastBackoff keeps tests fast while still exercising the backoff path.
var fastBackoff = WithBackoff(1*time.Millisecond, 5*time.Millisecond)

func TestRetry_BackoffSucceedsAfterFlakes(t *testing.T) {
	var calls int
	base := agent.ModelHandler(func(context.Context, agent.Request) (agent.Message, agent.Usage, error) {
		calls++
		if calls < 3 {
			return agent.Message{}, agent.Usage{}, errors.New("flaky")
		}
		return agent.Message{}, agent.Usage{}, nil
	})
	_, _, err := Retry(3, fastBackoff)(base)(context.Background(), agent.Request{})
	if err != nil {
		t.Fatalf("err = %v, want nil after retries", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestRetry_RateLimitedOnceSucceeds(t *testing.T) {
	var calls int
	rl := &agent.RateLimited{
		RetryAfter: 5 * time.Millisecond,
		Err:        errors.New("rate limited"),
	}
	base := agent.ModelHandler(func(context.Context, agent.Request) (agent.Message, agent.Usage, error) {
		calls++
		if calls == 1 {
			return agent.Message{}, agent.Usage{}, rl
		}
		return agent.Message{}, agent.Usage{}, nil
	})
	_, _, err := Retry(2, fastBackoff)(base)(context.Background(), agent.Request{})
	if err != nil {
		t.Fatalf("err = %v, want nil after rate-limited retry", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestRetry_ContextCanceledMidRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var calls int
	base := agent.ModelHandler(func(context.Context, agent.Request) (agent.Message, agent.Usage, error) {
		calls++
		// Cancel after first call so we exercise the ctx-cancellation path.
		if calls == 1 {
			cancel()
		}
		return agent.Message{}, agent.Usage{}, errors.New("always fails")
	})

	// Use a non-trivial backoff so the select actually races ctx.Done().
	_, _, err := Retry(5, WithBackoff(10*time.Millisecond, 50*time.Millisecond))(base)(ctx, agent.Request{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
