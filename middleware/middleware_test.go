package middleware

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	agent "github.com/blackwell-systems/bide"
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

func TestTokenBudget_AbortsWhenExceeded(t *testing.T) {
	base := agent.ModelHandler(func(context.Context, agent.Request) (agent.Message, agent.Usage, error) {
		return agent.Message{}, agent.Usage{InputTokens: 60}, nil
	})
	h := TokenBudget(100)(base)
	ctx := context.Background()

	if _, _, err := h(ctx, agent.Request{}); err != nil { // used 0 -> 60
		t.Fatal(err)
	}
	if _, _, err := h(ctx, agent.Request{}); err != nil { // used 60 -> 120
		t.Fatal(err)
	}
	if _, _, err := h(ctx, agent.Request{}); err == nil { // used 120 >= 100 -> refuse
		t.Fatal("want budget-exceeded error on third call")
	}
}
