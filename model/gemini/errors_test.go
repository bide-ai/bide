package gemini

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

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

// bodyClient is an http.Client whose server is a function: the reply body is built in memory, so a test
// of a huge or a cut reply depends on no socket (a 40 MB write on a loaded host fails with ENOBUFS and
// cuts the stream, which is how this test once failed).
func bodyClient(body func() io.Reader) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(body())}, nil
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type aBytes struct{}

func (aBytes) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'A'
	}
	return len(p), nil
}

// A response line longer than the scanner allows fails the same way on every attempt: it must
// be a model error that is not retried, not a bare bufio error.
func TestStream_OverlongLineIsNotRetried(t *testing.T) {
	c := bodyClient(func() io.Reader {
		return io.MultiReader(strings.NewReader(`data: {"candidates":[{"content":{"parts":[{"text":"`),
			io.LimitReader(aBytes{}, 40<<20), strings.NewReader("\"}]},\"finishReason\":\"STOP\"}]}\n\n"))
	})
	_, _, err := agent.Generate(context.Background(), New("k", WithHTTPClient(c)), agent.Request{})
	if !errors.Is(err, agent.ErrModel) || middleware.Retryable(err) {
		t.Fatalf("err = %v (retryable %v), want a non-retryable ErrModel", err, middleware.Retryable(err))
	}
}

// A stream cut partway through an event is the read error that cut it, retried; it is not the half event
// decoded ("unexpected end of JSON input").
func TestStream_CutMidEventIsAReadError(t *testing.T) {
	c := bodyClient(func() io.Reader {
		return io.MultiReader(strings.NewReader(`data: {"candidates":[{"content":{"parts":[{"text":"hi`), iotest.ErrReader(io.ErrUnexpectedEOF))
	})
	_, _, err := agent.Generate(context.Background(), New("k", WithHTTPClient(c)), agent.Request{})
	if !errors.Is(err, agent.ErrModel) || !errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(fmt.Sprint(err), "sse decode") || !middleware.Retryable(err) {
		t.Fatalf("err = %v, want a retryable ErrModel wrapping the read error", err)
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
