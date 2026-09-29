package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/middleware"
)

// An exhausted quota answers 429 but no wait lifts it: retrying burns attempts for nothing.
// The provider's message must reach the caller.
func TestStream_InsufficientQuotaIsNotRetried(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":{"message":"You exceeded your current quota","type":"insufficient_quota","code":"insufficient_quota"}}`)
	}))
	defer srv.Close()
	_, err := New("k", WithBaseURL(srv.URL)).Stream(context.Background(), agent.Request{})
	if middleware.Retryable(err) {
		t.Errorf("insufficient_quota is retryable: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "You exceeded your current quota") {
		t.Errorf("err = %v, want the provider's message", err)
	}
}

// OpenAI reports a failure partway through a stream as a data line holding an error object.
// It must end the turn as an error carrying the message, not be skipped as an empty chunk.
func TestStreamSSE_MidStreamErrorIsReported(t *testing.T) {
	_, _, err := testStream("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"error\":{\"message\":\"The server had an error while processing your request\",\"type\":\"server_error\"}}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n").Message()
	if !errors.Is(err, agent.ErrModel) || !strings.Contains(fmt.Sprint(err), "The server had an error") {
		t.Fatalf("err = %v, want an ErrModel carrying the provider's message", err)
	}
}

// A line over the scanner's cap fails the same way on every attempt: a non-retryable ErrModel.
func TestStreamSSE_OverlongLineIsNotRetried(t *testing.T) {
	_, _, err := testStream("data: " + strings.Repeat("x", agent.MaxSSELine) + "\n\n").Message()
	if !errors.Is(err, agent.ErrResponseTooLarge) || middleware.Retryable(err) {
		t.Fatalf("err = %v, want a non-retryable ErrResponseTooLarge", err)
	}
}

// A config error is the caller's mistake: the same request fails the same way every time.
func TestStream_ConfigErrorIsNotRetried(t *testing.T) {
	_, err := New("k").Stream(context.Background(), agent.Request{ResponseFormat: &agent.ResponseFormat{Name: "x",
		Schema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"object","additionalProperties":{"type":"integer"}}}}`)}})
	if !errors.Is(err, agent.ErrConfig) || middleware.Retryable(err) {
		t.Fatalf("err = %v (retryable %v), want a non-retryable ErrConfig", err, middleware.Retryable(err))
	}
}
