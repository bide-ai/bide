package openai

import (
	"strings"
	"testing"

	"github.com/bide-ai/bide/model/modeltest"
)

// FuzzStreamSSE: any response body keeps modeltest.ReadSSE's properties (one Finish, last; errors
// last and an agent.ErrModel), and a body cut at any byte never succeeds with a message that
// differs from the whole body's success.
func FuzzStreamSSE(f *testing.F) {
	for i := range 8 {
		f.Add([]byte(sample), uint32(len(sample)*i/8))
	}
	// Usage on every chunk (vLLM continuous usage stats): cut after the first chunk, the body was
	// read as a complete answer.
	first := "data: {\"choices\":[{\"delta\":{\"content\":\"Your refund is appr\"}}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1}}\n\n"
	f.Add([]byte(first+"data: {\"choices\":[{\"delta\":{\"content\":\"oved.\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n"), uint32(len(first)))
	// Content after finish_reason: cut after the finish_reason chunk, the prefix is the whole answer.
	done := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"
	f.Add([]byte(done+"data: {\"choices\":[{\"delta\":{\"content\":\" more\"}}]}\n\n"), uint32(len(done)))
	for _, s := range []string{
		// Content after finish_reason, and a finish_reason followed by a usage-only chunk and no [DONE].
		"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\" more\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":1}}\n\n",
		"data: [DONE]\n", "data: {}\n\ndata: {}\n\n", "data: {\"error\":{\"message\":\"overloaded\"}}\n\n",
		// A line longer than the scanner's initial buffer (a line over agent.MaxSSELine, 32MB, is
		// too large for a seed; errors_test.go covers it).
		"data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("a", 80<<10) + "\"},\"finish_reason\":\"stop\"}]}\n",
	} {
		f.Add([]byte(s), uint32(len(s)/2))
	}
	f.Fuzz(func(t *testing.T, body []byte, cut uint32) {
		modeltest.CheckSSEPrefix(t, streamSSE, body, int(cut%uint32(len(body)+1)))
	})
}
