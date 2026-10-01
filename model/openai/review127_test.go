package openai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// V1 (review of #127): a configuration this adapter refuses with ErrConfig on every run
// (modeltest.ToolConfig: "required" with no tools declared; a tool name outside [a-zA-Z0-9_-])
// is refused by Build, never accepted there to fail every run.
func TestBuild_RefusesWhatTheAdapterRefuses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not a provider", http.StatusInternalServerError)
	}))
	defer srv.Close()
	m := New("k", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	for name, opts := range map[string][]agent.Option{
		"required, no tools": {agent.WithToolChoice(agent.ToolChoice{Mode: "required"})},
		"space in tool name": {agent.WithTools(agent.Func("get weather", "", agent.Safety{ReadOnly: true},
			func(context.Context, struct{}) (string, error) { return "", nil }))},
	} {
		a, err := agent.Build(m, agent.NewMemStore().Journal(), opts...)
		if err != nil {
			continue // refused at build: what the PR claims
		}
		_, rerr := a.Run(context.Background(), "r", "hi")
		if errors.Is(rerr, agent.ErrConfig) {
			t.Errorf("%s: Build accepted it, and the run fails with ErrConfig: %v", name, rerr)
		}
	}
}
