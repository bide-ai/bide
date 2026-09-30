package anthropic

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
	// Content after message_delta was added to the finished turn's answer.
	late := sse(evStart, evTextStart, evText, evTextStop, evDelta,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" Denied."}}`, evStop)
	f.Add([]byte(late), uint32(strings.Index(late, " Denied.")))
	for _, s := range []string{
		sse(evStart, evTextStart, evText, evTextStop, evDelta), // no message_stop
		sse(evStart, evTextStart, evText, evTextStop, evStop),  // message_stop with no message_delta
		sse(evStart, evDelta, `{"type":"message_delta","delta":{"stop_reason":null},"usage":{"output_tokens":9}}`, `{"type":"ping"}`, evStop),
		sse(evStart, evTextStart, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`),
		"data: {not json\n\n", "event: ping\n\n",
		// A line longer than the scanner's initial buffer (a line over provider.MaxSSELine, 32MB, is
		// too large for a seed; errors_test.go covers it).
		sse(evStart, evTextStart, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+strings.Repeat("a", 80<<10)+`"}}`, evTextStop, evDelta, evStop),
	} {
		f.Add([]byte(s), uint32(len(s)/2))
	}
	f.Fuzz(func(t *testing.T, body []byte, cut uint32) {
		modeltest.CheckSSEPrefix(t, streamSSE, body, int(cut%uint32(len(body)+1)))
	})
}
