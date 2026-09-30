package anthropic

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// capBody is a well-formed streamed reply of at least n bytes made of many small events, each far
// below the per-line cap.
func capBody(n int) []byte {
	chunk := strings.Repeat("a", 1000)
	var b strings.Builder
	b.WriteString(`data: {"type":"message_start","message":{"usage":{"input_tokens":1}}}` + "\n\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}` + "\n\n")
	for b.Len() < n {
		b.WriteString(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + chunk + `"}}` + "\n\n")
	}
	b.WriteString(`data: {"type":"content_block_stop","index":0}` + "\n\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}` + "\n\n" + `data: {"type":"message_stop"}` + "\n\n")
	return []byte(b.String())
}

func capServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A reply longer than the default response cap (32 MiB) fails with ErrResponseTooLarge, however
// small its lines: without a cap a broken or hostile endpoint grows one turn without limit.
func TestStream_ReplyOverTheDefaultCapIsTooLarge(t *testing.T) {
	srv := capServer(t, capBody(32<<20+1))
	s, err := New("k", WithBaseURL(srv.URL)).Stream(context.Background(), agent.Request{Messages: []agent.Message{agent.UserText("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	msg, _, err := s.Message()
	if !errors.Is(err, agent.ErrResponseTooLarge) {
		t.Fatalf("got %d bytes of text, err %v; want ErrResponseTooLarge", len(msg.Text()), err)
	}
}

// WithMaxResponseBytes sets the cap; zero keeps the default.
func TestStream_WithMaxResponseBytes(t *testing.T) {
	srv := capServer(t, capBody(10_000))
	for max, want := range map[int64]error{4096: agent.ErrResponseTooLarge, 0: nil, 1 << 20: nil} {
		s, err := New("k", WithBaseURL(srv.URL), WithMaxResponseBytes(max)).Stream(context.Background(), agent.Request{Messages: []agent.Message{agent.UserText("hi")}})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Message(); want == nil && err != nil || want != nil && !errors.Is(err, want) {
			t.Errorf("max %d: err %v, want %v", max, err, want)
		}
	}
}
