package openai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// V1 (review of #127): the adapter declares its tool rules (agent.ToolRules), so agent.Build
// refuses a tool name OpenAI refuses, and a run of an agent with tool choice "required" and
// nothing to call fails with ErrConfig before anything reaches the provider.
func TestBuild_FollowsTheAdaptersToolRules(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "not a provider", http.StatusInternalServerError)
	}))
	defer srv.Close()
	m := New("k", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	for _, n := range []string{"get weather", "fs.read", "fs:read"} {
		tool := agent.Func(n, "", agent.Safety{ReadOnly: true}, func(context.Context, struct{}) (string, error) { return "", nil })
		if _, err := agent.Build(m, agent.NewMemStore().Journal(), agent.WithTools(tool)); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("tool %q: Build err = %v, want ErrConfig", n, err)
		}
	}
	a, err := agent.Build(m, agent.NewMemStore().Journal(), agent.WithToolChoice(agent.ToolChoice{Mode: "required"}))
	if err != nil {
		t.Fatalf("Build refused required with no tools (RunTyped may supply one): %v", err)
	}
	if _, err := a.Run(context.Background(), "r", "hi"); !errors.Is(err, agent.ErrConfig) || hits.Load() != 0 {
		t.Errorf("run with required and no tools: err %v, %d requests sent; want ErrConfig and none", err, hits.Load())
	}
}
