package openai

import (
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// The adapter asks for one completion, so every chunk's choice is index 0. A choice with another
// index is a second completion the server sent unasked; its text or calls are not part of the
// first, so the stream fails instead of interleaving them into one answer.
func TestStreamSSE_OtherChoiceIsAnError(t *testing.T) {
	for name, src := range map[string]string{
		"second choice in a chunk": "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Yes.\"}},{\"index\":1,\"delta\":{\"content\":\"No.\"}}]}\n\n" +
			"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
		"chunk for choice 1": "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Yes.\"}}]}\n\n" +
			"data: {\"choices\":[{\"index\":1,\"delta\":{\"content\":\"No.\"}}]}\n\n" +
			"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
	} {
		msg, _, err := testStream(src).Message()
		if !errors.Is(err, agent.ErrStreamProtocol) {
			t.Errorf("%s: got %q, %v; want ErrStreamProtocol", name, msg.Text(), err)
		}
	}
	// A server that leaves out the index sends choice 0.
	msg, _, err := testStream("data: {\"choices\":[{\"delta\":{\"content\":\"Yes.\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n").Message()
	if err != nil || msg.Text() != "Yes." {
		t.Fatalf("no index: got %q, %v", msg.Text(), err)
	}
}
