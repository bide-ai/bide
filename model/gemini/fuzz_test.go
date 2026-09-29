package gemini

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/model/modeltest"
)

// streamSSEStableIDs is streamSSE with the ids it makes up for calls Gemini sent without one
// (newCallID, random by design) replaced by a fixed one, so two reads of a body compare equal.
func streamSSEStableIDs(body io.ReadCloser, send func(agent.Emit) bool) {
	data, _ := io.ReadAll(body)
	streamSSE(io.NopCloser(bytes.NewReader(data)), func(e agent.Emit) bool {
		if d, ok := e.Event.(agent.ToolCallDelta); ok && !bytes.Contains(data, []byte(d.ID)) {
			d.ID = "generated"
			e.Event = d
		}
		return send(e)
	})
}

// FuzzStreamSSE: any response body keeps modeltest.ReadSSE's properties (one Finish, last; errors
// last and an agent.ErrModel), and a body cut at any byte never succeeds with a message that
// differs from the whole body's success (a made-up call id aside).
func FuzzStreamSSE(f *testing.F) {
	for i := range 8 {
		f.Add([]byte(sample), uint32(len(sample)*i/8))
	}
	// Content after finishReason was added to the finished turn's answer.
	first := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Refund approved.\"}]},\"finishReason\":\"STOP\"}]}\n\n"
	f.Add([]byte(first+"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\" Denied.\"}]}}]}\n\n"), uint32(len(first)))
	for _, s := range []string{
		first + "data: {\"candidates\":[{\"content\":{\"parts\":[]},\"finishReason\":\"MAX_TOKENS\"}]}\n\n",
		first + "data: {\"usageMetadata\":{\"promptTokenCount\":4,\"candidatesTokenCount\":2}}\n\n",
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"f\"}}]},\"finishReason\":\"STOP\"}]}\n\n",
		// A call with no id in both the whole body and its prefix (each read makes one up).
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"f\"}}]}}]}\n\ndata: {\"candidates\":[{\"finishReason\":\"STOP\"}]}\n\n",
		"data: {\"error\":{\"code\":503,\"message\":\"busy\"}}\n\n", "data: {}\n\n", "data: [\n\n",
		// A line longer than the scanner's initial buffer (a line over agent.MaxSSELine, 32MB, is
		// too large for a seed; the agent package covers it).
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"" + strings.Repeat("a", 80<<10) + "\"}]},\"finishReason\":\"STOP\"}]}\n\n",
	} {
		f.Add([]byte(s), uint32(len(s)/2))
	}
	f.Fuzz(func(t *testing.T, body []byte, cut uint32) {
		modeltest.CheckSSEPrefix(t, streamSSEStableIDs, body, int(cut%uint32(len(body)+1)))
	})
}
