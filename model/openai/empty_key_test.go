package openai_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/model/openai"
)

// A provider that wants a key answers a request without one with 401 (OpenRouter: "No auth
// credentials found"). With an empty key, the error says the key is missing, so it does not read
// as a bad key or a provider fault; it is still the provider's APIError. A server that needs no
// key (Ollama, a local vLLM) is unaffected: the request goes without one, as before.
func TestStream_EmptyKeyRefusedNamesTheMissingKey(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("authorization") != "" {
				t.Errorf("a request with an empty key sent authorization %q", r.Header.Get("authorization"))
			}
			w.WriteHeader(status)
			w.Write([]byte(`{"error":{"message":"No auth credentials found","code":401}}`))
		}))
		_, err := openai.New("", openai.WithBaseURL(srv.URL)).Stream(context.Background(), agent.Request{Messages: []agent.Message{agent.UserText("hi")}})
		srv.Close()
		var api *agent.APIError
		if !errors.As(err, &api) || api.StatusCode != status || !strings.Contains(err.Error(), "no API key") {
			t.Fatalf("status %d: err = %v; want the provider's APIError saying no API key was set", status, err)
		}
	}
	// a key that is set and refused is the provider's error alone
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"Incorrect API key provided"}}`))
	}))
	defer srv.Close()
	_, err := openai.New("sk-bad", openai.WithBaseURL(srv.URL)).Stream(context.Background(), agent.Request{Messages: []agent.Message{agent.UserText("hi")}})
	if err == nil || strings.Contains(err.Error(), "no API key") {
		t.Fatalf("a refused key: err = %v; want the provider's error, not a missing-key one", err)
	}
}
