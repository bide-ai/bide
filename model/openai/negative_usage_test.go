package openai

import (
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// A response reporting a negative token count is rejected, not counted.
func TestStreamSSE_NegativeUsageIsAnError(t *testing.T) {
	src := `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":-5,"completion_tokens":3}}

data: [DONE]

`
	_, u, err := testStream(src).Message()
	if !errors.Is(err, agent.ErrProtocol) || !errors.Is(err, agent.ErrModel) {
		t.Fatalf("err = %v, want a model protocol error", err)
	}
	if u.InputTokens < 0 {
		t.Fatalf("usage = %+v, want no negative count", u)
	}
}
