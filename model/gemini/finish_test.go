package gemini

import (
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// A finishReason ends the turn. Content in a later chunk is not more of the answer: the stream
// fails with ErrStreamProtocol instead of returning text or a tool call the finished turn did not
// contain. A later chunk that only reports usage is allowed.
func TestStreamSSE_ContentAfterFinishReasonIsAnError(t *testing.T) {
	first := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Refund approved.\"}]},\"finishReason\":\"STOP\"}]}\n\n"
	for name, late := range map[string]string{
		"text":       `{"candidates":[{"content":{"parts":[{"text":" Denied."}]}}]}`,
		"thought":    `{"candidates":[{"content":{"parts":[{"text":"hmm","thought":true}]}}]}`,
		"tool call":  `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"refund","args":{}}}]}}]}`,
		"new reason": `{"candidates":[{"content":{"parts":[]},"finishReason":"MAX_TOKENS"}]}`,
	} {
		msg, _, err := testStream(first + "data: " + late + "\n\n").Message()
		if !errors.Is(err, agent.ErrStreamProtocol) {
			t.Errorf("%s after finishReason: got %q, %v; want ErrStreamProtocol", name, msg.Text(), err)
		}
	}
	repeat := "data: {\"candidates\":[{\"content\":{\"parts\":[]},\"finishReason\":\"STOP\"}]}\n\n"
	msg, usage, err := testStream(first + repeat + "data: {\"usageMetadata\":{\"promptTokenCount\":4,\"candidatesTokenCount\":2}}\n\n").Message()
	if err != nil || msg.Text() != "Refund approved." || usage.OutputTokens != 2 {
		t.Errorf("usage after finishReason: got %q, %+v, %v", msg.Text(), usage, err)
	}
}
