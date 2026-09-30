package middleware_test

import (
	"context"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/middleware"
)

// S2: a retried streaming turn: one TurnRestarted, and the consumer's text matches the journal.
func TestS2_RetryStreamRestart(t *testing.T) {
	m := &flakyModel{name: "p", fail: 1}
	a := agent.New(m, agent.NewMemStore()).Use(middleware.Retry(2, middleware.WithBackoff(0, 0)))
	if n := streamCheck(t, a.Stream(context.Background(), "r", "q")); n != 1 {
		t.Fatalf("restarts %d, want 1", n)
	}
}

// S3: a guard middleware rejects the first (streamed, successful) response; Retry re-sends. The
// consumer must not end up with text that differs from the journal.
func TestS3_GuardRejectsStreamedResponse(t *testing.T) {
	m := &flakyModel{name: "p"}
	first := true
	guard := func(next agent.ModelHandler) agent.ModelHandler {
		return func(ctx context.Context, call agent.ModelCall) (agent.ModelResponse, error) {
			r, err := next(ctx, call)
			if err == nil && first {
				first = false
				return agent.ModelResponse{}, context.DeadlineExceeded
			}
			return r, err
		}
	}
	a := agent.New(m, agent.NewMemStore()).Use(middleware.Retry(2, middleware.WithBackoff(0, 0)), guard)
	if n := streamCheck(t, a.Stream(context.Background(), "r", "q")); n != 1 {
		t.Fatalf("restarts %d, want 1", n)
	}
}

