package middleware

import (
	"context"
	"errors"
	"testing"

	agent "github.com/dayna/go-agents"
)

func TestRetry_SucceedsAfterFlakes(t *testing.T) {
	var calls int
	base := agent.ModelHandler(func(context.Context, agent.Request) (agent.Message, agent.Usage, error) {
		calls++
		if calls < 3 {
			return agent.Message{}, agent.Usage{}, errors.New("flaky")
		}
		return agent.Message{}, agent.Usage{}, nil
	})
	if _, _, err := Retry(3)(base)(context.Background(), agent.Request{}); err != nil {
		t.Fatalf("err = %v, want nil after retries", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestRetry_Exhausts(t *testing.T) {
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
