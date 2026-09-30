package openai

import (
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// A server that leaves out tool_calls[].index puts every call at index 0. The second call's new
// id is not more of the first call: the stream fails instead of merging them into one call.
func TestStreamSSE_CallsWithoutIndexAreNotMerged(t *testing.T) {
	src := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"a\",\"function\":{\"name\":\"x\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"b\",\"function\":{\"name\":\"y\",\"arguments\":\"{\\\"q\\\":2}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	msg, _, err := testStream(src).Message()
	if !errors.Is(err, agent.ErrStreamProtocol) {
		t.Fatalf("got %+v, %v; want ErrStreamProtocol", msg.Parts, err)
	}
}
