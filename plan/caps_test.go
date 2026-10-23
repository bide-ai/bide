package plan

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// cappedTool is a tool that carries amount caps (a gate or a refuse rule).
type cappedTool struct{ stubTool }

func (t *cappedTool) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: "refund", Input: json.RawMessage(`{"type":"object","properties":{"amount":{"type":"integer"}}}`),
		Caps: []string{`r: refund.amount[minor] > 5 USD -> refuse`}}
}

// Plan flows are not specified for caps (note 5.9): a node's gate path bypasses effectiveGates, so a
// flow that took a capped tool would run it without the cap. Both ways of adding a tool refuse it,
// naming the tool and the feature, and a tool with no caps is unchanged.
func TestCapsToolIsRefusedAsAFlowBlock(t *testing.T) {
	reg := NewRegistry()
	err := reg.RegisterTool[int, int]("refund", &cappedTool{})
	if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "refund") || !strings.Contains(err.Error(), "caps") {
		t.Fatalf("RegisterTool of a capped tool = %v, want ErrConfig naming the tool and caps", err)
	}
	if err := NewRegistry().RegisterTool[int, int]("echo", &stubTool{}); err != nil {
		t.Fatalf("a tool without caps: %v", err)
	}
	// The same through the builder, at Build.
	b := New[int, int]("f")
	b.Tool[int, int]("refund", &cappedTool{})
	if _, err := b.Build(); !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "caps") {
		t.Fatalf("Build with a capped tool node = %v, want ErrConfig", err)
	}
}
