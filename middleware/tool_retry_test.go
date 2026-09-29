package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/bide-ai/bide/agent"
)

// Run inside synctest bubbles: backoff sleeps use a fake clock (deterministic, instant).

func TestToolRetry_SucceedsAfterFlakes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const flakes = 3
		var calls int
		base := agent.ToolHandler(func(_ context.Context, _ agent.ToolUse) (json.RawMessage, error) {
			calls++
			if calls <= flakes {
				return nil, errors.New("flaky")
			}
			return json.RawMessage(`{}`), nil
		})
		res, err := ToolRetry(flakes)(base)(idempotent(context.Background()), agent.ToolUse{Name: "t"})
		if err != nil {
			t.Fatalf("err = %v, want nil after retries", err)
		}
		if res == nil {
			t.Fatal("result is nil, want non-nil")
		}
		if calls != flakes+1 {
			t.Fatalf("calls = %d, want %d", calls, flakes+1)
		}
	})
}

func TestToolRetry_RateLimitedOnceSucceeds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls int
		rl := &agent.RateLimited{RetryAfter: 2 * time.Second, Err: errors.New("rate limited")}
		base := agent.ToolHandler(func(_ context.Context, _ agent.ToolUse) (json.RawMessage, error) {
			calls++
			if calls == 1 {
				return nil, rl
			}
			return json.RawMessage(`{}`), nil
		})
		if _, err := ToolRetry(2)(base)(idempotent(context.Background()), agent.ToolUse{Name: "t"}); err != nil {
			t.Fatalf("err = %v, want nil after rate-limited retry", err)
		}
		if calls != 2 {
			t.Fatalf("calls = %d, want 2", calls)
		}
	})
}

func TestToolRetry_ContextCanceledReturnsCtxErr(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(idempotent(context.Background()))
		var calls int
		base := agent.ToolHandler(func(_ context.Context, _ agent.ToolUse) (json.RawMessage, error) {
			calls++
			if calls == 1 {
				cancel() // exercise the ctx-cancellation path mid-backoff
			}
			return nil, errors.New("always fails")
		})
		if _, err := ToolRetry(5)(base)(ctx, agent.ToolUse{Name: "t"}); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
}

func TestToolRetry_ExhaustedReturnsLastErr(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sentinel := errors.New("always fails")
		var calls int
		base := agent.ToolHandler(func(_ context.Context, _ agent.ToolUse) (json.RawMessage, error) {
			calls++
			return nil, sentinel
		})
		_, err := ToolRetry(2)(base)(idempotent(context.Background()), agent.ToolUse{Name: "t"})
		if !errors.Is(err, sentinel) {
			t.Fatalf("err = %v, want sentinel error", err)
		}
		if calls != 3 {
			t.Fatalf("calls = %d, want 3 (1 initial + 2 retries)", calls)
		}
	})
}

// idempotent marks ctx as a call to a retry-safe tool, as the agent does for a tool declared
// Idempotent: ToolRetry retries only such tools.
func idempotent(ctx context.Context) context.Context {
	return agent.WithToolSafety(ctx, agent.Safety{Idempotent: true})
}
