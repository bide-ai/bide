package anthropic

import (
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// reasonOf streams a text turn ending in stop_reason and returns the Finish reason the adapter
// reports and the error Message returns.
func reasonOf(t *testing.T, stopReason string) (string, error) {
	t.Helper()
	src := sse(evStart, evTextStart, evText, evTextStop,
		`{"type":"message_delta","delta":{"stop_reason":"`+stopReason+`"},"usage":{"output_tokens":7}}`, evStop)
	var reason string
	for ev, err := range testStream(src).Events() {
		if err != nil {
			break
		}
		if f, ok := ev.(agent.Finish); ok {
			reason = f.Reason
		}
	}
	_, _, err := testStream(src).Message()
	return reason, err
}

// Anthropic's stop reasons are mapped onto the neutral ones where they enter, so a cut-off or
// refused turn is never taken for a finished answer.
func TestStreamSSE_StopReasonsAreMapped(t *testing.T) {
	for stop, want := range map[string]struct {
		reason string
		err    error
	}{
		"end_turn":                      {"stop", nil},
		"stop_sequence":                 {"stop", nil},
		"max_tokens":                    {"length", agent.ErrModel},
		"model_context_window_exceeded": {"length", agent.ErrModel},
		"refusal":                       {"filtered", agent.ErrModel},
		"pause_turn":                    {"pause_turn", agent.ErrStreamProtocol},
	} {
		reason, err := reasonOf(t, stop)
		if reason != want.reason {
			t.Errorf("%s: Finish.Reason = %q, want %q", stop, reason, want.reason)
		}
		if want.err == nil && err != nil || want.err != nil && !errors.Is(err, want.err) {
			t.Errorf("%s: err = %v, want %v", stop, err, want.err)
		}
	}
}
