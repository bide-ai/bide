package rename

import (
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/mcp"
)

func TestRename(t *testing.T) {
	m := agent.NewScriptedModel(agent.ToolTurn("c1", "x", `{}`), agent.TextTurn("done"))
	var _ *agent.ScriptedModel = m
	_ = mcp.Connect()
}
