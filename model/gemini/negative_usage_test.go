package gemini

import (
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// A response reporting a negative token count is rejected, not counted.
func TestStreamSSE_NegativeUsageIsAnError(t *testing.T) {
	src := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":-4,\"candidatesTokenCount\":2}}\n\n"
	_, u, err := testStream(src).Message()
	if !errors.Is(err, agent.ErrProtocol) || !errors.Is(err, agent.ErrModel) {
		t.Fatalf("err = %v, want a model protocol error", err)
	}
	if u.InputTokens < 0 {
		t.Fatalf("usage = %+v, want no negative count", u)
	}
}
