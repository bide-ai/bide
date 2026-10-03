package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/middleware"
)

// The adapter does not send a JSON-schema response format, so a request that sets one is a
// config error before anything is sent, rather than a request sent without the constraint: the
// model then answers in free text, and RunTypedNative fails to decode it only after the run is
// complete, or decodes JSON that no schema constrained.
func TestStream_ResponseFormatIsAConfigError(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		// A complete text turn, so a request that reaches the server gets an answer.
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n" +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"{\\\"a\\\":\\\"free\\\"}\"}}\n\n" +
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer srv.Close()
	m := New("k", WithBaseURL(srv.URL))

	_, err := m.Stream(context.Background(), agent.Request{
		Messages:       []agent.Message{agent.UserText("hi")},
		ResponseFormat: &agent.ResponseFormat{Name: "r", Schema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`)},
	})
	if !errors.Is(err, agent.ErrConfig) || middleware.Retryable(err) {
		t.Errorf("Stream = %v (retryable %v), want a non-retryable ErrConfig", err, middleware.Retryable(err))
	}

	type out struct {
		A string `json:"a"`
	}
	got, err := agent.RunTypedNative[out](context.Background(), agenttest.MustNew(m, agenttest.MemJournal()), "r", "hi")
	if !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("RunTypedNative = %+v, %v; want ErrConfig", got, err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the adapter sent %d requests, want 0", n)
	}
}
