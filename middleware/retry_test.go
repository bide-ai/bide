package middleware

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/bide-ai/bide/agent"
)

// These run inside synctest bubbles: the backoff sleeps use a fake clock, so realistic
// durations cost zero wall-clock and the tests are deterministic (no real-timer flake).

func TestRetry_BackoffSucceedsAfterFlakes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls int
		base := agent.ModelHandler(func(context.Context, agent.Request) (agent.Message, agent.Usage, error) {
			calls++
			if calls < 3 {
				return agent.Message{}, agent.Usage{}, errors.New("flaky")
			}
			return agent.Message{}, agent.Usage{}, nil
		})
		_, _, err := Retry(3)(base)(context.Background(), agent.Request{}) // default backoff
		if err != nil {
			t.Fatalf("err = %v, want nil after retries", err)
		}
		if calls != 3 {
			t.Fatalf("calls = %d, want 3", calls)
		}
	})
}

func TestRetry_RateLimitedOnceSucceeds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls int
		rl := &agent.RateLimited{RetryAfter: 2 * time.Second, Err: errors.New("rate limited")}
		base := agent.ModelHandler(func(context.Context, agent.Request) (agent.Message, agent.Usage, error) {
			calls++
			if calls == 1 {
				return agent.Message{}, agent.Usage{}, rl
			}
			return agent.Message{}, agent.Usage{}, nil
		})
		_, _, err := Retry(2)(base)(context.Background(), agent.Request{})
		if err != nil {
			t.Fatalf("err = %v, want nil after rate-limited retry", err)
		}
		if calls != 2 {
			t.Fatalf("calls = %d, want 2", calls)
		}
	})
}

func TestRetry_ContextCanceledMidRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var calls int
		base := agent.ModelHandler(func(context.Context, agent.Request) (agent.Message, agent.Usage, error) {
			calls++
			if calls == 1 {
				cancel() // exercise the ctx-cancellation path mid-backoff
			}
			return agent.Message{}, agent.Usage{}, errors.New("always fails")
		})
		_, _, err := Retry(5)(base)(ctx, agent.Request{})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
}
