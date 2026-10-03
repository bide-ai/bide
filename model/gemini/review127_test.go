package gemini

import (
	"context"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// V1 (review of #127): Gemini declares its own, looser tool-name rule, so agent.Build accepts a
// dotted name for it and refuses one Gemini refuses.
func TestBuild_FollowsGeminisToolNameRule(t *testing.T) {
	m := New("k")
	for n, ok := range map[string]bool{"fs.read": true, "ns:tool": true, "get_weather": true, "1tool": false, "get weather": false} {
		tool := agent.Func(n, "", agent.Safety{ReadOnly: true}, func(context.Context, struct{}) (string, error) { return "", nil })
		_, err := agent.New(m, agenttest.MemJournal(), agent.WithTools(tool))
		if ok != (err == nil) || err != nil && !errors.Is(err, agent.ErrConfig) {
			t.Errorf("tool %q: Build err = %v, want accepted %v", n, err, ok)
		}
	}
}
