package anthropic

import (
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/middleware"
)

// A line over the scanner's cap fails the same way on every attempt: a non-retryable ErrModel.
func TestStreamSSE_OverlongLineIsNotRetried(t *testing.T) {
	_, _, err := testStream("data: " + strings.Repeat("x", agent.MaxSSELine) + "\n\n").Message()
	if !errors.Is(err, agent.ErrResponseTooLarge) || middleware.Retryable(err) {
		t.Fatalf("err = %v, want a non-retryable ErrResponseTooLarge", err)
	}
}

// Anthropic reports a failure partway through a stream as an "error" event. It is classified
// by its type, carries the provider's message, and does not paste the raw event into the error
// text, however large it is.
func TestStreamSSE_ErrorEvent(t *testing.T) {
	_, _, err := testStream("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n").Message()
	if !errors.Is(err, agent.ErrModel) || !strings.Contains(err.Error(), "Overloaded") || !middleware.Retryable(err) {
		t.Fatalf("overloaded: err = %v, want a retryable ErrModel carrying the message", err)
	}
	_, _, err = testStream("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"slow down\"}}\n\n").Message()
	var rl *agent.RateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("rate_limit_error: err = %T %v, want *RateLimited", err, err)
	}
	huge := strings.Repeat("x", 4<<20)
	_, _, err = testStream("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"boom\"},\"pad\":\"" + huge + "\"}\n\n").Message()
	if err == nil || len(err.Error()) > 64<<10 || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error text is %d bytes, want it bounded and carrying the message", len(err.Error()))
	}
}
