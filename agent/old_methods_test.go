package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// notifyDecorator is a decorator written for the old Tool method set: it embeds the tool it
// decorates, adds a side effect of its own, and declares itself a side effect through the old
// Safety method. The agent reads only Spec, which it promotes from the embedded tool
// (Idempotent), so the override would be dead and a resume would fire the decorator again.
type notifyDecorator struct{ agent.Tool }

func (notifyDecorator) Safety() agent.Safety { return agent.Safety{} }

// renamedDecorator overrides the name the old way.
type renamedDecorator struct{ agent.Tool }

func (renamedDecorator) Name() string { return "other" }

// agreeing has old methods that say what its Spec says: harmless helpers.
type agreeing struct{ agent.Tool }

func (a agreeing) Name() string { return a.Tool.Spec().Name }

// specDecorator overrides the way the new API does: in its Spec.
type specDecorator struct{ agent.Tool }

func (d specDecorator) Spec() agent.ToolSpec {
	s := d.Tool.Spec()
	s.Safety = agent.Safety{}
	return s
}

func idempotentLookup() agent.Tool {
	return agent.MustFunc("lookup", "look up", func(context.Context, struct{}) (string, error) { return "ok", nil },
		agent.WithSafety(agent.Safety{Idempotent: true}))
}

// A tool whose old-method-set method disagrees with its Spec is refused when the agent is built:
// the agent reads only Spec, so the method's value would be silently ignored (a side effect
// declared through Safety() re-run on resume, a name the model is never shown).
func TestNew_RefusesAnOldMethodThatDisagreesWithSpec(t *testing.T) {
	for name, tool := range map[string]agent.Tool{
		"Safety": notifyDecorator{idempotentLookup()},
		"Name":   renamedDecorator{idempotentLookup()},
	} {
		a, err := agent.New(agenttest.NewScriptedModel(), agenttest.MemJournal(), agent.WithTools(tool))
		if !errors.Is(err, agent.ErrConfig) || a != nil {
			t.Errorf("%s override: New = %v, %v; want nil and ErrConfig", name, a, err)
		}
	}
	for name, tool := range map[string]agent.Tool{
		"agreeing helper": agreeing{idempotentLookup()},
		"Spec override":   specDecorator{idempotentLookup()},
	} {
		if _, err := agent.New(agenttest.NewScriptedModel(), agenttest.MemJournal(), agent.WithTools(tool)); err != nil {
			t.Errorf("%s: New = %v, want accepted", name, err)
		}
	}
	var _ json.RawMessage
}
