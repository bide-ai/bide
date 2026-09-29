package gemini

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/middleware"
)

// Gemini reports a failure partway through a stream as a data line holding an error object.
// It must end the turn as an error carrying the message, classified by its code.
func TestStreamSSE_MidStreamErrorIsReported(t *testing.T) {
	_, _, err := testStream("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]}\n\n" +
		"data: {\"error\":{\"code\":503,\"message\":\"The model is overloaded\",\"status\":\"UNAVAILABLE\"}}\n\n" +
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\".\"}]},\"finishReason\":\"STOP\"}]}\n\n").Message()
	if !errors.Is(err, agent.ErrModel) || !strings.Contains(fmt.Sprint(err), "The model is overloaded") || !middleware.Retryable(err) {
		t.Fatalf("err = %v, want a retryable ErrModel carrying the provider's message", err)
	}
}

// A response line longer than the scanner allows fails the same way on every attempt: it must
// be a model error that is not retried, not a bare bufio error.
func TestStream_OverlongLineIsNotRetried(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"%s\"}]},\"finishReason\":\"STOP\"}]}\n\n", strings.Repeat("A", 40<<20))
	}))
	defer srv.Close()
	_, _, err := agent.Generate(context.Background(), New("k", WithBaseURL(srv.URL)), agent.Request{})
	if !errors.Is(err, agent.ErrModel) || middleware.Retryable(err) {
		t.Fatalf("err = %v (retryable %v), want a non-retryable ErrModel", err, middleware.Retryable(err))
	}
}

// Gemini's own retry hint (RetryInfo.retryDelay) is the wait a retry honors.
func TestStream_RateLimitHonorsRetryDelay(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":{"code":429,"message":"Resource has been exhausted","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"12s"}]}}`)
	}))
	defer srv.Close()
	_, err := New("k", WithBaseURL(srv.URL)).Stream(context.Background(), agent.Request{})
	var rl *agent.RateLimited
	if !errors.As(err, &rl) || rl.RetryAfter.Seconds() != 12 {
		t.Fatalf("err = %v, want *RateLimited with RetryAfter 12s", err)
	}
}
