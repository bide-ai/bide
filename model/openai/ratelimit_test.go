package openai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

func TestStream_429_RateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	m := New("test-key", WithBaseURL(srv.URL))
	_, err := m.Stream(context.Background(), agent.Request{
		Messages: []agent.Message{agent.UserText("hi")},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var rl *agent.RateLimited
	if !errors.As(err, &rl) {
		t.Fatalf("err = %T(%v), want *agent.RateLimited", err, err)
	}
	if rl.RetryAfter != 2*time.Second {
		t.Errorf("RetryAfter = %v, want 2s", rl.RetryAfter)
	}
	if !errors.Is(err, agent.ErrModel) {
		t.Errorf("errors.Is(err, agent.ErrModel) = false, want true")
	}
}
