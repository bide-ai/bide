package gemini

import (
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// A finishReason is provider output of any length. The protocol errors that quote one must not
// carry it in full.
func TestStreamSSE_FinishReasonErrorsAreBounded(t *testing.T) {
	huge := strings.Repeat("R", 1<<20)
	for name, src := range map[string]string{
		"content after": "data: {\"candidates\":[{\"content\":{\"parts\":[]},\"finishReason\":\"" + huge + "\"}]}\n\n" +
			"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"more\"}]}}]}\n\n",
		"huge reason, then another": "data: {\"candidates\":[{\"content\":{\"parts\":[]},\"finishReason\":\"" + huge + "\"}]}\n\n" +
			"data: {\"candidates\":[{\"content\":{\"parts\":[]},\"finishReason\":\"STOP\"}]}\n\n",
		"reason after": "data: {\"candidates\":[{\"content\":{\"parts\":[]},\"finishReason\":\"STOP\"}]}\n\n" +
			"data: {\"candidates\":[{\"content\":{\"parts\":[]},\"finishReason\":\"" + huge + "\"}]}\n\n",
	} {
		_, _, err := testStream(src).Message()
		if !errors.Is(err, agent.ErrStreamProtocol) {
			t.Errorf("%s: err = %v, want ErrStreamProtocol", name, err)
			continue
		}
		if n := len(err.Error()); n > 1024 {
			t.Errorf("%s: error text is %d bytes, want at most 1024", name, n)
		}
	}
}
