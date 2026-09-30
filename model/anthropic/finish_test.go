package anthropic

import (
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// sse renders data payloads as an SSE body, one event per payload.
func sse(payloads ...string) string {
	var b strings.Builder
	for _, p := range payloads {
		b.WriteString("data: " + p + "\n\n")
	}
	return b.String()
}

const (
	evStart     = `{"type":"message_start","message":{"usage":{"input_tokens":10}}}`
	evTextStart = `{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`
	evText      = `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Refund approved."}}`
	evTextStop  = `{"type":"content_block_stop","index":0}`
	evDelta     = `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`
	evStop      = `{"type":"message_stop"}`
)

// Content after message_delta is past the turn's end: the stream fails with ErrStreamProtocol
// instead of adding text, reasoning, or a tool call the finished turn did not contain.
func TestStreamSSE_ContentAfterMessageDeltaIsAnError(t *testing.T) {
	for name, late := range map[string]string{
		"text delta":        `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" Denied."}}`,
		"tool block":        `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_9","name":"refund"}}`,
		"tool args":         `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		"thinking":          `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`,
		"redacted thinking": `{"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"x"}}`,
		"block stop":        `{"type":"content_block_stop","index":0}`,
		"message start":     evStart,
	} {
		msg, _, err := testStream(sse(evStart, evTextStart, evText, evTextStop, evDelta, late, evStop)).Message()
		if !errors.Is(err, agent.ErrStreamProtocol) {
			t.Errorf("%s after message_delta: got %q, %v; want ErrStreamProtocol", name, msg.Text(), err)
		}
	}
}

// The turn ends at message_stop. A stream cut after message_delta, before message_stop, did not
// finish the turn: it reads as ErrIncompleteResponse.
func TestStreamSSE_MessageDeltaWithoutStopIsIncomplete(t *testing.T) {
	msg, _, err := testStream(sse(evStart, evTextStart, evText, evTextStop, evDelta)).Message()
	if !errors.Is(err, agent.ErrIncompleteResponse) {
		t.Fatalf("got %q, %v; want ErrIncompleteResponse", msg.Text(), err)
	}
}

// message_stop with no message_delta before it carries no stop reason: the stream broke its
// protocol.
func TestStreamSSE_MessageStopWithoutDeltaIsAnError(t *testing.T) {
	msg, _, err := testStream(sse(evStart, evTextStart, evText, evTextStop, evStop)).Message()
	if !errors.Is(err, agent.ErrStreamProtocol) {
		t.Fatalf("got %q, %v; want ErrStreamProtocol", msg.Text(), err)
	}
}

// Anthropic may send more than one message_delta; its usage is cumulative. The turn gets one
// Finish, last, with the latest stop reason (a null one does not erase it) and output tokens. A
// ping or an event type this adapter does not know (Anthropic may add new ones) is allowed after
// message_delta, and anything after message_stop is not read.
func TestStreamSSE_OneFinishAtMessageStop(t *testing.T) {
	src := sse(evStart, evTextStart, evText, evTextStop,
		`{"type":"message_delta","delta":{"stop_reason":null},"usage":{"output_tokens":3}}`,
		`{"type":"ping"}`,
		`{"type":"some_future_event"}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		`{"type":"message_delta","delta":{"stop_reason":null},"usage":{"output_tokens":7}}`,
		evStop,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" Denied."}}`)
	var evs []agent.Event
	for ev, err := range testStream(src).Events() {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		evs = append(evs, ev)
	}
	want := agent.Finish{Reason: agent.FinishStop, Raw: "end_turn", Usage: agent.Usage{InputTokens: 10, OutputTokens: 7}}
	if len(evs) != 2 || evs[0] != (agent.TextDelta{Text: "Refund approved."}) || evs[1] != want {
		t.Fatalf("events = %+v, want the text then %+v", evs, want)
	}
}
