package modeltest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	tool := namedTool

	for name, req := range map[string]agent.Request{
		"space in name":    {Messages: msgs, Tools: []agent.ToolSpec{tool("get weather")}},
		"empty name":       {Messages: msgs, Tools: []agent.ToolSpec{tool("")}},
		"slash in name":    {Messages: msgs, Tools: []agent.ToolSpec{tool("fs/read")}},
		"non-ASCII name":   {Messages: msgs, Tools: []agent.ToolSpec{tool("résumé")}},
		"65-char name":     {Messages: msgs, Tools: []agent.ToolSpec{tool(strings.Repeat("a", 65))}},
		"duplicate names":  {Messages: msgs, Tools: []agent.ToolSpec{tool("a"), tool("a")}},
		"unknown mode":     {Messages: msgs, Tools: []agent.ToolSpec{tool("a")}, ToolChoice: &agent.ToolChoice{Mode: "sometimes"}},
		"required, none":   {Messages: msgs, ToolChoice: &agent.ToolChoice{Mode: "required"}},
		"tool, none":       {Messages: msgs, ToolChoice: &agent.ToolChoice{Mode: "tool", Name: "a"}},
		"tool, no name":    {Messages: msgs, Tools: []agent.ToolSpec{tool("a")}, ToolChoice: &agent.ToolChoice{Mode: "tool"}},
		"tool, undeclared": {Messages: msgs, Tools: []agent.ToolSpec{tool("a")}, ToolChoice: &agent.ToolChoice{Mode: "tool", Name: "b"}},
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
		"valid names":    {Messages: msgs, Tools: []agent.ToolSpec{tool("get_weather"), tool("a-1"), tool(strings.Repeat("a", 64))}},
		"auto, no tools": {Messages: msgs, ToolChoice: &agent.ToolChoice{Mode: "auto"}},
		"none, no tools": {Messages: msgs, ToolChoice: &agent.ToolChoice{Mode: "none"}},
		"tool, declared": {Messages: msgs, Tools: []agent.ToolSpec{tool("a")}, ToolChoice: &agent.ToolChoice{Mode: "tool", Name: "a"}},
	} {
		before := hits.Load()
		_, err := m.Stream(context.Background(), req)
		if errors.Is(err, agent.ErrConfig) || hits.Load() == before {
			t.Errorf("%s: err = %v, want the request sent to the provider", name, err)
		}
	}
}

// namedTool is the spec of a tool with a one-string-argument schema and the given name.
func namedTool(name string) agent.ToolSpec {
	return agent.ToolSpec{
		Name:        name,
		Description: "a test tool",
		Input:       json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`),
		Safety:      agent.Safety{ReadOnly: true},
	}
}

// ToolNames checks the adapter's own tool-name rule against names an MCP server may list (the
// MCP grammar is 1 to 128 of A-Z a-z 0-9 _ - ., wider than every provider's): each name in
// refused must fail with agent.ErrConfig that quotes the name, before anything is sent, and each
// name in accepted must reach the provider.
func ToolNames(t *testing.T, newModel func(baseURL string, client *http.Client) agent.Model, refused, accepted []string) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "modeltest: not a provider", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	m := newModel(srv.URL, srv.Client())
	msgs := []agent.Message{agent.UserText("hi")}
	for _, n := range refused {
		before := hits.Load()
		_, err := m.Stream(context.Background(), agent.Request{Messages: msgs, Tools: []agent.ToolSpec{namedTool("ok"), namedTool(n)}})
		if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), strconv.Quote(n)) {
			t.Errorf("%q: err = %v, want agent.ErrConfig naming the tool", n, err)
		}
		if hits.Load() != before {
			t.Errorf("%q: the request reached the provider", n)
		}
	}
	for _, n := range accepted {
		before := hits.Load()
		_, err := m.Stream(context.Background(), agent.Request{Messages: msgs, Tools: []agent.ToolSpec{namedTool(n)}})
		if errors.Is(err, agent.ErrConfig) || hits.Load() == before {
			t.Errorf("%q: err = %v, want the request sent to the provider", n, err)
		}
	}
}
