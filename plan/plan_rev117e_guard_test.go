package plan

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/toolhook"
)

// toolhook.CallGuard is documented as "asked before every tool call reaches its tool" (audit sets
// it to refuse calls under an expired delegated grant). An agent's base handler asks it; a plan
// Tool node's callTool does not, so a flow run under a delegation's context (a tool in the
// sub-run that drives a flow) calls its tools past the grant's expiry.
func TestRev117e_PlanToolNodeSkipsCallGuard(t *testing.T) {
	prev := toolhook.CallGuard
	defer func() { toolhook.CallGuard = prev }()
	toolhook.CallGuard = func(context.Context) error { return errors.New("grant expired") }
	var ran atomic.Int32
	tool := agent.MustFunc("send", "", func(context.Context, int) (int, error) { ran.Add(1); return 1, nil })
	args, _ := json.Marshal(1)
	_, err := callTool(context.Background(), tool, 0, args)
	if ran.Load() != 0 {
		t.Fatalf("the guard refused every call, yet the plan Tool node ran the tool (err %v)", err)
	}
}
