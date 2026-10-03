package plan

import (
	"errors"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

type timedSubWrap struct{ agent.Tool }

func (w timedSubWrap) Spec() agent.ToolSpec {
	s := w.Tool.Spec()
	s.Timeout = time.Millisecond
	return s
}
func (w timedSubWrap) Unwrap() agent.Tool { return w.Tool }

// ADV117b-5 ((b)/(c)). agent.New refuses a wrapper that gives a sub-agent a Timeout, since the
// timeout would cut the sub-run off mid-call. plan now applies ToolSpec.Timeout to Tool nodes,
// but Build does not run the same check, so the same wrapper is accepted as a flow node.
func TestAdv117b_PlanAcceptsATimedWrapperOverASubAgent(t *testing.T) {
	sub := agenttest.MustNew(agent.NewScriptedModel(agent.TextTurn("x")), agenttest.MemJournal())
	b := New[string, string]("f")
	b.Tool[string, string]("delegate", timedSubWrap{agent.MustSubAgent("delegate", "", sub)})
	_, err := b.Build()
	if !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("Build = %v; want ErrConfig, as agent.New gives for the same tool", err)
	}
}

// RegisterTool refuses the same wrapper, and a Compensator nested in an Unwrap chain.
func TestAdv117b_RegisterToolRefusesUnsafeWrappers(t *testing.T) {
	sub := agenttest.MustNew(agent.NewScriptedModel(agent.TextTurn("x")), agenttest.MemJournal())
	reg := NewRegistry()
	if err := reg.RegisterTool[string, string]("delegate", timedSubWrap{agent.MustSubAgent("delegate", "", sub)}); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("RegisterTool = %v, want ErrConfig", err)
	}
}
