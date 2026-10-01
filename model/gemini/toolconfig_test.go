package gemini

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/model/modeltest"
)

// "auto" or "none" with no tools is already met: no toolConfig is sent (Gemini rejects one
// with no tools).
func TestBuildRequest_NoToolConfigWithoutTools(t *testing.T) {
	for _, mode := range []string{"auto", "none"} {
		body, err := New("k").buildRequest(agent.Request{ToolChoice: &agent.ToolChoice{Mode: mode}})
		if err != nil || strings.Contains(string(body), "toolConfig") {
			t.Errorf("%s: body %s, err %v; want no toolConfig", mode, body, err)
		}
	}
}

// Gemini's own name rule: dots and colons allowed, a leading digit or dash not.
func TestBuildRequest_GeminiToolNames(t *testing.T) {
	stub := func(n string) agent.Tool {
		return agent.Func(n, "t", agent.Safety{}, func(context.Context, struct {
			Q string `json:"q"`
		}) (int, error) {
			return 0, nil
		})
	}
	if _, err := New("k").buildRequest(agent.Request{Tools: []agent.ToolSpec{agent.SpecOf(stub("ns.tool:v1")), agent.SpecOf(stub("_x"))}}); err != nil {
		t.Errorf("dotted name refused: %v", err)
	}
	for _, n := range []string{"1abc", "-x"} {
		if _, err := New("k").buildRequest(agent.Request{Tools: []agent.ToolSpec{agent.SpecOf(stub(n))}}); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("%q: err = %v, want ErrConfig", n, err)
		}
	}
}

// A tool setup the provider would reject with a 400 is a config error, found before sending.
func TestStream_RefusesBadToolConfig(t *testing.T) {
	modeltest.ToolConfig(t, func(url string, c *http.Client) agent.Model {
		return New("k", WithBaseURL(url), WithHTTPClient(c))
	})
}

// Names an MCP server may list but Gemini does not accept (^[a-zA-Z_][a-zA-Z0-9_.:-]{0,63}$): a
// leading digit, dash or dot, or 65 to 128 characters. Each is refused, naming the tool, before a
// request is sent; dotted names Gemini does accept reach it.
func TestStream_RefusesMCPNamesTheProviderRejects(t *testing.T) {
	modeltest.ToolNames(t, func(url string, c *http.Client) agent.Model {
		return New("k", WithBaseURL(url), WithHTTPClient(c))
	},
		[]string{"1tool", "-tool", ".tool", strings.Repeat("a", 65), strings.Repeat("a", 128), "a\nb"},
		[]string{"admin.tools.list", "ns:tool", "_x", strings.Repeat("a", 64)})
}
