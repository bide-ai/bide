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
		"max_tokens":                    {"length", agent.ErrOutputTruncated},
		"model_context_window_exceeded": {"length", agent.ErrOutputTruncated},
		"refusal":                       {"filtered", agent.ErrOutputFiltered},
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

// stop_reason tool_use with no tool_use block lost the calls it was for; tool_use with one is the
// call; pause_turn asks for the turn to be continued, so it is never the answer.
func TestStreamSSE_ToolUseReasonNeedsACall(t *testing.T) {
	if _, err := reasonOf(t, "tool_use"); !errors.Is(err, agent.ErrStreamProtocol) {
		t.Errorf("tool_use with only text: err %v, want ErrStreamProtocol", err)
	}
	src := sse(evStart,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"f"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}`, evStop)
	if msg, _, err := testStream(src).Message(); err != nil || len(msg.Parts) != 1 {
		t.Errorf("tool_use with a call: %+v, %v", msg, err)
	}
	if _, err := reasonOf(t, "pause_turn"); !errors.Is(err, agent.ErrStreamProtocol) {
		t.Errorf("pause_turn: err %v, want ErrStreamProtocol", err)
	}
}
