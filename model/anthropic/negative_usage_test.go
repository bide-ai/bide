package anthropic

import (
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// A response reporting a negative token count is rejected, not counted.
func TestStreamSSE_NegativeUsageIsAnError(t *testing.T) {
	start := `{"type":"message_start","message":{"usage":{"input_tokens":-10}}}`
	_, u, err := testStream(sse(start, evTextStart, evText, evTextStop, evDelta, evStop)).Message()
	if !errors.Is(err, agent.ErrProtocol) || !errors.Is(err, agent.ErrModel) {
		t.Fatalf("err = %v, want a model protocol error", err)
	}
	if u.InputTokens < 0 {
		t.Fatalf("usage = %+v, want no negative count", u)
	}
}
