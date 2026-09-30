// Package toolcfg checks a request's tools and tool choice against the rules the model
// providers enforce, so an adapter refuses a setup the provider would answer with a 400
// before sending it, with an agent.ErrConfig that says what is wrong.
package toolcfg

import (
	"fmt"
	"regexp"

	"github.com/bide-ai/bide/agent"
)

// OpenAIName and AnthropicName are the tool-name patterns OpenAI and Anthropic document.
// GeminiName is Gemini's: it also allows dots and colons, but must start with a letter or
// an underscore.
var (
	OpenAIName    = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	AnthropicName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	GeminiName    = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.:-]{0,63}$`)
)

// Check validates req for provider, whose tool names must match name. Every tool name must
// match and be unique; a tool choice must have a known mode, and "required" or "tool" needs
// declared tools, "tool" naming one of them. send reports whether the adapter should send
// req.ToolChoice at all: "auto" or "none" with no tools declared is already met, and the
// providers reject a tool choice with no tools, so it is left out.
func Check(provider string, name *regexp.Regexp, req agent.Request) (send bool, err error) {
	seen := map[string]bool{}
	for _, t := range req.Tools {
		n := t.Name
		if !name.MatchString(n) {
			return false, fmt.Errorf("%s: tool name %q does not match %s: %w", provider, n, name, agent.ErrConfig)
		}
		if seen[n] {
			return false, fmt.Errorf("%s: two tools are named %q: %w", provider, n, agent.ErrConfig)
		}
		seen[n] = true
	}
	tc := req.ToolChoice
	if tc == nil {
		return false, nil
	}
	switch tc.Mode {
	case "", "auto", "none":
		return len(req.Tools) > 0, nil
	case "required":
		if len(req.Tools) == 0 {
			return false, fmt.Errorf("%s: tool choice %q with no tools declared: %w", provider, tc.Mode, agent.ErrConfig)
		}
		return true, nil
	case "tool":
		if !seen[tc.Name] {
			return false, fmt.Errorf("%s: tool choice names %q, which is not a declared tool: %w", provider, tc.Name, agent.ErrConfig)
		}
		return true, nil
	default:
		return false, fmt.Errorf("%s: unknown tool choice mode %q (want auto, none, required, or tool): %w", provider, tc.Mode, agent.ErrConfig)
	}
}
