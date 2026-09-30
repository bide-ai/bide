package anthropic

import (
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

const (
	evToolStart = `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"refund"}}`
	evToolArgs  = `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}`
	evToolStop  = `{"type":"content_block_stop","index":1}`
	evToolDelta = `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}`
)

// Each delta belongs to a content block that message_start opened with content_block_start, and
// its type must be one that block carries. A delta that names no open block, or the wrong kind of
// block, would put text into a tool call's arguments, arguments into the answer, or make a call
// with no id or name; the stream is broken, so it is ErrStreamProtocol.
func TestStreamSSE_DeltasMustMatchTheirBlock(t *testing.T) {
	for name, events := range map[string][]string{
		"delta for a block never started": {evStart,
			`{"type":"content_block_delta","index":5,"delta":{"type":"input_json_delta","partial_json":"{}"}}`},
		"text delta on a tool block": {evStart, evToolStart,
			`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"hi"}}`},
		"args delta on a text block": {evStart, evTextStart,
			`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`},
		"thinking delta on a text block": {evStart, evTextStart,
			`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hm"}}`},
		"signature delta on a text block": {evStart, evTextStart,
			`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"s"}}`},
		"block started twice": {evStart, evToolStart,
			`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_2","name":"charge"}}`},
		"delta after its block stopped":  {evStart, evTextStart, evText, evTextStop, evText},
		"stop for a block never started": {evStart, `{"type":"content_block_stop","index":3}`},
	} {
		src := sse(append(events, evDelta, evStop)...)
		msg, _, err := testStream(src).Message()
		if !errors.Is(err, agent.ErrStreamProtocol) {
			t.Errorf("%s: got %+v, %v; want ErrStreamProtocol", name, msg.Parts, err)
		}
	}
}

// A block of a type the adapter does not handle (a server tool's call, say) is skipped whole: its
// argument deltas do not become a tool call with no id and no name.
func TestStreamSSE_UnhandledBlockDeltasAreIgnored(t *testing.T) {
	src := sse(evStart,
		`{"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"x\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Refund approved."}}`,
		`{"type":"content_block_stop","index":1}`,
		evDelta, evStop)
	msg, _, err := testStream(src).Message()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range msg.Parts {
		if tu, ok := p.(agent.ToolUse); ok {
			t.Fatalf("server tool's block became a tool call: %+v", tu)
		}
	}
	if msg.Text() != "Refund approved." {
		t.Fatalf("text = %q", msg.Text())
	}
}

// A well-formed turn with text, a tool call and thinking still assembles.
func TestStreamSSE_WellFormedBlocksAssemble(t *testing.T) {
	src := sse(evStart,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hm"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"s"}}`,
		`{"type":"content_block_stop","index":0}`,
		evToolStart, evToolArgs, evToolStop, evToolDelta, evStop)
	msg, _, err := testStream(src).Message()
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.Parts) != 2 {
		t.Fatalf("parts = %+v", msg.Parts)
	}
}
