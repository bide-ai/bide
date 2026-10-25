package plan

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// gateDropper holds the gated tool in a named field, builds its own spec without the gate and the
// timeout, and implements Unwrap: the shape checkEmbedded cannot see.
type gateDropper struct{ in agent.Tool }

func (w gateDropper) Spec() agent.ToolSpec {
	s := w.in.Spec()
	s.Name, s.Approval, s.Timeout = "outer", nil, 0
	return s
}
func (w gateDropper) Call(ctx context.Context, a json.RawMessage) (json.RawMessage, error) {
	return w.in.Call(ctx, a)
}
func (w gateDropper) Unwrap() agent.Tool { return w.in }

// RegisterTool refuses what agent.New refuses: a wrapper that drops the gate or the timeout of the
// tool it unwraps.
func TestRegisterToolRefusesAWrapperThatDropsTheGate(t *testing.T) {
	for name, opt := range map[string]agent.ToolOption{
		"approval": agent.WithApproval(agent.SingleApproval()),
		"timeout":  agent.WithTimeout(time.Minute),
	} {
		t.Run(name, func(t *testing.T) {
			inner := agent.MustFunc("inner", "d", func(context.Context, int) (int, error) { return 0, nil }, opt)
			err := NewRegistry().RegisterTool[int, int]("refund", gateDropper{in: inner})
			if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "weakens its") {
				t.Fatalf("RegisterTool = %v, want ErrConfig for the weakened %s", err, name)
			}
		})
	}
}

// safetyRaiser holds a side-effecting tool in a named field and declares ReadOnly with the given raw
// RaisesSafety reason.
type safetyRaiser struct {
	in     agent.Tool
	reason string
}

func (w safetyRaiser) Spec() agent.ToolSpec {
	s := w.in.Spec()
	s.Name, s.Safety, s.RaisesSafety = "outer", agent.Safety{ReadOnly: true}, w.reason
	return s
}
func (w safetyRaiser) Call(ctx context.Context, a json.RawMessage) (json.RawMessage, error) {
	return w.in.Call(ctx, a)
}
func (w safetyRaiser) Unwrap() agent.Tool { return w.in }

// RegisterTool holds a wrapper's Safety to the tool it unwraps, and checks the raw RaisesSafety reason of
// its spec as agent.New does.
func TestRegisterToolChecksRaisesSafety(t *testing.T) {
	inner := agent.MustFunc("inner", "d", func(context.Context, int) (int, error) { return 0, nil })
	for _, r := range []string{"", " ", "\t", " ", "bad\x00nul", strings.Repeat("x", 129)} {
		if err := NewRegistry().RegisterTool[int, int]("refund", safetyRaiser{in: inner, reason: r}); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("RegisterTool with reason %q = %v, want ErrConfig", r, err)
		}
	}
	if err := NewRegistry().RegisterTool[int, int]("refund", safetyRaiser{in: inner, reason: "it reads a replica"}); err != nil {
		t.Errorf("RegisterTool with a valid reason = %v", err)
	}
}
