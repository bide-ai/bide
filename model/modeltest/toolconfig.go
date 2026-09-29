package modeltest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// ToolConfig checks that the adapter refuses a tool setup every provider rejects with
// agent.ErrConfig, before it sends anything: a tool name outside [a-zA-Z0-9_-] or longer than
// 64 characters, two tools with one name, and a tool choice that cannot be met (an unknown
// mode; "required" or "tool" with no tools declared; "tool" naming no tool or one not
// declared). A name every provider accepts, and "auto" or "none" with no tools, must reach the
// provider.
func ToolConfig(t *testing.T, newModel func(baseURL string, client *http.Client) agent.Model) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "modeltest: not a provider", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	m := newModel(srv.URL, srv.Client())
	msgs := []agent.Message{agent.UserText("hi")}
	tool := func(name string) agent.Tool { return namedTool(name) }

	for name, req := range map[string]agent.Request{
		"space in name":    {Messages: msgs, Tools: []agent.Tool{tool("get weather")}},
		"empty name":       {Messages: msgs, Tools: []agent.Tool{tool("")}},
		"slash in name":    {Messages: msgs, Tools: []agent.Tool{tool("fs/read")}},
		"non-ASCII name":   {Messages: msgs, Tools: []agent.Tool{tool("résumé")}},
		"65-char name":     {Messages: msgs, Tools: []agent.Tool{tool(strings.Repeat("a", 65))}},
		"duplicate names":  {Messages: msgs, Tools: []agent.Tool{tool("a"), tool("a")}},
		"unknown mode":     {Messages: msgs, Tools: []agent.Tool{tool("a")}, ToolChoice: &agent.ToolChoice{Mode: "sometimes"}},
		"required, none":   {Messages: msgs, ToolChoice: &agent.ToolChoice{Mode: "required"}},
		"tool, none":       {Messages: msgs, ToolChoice: &agent.ToolChoice{Mode: "tool", Name: "a"}},
		"tool, no name":    {Messages: msgs, Tools: []agent.Tool{tool("a")}, ToolChoice: &agent.ToolChoice{Mode: "tool"}},
		"tool, undeclared": {Messages: msgs, Tools: []agent.Tool{tool("a")}, ToolChoice: &agent.ToolChoice{Mode: "tool", Name: "b"}},
	} {
		before := hits.Load()
		_, err := m.Stream(context.Background(), req)
		if !errors.Is(err, agent.ErrConfig) {
			t.Errorf("%s: err = %v, want agent.ErrConfig", name, err)
		}
		if hits.Load() != before {
			t.Errorf("%s: the request reached the provider", name)
		}
	}
	for name, req := range map[string]agent.Request{
		"valid names":    {Messages: msgs, Tools: []agent.Tool{tool("get_weather"), tool("a-1"), tool(strings.Repeat("a", 64))}},
		"auto, no tools": {Messages: msgs, ToolChoice: &agent.ToolChoice{Mode: "auto"}},
		"none, no tools": {Messages: msgs, ToolChoice: &agent.ToolChoice{Mode: "none"}},
		"tool, declared": {Messages: msgs, Tools: []agent.Tool{tool("a")}, ToolChoice: &agent.ToolChoice{Mode: "tool", Name: "a"}},
	} {
		before := hits.Load()
		_, err := m.Stream(context.Background(), req)
		if errors.Is(err, agent.ErrConfig) || hits.Load() == before {
			t.Errorf("%s: err = %v, want the request sent to the provider", name, err)
		}
	}
}

// namedTool is a tool with a one-string-argument schema and the given name.
type namedTool string

func (n namedTool) Name() string                                                 { return string(n) }
func (namedTool) Description() string                                            { return "a test tool" }
func (namedTool) Safety() agent.Safety                                           { return agent.Safety{ReadOnly: true} }
func (namedTool) Call(context.Context, json.RawMessage) (json.RawMessage, error) { return nil, nil }
func (namedTool) ArgsSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`)
}
