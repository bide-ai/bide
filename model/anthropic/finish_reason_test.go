package anthropic

import (
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// finishOf streams a text turn whose message_delta carries delta and returns the Finish the
// adapter reports and the error Message returns.
func finishOf(t *testing.T, delta string) (agent.Finish, error) {
	t.Helper()
	src := sse(evStart, evTextStart, evText, evTextStop,
		`{"type":"message_delta","delta":`+delta+`,"usage":{"output_tokens":7}}`, evStop)
	var fin agent.Finish
	for ev, err := range testStream(src).Events() {
		if err != nil {
			break
		}
		if f, ok := ev.(agent.Finish); ok {
			fin = f
		}
	}
	_, _, err := testStream(src).Message()
	return fin, err
}

// reasonOf is finishOf for a message_delta naming stopReason, returning the neutral reason.
func reasonOf(t *testing.T, stopReason string) (agent.FinishReason, error) {
	t.Helper()
	f, err := finishOf(t, `{"stop_reason":"`+stopReason+`"}`)
	return f.Reason, err
}

// Anthropic's stop reasons are mapped onto the neutral ones where they enter, so a cut-off or
// refused turn is never taken for a finished answer, and the Finish keeps the provider's own value
// in Raw. The reason is never empty: a message_delta naming no stop_reason is a natural stop.
func TestStreamSSE_StopReasonsAreMapped(t *testing.T) {
	for name, want := range map[string]struct {
		delta  string
		reason agent.FinishReason
		raw    string
		err    error
	}{
		"end_turn":                      {`{"stop_reason":"end_turn"}`, agent.FinishStop, "end_turn", nil},
		"stop_sequence":                 {`{"stop_reason":"stop_sequence"}`, agent.FinishStop, "stop_sequence", nil},
		"tool_use":                      {`{"stop_reason":"tool_use"}`, agent.FinishToolUse, "tool_use", agent.ErrStreamProtocol}, // no call arrived
		"max_tokens":                    {`{"stop_reason":"max_tokens"}`, agent.FinishLength, "max_tokens", agent.ErrOutputTruncated},
		"model_context_window_exceeded": {`{"stop_reason":"model_context_window_exceeded"}`, agent.FinishLength, "model_context_window_exceeded", agent.ErrOutputTruncated},
		"refusal":                       {`{"stop_reason":"refusal"}`, agent.FinishFiltered, "refusal", agent.ErrOutputFiltered},
		"pause_turn":                    {`{"stop_reason":"pause_turn"}`, "pause_turn", "pause_turn", agent.ErrStreamProtocol},
		"empty stop_reason":             {`{"stop_reason":""}`, agent.FinishStop, "", nil},
		"null stop_reason":              {`{"stop_reason":null}`, agent.FinishStop, "", nil},
		"no stop_reason":                {`{}`, agent.FinishStop, "", nil},
	} {
		f, err := finishOf(t, want.delta)
		if f.Reason != want.reason || f.Raw != want.raw {
			t.Errorf("%s: Finish reason %q raw %q, want %q and %q", name, f.Reason, f.Raw, want.reason, want.raw)
		}
		if f.Discarded != (agent.Usage{}) {
			t.Errorf("%s: Finish.Discarded = %+v, want zero from a live adapter", name, f.Discarded)
		}
		if want.err == nil && err != nil || want.err != nil && !errors.Is(err, want.err) {
			t.Errorf("%s: err = %v, want %v", name, err, want.err)
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
