package middleware

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/bide-ai/bide/agent"
)

// (The flaky-then-succeeds case lives in retry_test.go under synctest.)

func TestRetry_Exhausts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls int
		base := agent.ModelHandler(func(context.Context, agent.Request) (agent.Message, agent.Usage, error) {
			calls++
			return agent.Message{}, agent.Usage{}, errors.New("always")
		})
		if _, _, err := Retry(2)(base)(context.Background(), agent.Request{}); err == nil {
			t.Fatal("want error after exhausting retries")
		}
		if calls != 3 { // initial + 2 retries
			t.Fatalf("calls = %d, want 3", calls)
		}
	})
}
