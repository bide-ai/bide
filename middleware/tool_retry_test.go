package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	agent "github.com/dayna/go-agents"
)

func TestToolRetry_SucceedsAfterFlakes(t *testing.T) {
	const flakes = 3
	var calls int
	base := agent.ToolHandler(func(_ context.Context, _ agent.ToolUse) (json.RawMessage, error) {
		calls++
		if calls <= flakes {
			return nil, errors.New("flaky")
		}
		return json.RawMessage(`{}`), nil
	})
	h := ToolRetry(flakes, WithBackoff(1*time.Millisecond, 5*time.Millisecond))(base)
	res, err := h(context.Background(), agent.ToolUse{Name: "t"})
	if err != nil {
		t.Fatalf("err = %v, want nil after retries", err)
	}
	if res == nil {
		t.Fatal("result is nil, want non-nil")
	}
	if calls != flakes+1 {
		t.Fatalf("calls = %d, want %d", calls, flakes+1)
	}
}

func TestToolRetry_RateLimitedOnceSucceeds(t *testing.T) {
	var calls int
	rl := &agent.RateLimited{
		RetryAfter: 5 * time.Millisecond,
		Err:        errors.New("rate limited"),
	}
	base := agent.ToolHandler(func(_ context.Context, _ agent.ToolUse) (json.RawMessage, error) {
		calls++
		if calls == 1 {
			return nil, rl
		}
		return json.RawMessage(`{}`), nil
	})
	h := ToolRetry(2, WithBackoff(1*time.Millisecond, 20*time.Millisecond))(base)
	_, err := h(context.Background(), agent.ToolUse{Name: "t"})
	if err != nil {
		t.Fatalf("err = %v, want nil after rate-limited retry", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestToolRetry_ContextCanceledReturnsCtxErr(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var calls int
	base := agent.ToolHandler(func(_ context.Context, _ agent.ToolUse) (json.RawMessage, error) {
		calls++
		// Cancel after first call so we exercise the ctx-cancellation path.
		if calls == 1 {
			cancel()
		}
		return nil, errors.New("always fails")
	})

	h := ToolRetry(5, WithBackoff(10*time.Millisecond, 50*time.Millisecond))(base)
	_, err := h(ctx, agent.ToolUse{Name: "t"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestToolRetry_ExhaustedReturnsLastErr(t *testing.T) {
	sentinel := errors.New("always fails")
	var calls int
	base := agent.ToolHandler(func(_ context.Context, _ agent.ToolUse) (json.RawMessage, error) {
		calls++
		return nil, sentinel
	})
	h := ToolRetry(2, WithBackoff(1*time.Millisecond, 5*time.Millisecond))(base)
	_, err := h(context.Background(), agent.ToolUse{Name: "t"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel error", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3 (1 initial + 2 retries)", calls)
	}
}
