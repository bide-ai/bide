package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// A negative retry count is a configuration error on every call, and nothing is called: it used
// to skip the loop and return an empty success, an empty model answer or a tool result of nothing.
func TestRetry_NegativeCountIsAConfigError(t *testing.T) {
	for _, n := range []int{-1, -5} {
		calls := 0
		h := Retry(n)(func(context.Context, agent.ModelCall) (agent.ModelResponse, error) {
			calls++
			return agent.ModelResponse{Message: agent.Message{Role: agent.RoleAssistant}}, nil
		})
		if _, err := h(context.Background(), agent.ModelCall{}); !errors.Is(err, agent.ErrConfig) || calls != 0 {
			t.Errorf("Retry(%d): err %v, model called %d times; want ErrConfig and no call", n, err, calls)
		}
	}
	calls := 0
	h := Retry(0)(func(context.Context, agent.ModelCall) (agent.ModelResponse, error) {
		calls++
		return agent.ModelResponse{}, nil
	})
	if _, err := h(context.Background(), agent.ModelCall{}); err != nil || calls != 1 {
		t.Errorf("Retry(0): err %v, %d calls; want one call", err, calls)
	}
}

func TestToolRetry_NegativeCountIsAConfigError(t *testing.T) {
	for _, safety := range []agent.Safety{{ReadOnly: true}, {}} {
		for _, n := range []int{-1, -5} {
			calls := 0
			h := ToolRetry(n)(func(context.Context, agent.ToolUse) (json.RawMessage, error) {
				calls++
				return json.RawMessage(`"ok"`), nil
			})
			ctx := agent.WithToolSafety(context.Background(), safety)
			if _, err := h(ctx, agent.ToolUse{ID: "c", Name: "t"}); !errors.Is(err, agent.ErrConfig) || calls != 0 {
				t.Errorf("ToolRetry(%d), safety %+v: err %v, tool called %d times; want ErrConfig and no call", n, safety, err, calls)
			}
		}
	}
	calls := 0
	h := ToolRetry(0)(func(context.Context, agent.ToolUse) (json.RawMessage, error) {
		calls++
		return json.RawMessage(`"ok"`), nil
	})
	if _, err := h(agent.WithToolSafety(context.Background(), agent.Safety{ReadOnly: true}), agent.ToolUse{}); err != nil || calls != 1 {
		t.Errorf("ToolRetry(0): err %v, %d calls; want one call", err, calls)
	}
}
