package plan

import (
	"context"
	"testing"

	"github.com/bide-ai/bide/agent"
)

type deadSafetyDecorator struct{ agent.Tool }

func (deadSafetyDecorator) Safety() agent.Safety { return agent.Safety{} }

// A flow refuses a decorator whose old Safety override is dead, as agent.New does: the agent
// reads only Spec, so a resume would re-run a node the decorator declared a side effect.
func TestRegisterTool_RefusesAnOldMethodThatDisagreesWithSpec(t *testing.T) {
	inner := agent.MustFunc("lookup", "", func(context.Context, int) (int, error) { return 1, nil },
		agent.WithSafety(agent.Safety{Idempotent: true}))
	reg := NewRegistry()
	err := reg.RegisterTool[int, int]("lookup", deadSafetyDecorator{inner})
	t.Logf("RegisterTool err = %v", err)
	if err == nil {
		t.Fatalf("plan accepted a decorator whose Safety() (%+v) disagrees with its Spec (%+v); agent.New refuses it", deadSafetyDecorator{inner}.Safety(), deadSafetyDecorator{inner}.Spec().Safety)
	}
}
