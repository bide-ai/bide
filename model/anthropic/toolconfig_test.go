package anthropic

import (
	"net/http"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/model/modeltest"
)

// "auto" or "none" with no tools is already met: no tool_choice is sent (Anthropic rejects
// one with no tools).
func TestBuildRequest_NoToolChoiceWithoutTools(t *testing.T) {
	for _, mode := range []string{"auto", "none"} {
		body, err := New("k").buildRequest(agent.Request{ToolChoice: &agent.ToolChoice{Mode: mode}})
		if err != nil || strings.Contains(string(body), "tool_choice") {
			t.Errorf("%s: body %s, err %v; want no tool_choice", mode, body, err)
		}
	}
}

// A tool setup the provider would reject with a 400 is a config error, found before sending.
func TestStream_RefusesBadToolConfig(t *testing.T) {
	modeltest.ToolConfig(t, func(url string, c *http.Client) agent.Model {
		return New("k", WithBaseURL(url), WithHTTPClient(c))
	})
}

// Names an MCP server may list but the provider does not accept (^[a-zA-Z0-9_-]{1,64}$): a dot,
// a colon, or 65 to 128 characters. Each is refused, naming the tool, before a request is sent.
func TestStream_RefusesMCPNamesTheProviderRejects(t *testing.T) {
	modeltest.ToolNames(t, func(url string, c *http.Client) agent.Model {
		return New("k", WithBaseURL(url), WithHTTPClient(c))
	},
		[]string{"admin.tools.list", "ns:tool", strings.Repeat("a", 65), strings.Repeat("a", 128), "a\nb"},
		[]string{"get_user", "DATA-export-v2", strings.Repeat("a", 64)})
}
