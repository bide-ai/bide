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
}

// pointer receiver Safety, registered as a value
type ptrRecv struct{ agent.Tool }

func (*ptrRecv) Safety() agent.Safety { return agent.Safety{} }

// nested: outer embeds a middle decorator that carries the dead override
type middle struct{ agent.Tool }

func (middle) Safety() agent.Safety { return agent.Safety{} }

type outer struct{ middle }

// Safety from a different embedded type (a mixin), Spec from the tool
type sideEffectMixin struct{}

func (sideEffectMixin) Safety() agent.Safety { return agent.Safety{} }

type mixed struct {
	agent.Tool
	sideEffectMixin
}

// two embedded fields declare Safety: neither is promoted, so the override is lost silently
type readOnlyMixin struct{}

func (readOnlyMixin) Safety() agent.Safety { return agent.Safety{ReadOnly: true} }

type ambiguous struct {
	agent.Tool
	sideEffectMixin
	readOnlyMixin
}

// Safety returning a named type convertible to agent.Safety? (not the same method signature)
type mySafety agent.Safety
type otherSig struct{ agent.Tool }

func (otherSig) Safety() mySafety { return mySafety{} }

// legit: a decorator that overrides Spec and keeps old methods consistent
type legit struct{ agent.Tool }

func (d legit) Spec() agent.ToolSpec { s := d.Tool.Spec(); s.Safety = agent.Safety{}; return s }
func (legit) Safety() agent.Safety   { return agent.Safety{} }

// legit: Name() meaning something else entirely (an unrelated interface)
type displayNamed struct{ agent.Tool }

func (displayNamed) Name() string { return "display name" }

// Call-only decorator with a json schema override via ArgsSchema returning equal-but-reformatted JSON
type reformatted struct{ agent.Tool }

func (d reformatted) ArgsSchema() json.RawMessage {
	var v any
	_ = json.Unmarshal(d.Tool.Spec().Input, &v)
	b, _ := json.MarshalIndent(v, "", " ")
	return b
}

// Every shape in which an old method would be dead is refused, and the shapes that are not
// dead are accepted: a pointer-receiver Safety on a tool registered by value, an override nested
// one embedding down, an override a mixin declares, one two embedded fields declare (neither promoted), a Safety of
// another signature; a Spec that says what the old methods say, and an ArgsSchema that is the
// same JSON reformatted. A Name() that means something else is refused too, as documented: the
// agent cannot tell it from a dead override.
func TestNew_OldMethodShapes(t *testing.T) {
	inner := func() agent.Tool { return idempotentLookup() }
	for _, c := range []struct {
		name    string
		tool    agent.Tool
		refused bool
	}{
		{"pointer receiver, as a pointer", &ptrRecv{inner()}, true},
		{"pointer receiver, as a value", ptrRecv{inner()}, true},
		{"nested embedding", outer{middle{inner()}}, true},
		{"declared by an embedded mixin", mixed{Tool: inner()}, true},
		{"declared by two embedded fields", ambiguous{Tool: inner()}, true},
		{"another signature", otherSig{inner()}, true},
		{"Name meaning something else", displayNamed{inner()}, true},
		{"Spec says what Safety says", legit{inner()}, false},
		{"ArgsSchema reformatted", reformatted{inner()}, false},
	} {
		_, err := agent.New(agenttest.NewScriptedModel(), agenttest.MemJournal(), agent.WithTools(c.tool))
		if refused := errors.Is(err, agent.ErrConfig); refused != c.refused {
			t.Errorf("%s: New = %v; want refused %v", c.name, err, c.refused)
		}
	}
}
