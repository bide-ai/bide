package plan

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// A tool that declares untrusted content, or an exemption from the taint gate, cannot be a plan flow
// node until flows have a taint rule (docs/design/untrusted-content-and-amount-caps.md, section 5.9):
// it is refused with ErrConfig, in a registry and in a builder, and a tool that declares neither is
// not.
func TestTaintDeclarations_AreRefusedAsPlanNodes(t *testing.T) {
	fn := func(context.Context, int) (int, error) { return 1, nil }
	declared := map[string]agent.Tool{
		"Untrusted":       agent.MustFunc("lookup", "", fn, agent.Untrusted("crm-notes")),
		"TaintExempt":     agent.MustFunc("lookup", "", fn, agent.TaintExempt("reviewed")),
		"a Safety source": agent.MustFunc("lookup", "", fn, agent.WithSafety(agent.Safety{Untrusted: "inbox"})),
	}
	for what, tool := range declared {
		reg := NewRegistry()
		if err := reg.RegisterTool[int, int]("lookup", tool); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("RegisterTool with %s = %v, want ErrConfig", what, err)
		}
		b := New[int, int]("f")
		b.Tool[int, int]("lookup", tool)
		if _, err := b.Build(); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("Build with a %s tool node = %v, want ErrConfig", what, err)
		}
	}
	plain := agent.MustFunc("lookup", "", fn)
	reg := NewRegistry()
	if err := reg.RegisterTool[int, int]("lookup", plain); err != nil {
		t.Errorf("RegisterTool with a tool that declares nothing = %v", err)
	}
	b := New[int, int]("f")
	b.Tool[int, int]("lookup", plain)
	if _, err := b.Build(); err != nil && strings.Contains(err.Error(), "taint") {
		t.Errorf("Build with a plain tool node refused for taint: %v", err)
	}
}
