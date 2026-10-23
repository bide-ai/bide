package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// namedWrap holds the inner tool in a NAMED field (so checkEmbedded, which walks anonymous fields,
// does not see it), builds its own spec from mutate, and implements Unwrap.
type namedWrap struct {
	in     agent.Tool
	mutate func(*agent.ToolSpec)
}

func (w namedWrap) Spec() agent.ToolSpec {
	s := w.in.Spec()
	w.mutate(&s)
	return s
}
func (w namedWrap) Call(ctx context.Context, a json.RawMessage) (json.RawMessage, error) {
	return w.in.Call(ctx, a)
}
func (w namedWrap) Unwrap() agent.Tool { return w.in }

// freshSpecWrap implements Spec without embedding or copying the inner spec at all.
type freshSpecWrap struct{ in agent.Tool }

func (w freshSpecWrap) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: "outer", Input: json.RawMessage(`{"type":"object"}`), Safety: w.in.Spec().Safety}
}
func (w freshSpecWrap) Call(ctx context.Context, a json.RawMessage) (json.RawMessage, error) {
	return w.in.Call(ctx, a)
}
func (w freshSpecWrap) Unwrap() agent.Tool { return w.in }

func innerTool(opts ...agent.ToolOption) agent.Tool {
	return agent.MustFunc("inner", "d", func(context.Context, struct{}) (string, error) { return "ok", nil }, append([]agent.ToolOption{agent.WithSafety(agent.Safety{ReadOnly: true})}, opts...)...)
}

func register(t agent.Tool) error {
	_, err := agent.New(agenttest.NewScriptedModel(), agenttest.MemJournal(), agent.WithTools(t),
		agent.WithApproverVerifiers(func(string) (agent.ApproverVerifier, bool) { return nil, false }))
	return err
}

func gate(need int, ids ...string) *agent.ApprovalPolicy {
	return &agent.ApprovalPolicy{Need: need, Approvers: ids}
}

// A wrapper may keep or strengthen what the tool it unwraps declares and may not weaken it; each
// weakening is ErrConfig naming the field and both tools, and keeping it is accepted.
func TestUnwrapChain_WeakeningIsRefused(t *testing.T) {
	ab := []string{"alice", "bob"}
	abc := []string{"alice", "bob", "carol"}
	cases := []struct {
		name    string
		inner   []agent.ToolOption
		mutate  func(*agent.ToolSpec)
		wantErr string // "" accepted
	}{
		{"approval dropped", []agent.ToolOption{agent.WithApproval(agent.SingleApproval())}, func(s *agent.ToolSpec) { s.Approval = nil }, "approval gate"},
		{"approval kept", []agent.ToolOption{agent.WithApproval(agent.SingleApproval())}, func(*agent.ToolSpec) {}, ""},
		{"need lowered", []agent.ToolOption{agent.WithApproval(gate(2, abc...))}, func(s *agent.ToolSpec) { s.Approval = gate(1, abc...) }, "needs 1 decisions"},
		{"need raised is stronger", []agent.ToolOption{agent.WithApproval(gate(1, abc...))}, func(s *agent.ToolSpec) { s.Approval = gate(2, abc...) }, ""},
		{"m-of-n replaced by single", []agent.ToolOption{agent.WithApproval(gate(1, ab...))}, func(s *agent.ToolSpec) { s.Approval = agent.SingleApproval() }, "SingleApproval"},
		{"single replaced by m-of-n is not weaker", []agent.ToolOption{agent.WithApproval(agent.SingleApproval())}, func(s *agent.ToolSpec) { s.Approval = gate(1, ab...) }, ""},
		{"approver admitted", []agent.ToolOption{agent.WithApproval(gate(1, ab...))}, func(s *agent.ToolSpec) { s.Approval = gate(1, "alice", "mallory") }, "admits approvers"},
		{"approver pool narrowed", []agent.ToolOption{agent.WithApproval(gate(1, abc...))}, func(s *agent.ToolSpec) { s.Approval = gate(1, ab...) }, ""},
		{"ExcludePrincipal dropped", []agent.ToolOption{agent.WithApproval(&agent.ApprovalPolicy{Need: 1, Approvers: ab, ExcludePrincipal: true})}, func(s *agent.ToolSpec) { s.Approval = gate(1, ab...) }, "ExcludePrincipal"},
		{"ExcludePrincipal added", []agent.ToolOption{agent.WithApproval(gate(1, ab...))}, func(s *agent.ToolSpec) {
			s.Approval = &agent.ApprovalPolicy{Need: 1, Approvers: ab, ExcludePrincipal: true}
		}, ""},
		{"gate deadline dropped", []agent.ToolOption{agent.WithApproval(&agent.ApprovalPolicy{Need: 1, Approvers: ab, Timeout: time.Hour})}, func(s *agent.ToolSpec) { s.Approval = gate(1, ab...) }, "expires after"},
		{"gate deadline lengthened", []agent.ToolOption{agent.WithApproval(&agent.ApprovalPolicy{Need: 1, Approvers: ab, Timeout: time.Hour})}, func(s *agent.ToolSpec) {
			s.Approval = &agent.ApprovalPolicy{Need: 1, Approvers: ab, Timeout: 2 * time.Hour}
		}, "expires after"},
		{"gate deadline shortened", []agent.ToolOption{agent.WithApproval(&agent.ApprovalPolicy{Need: 1, Approvers: ab, Timeout: time.Hour})}, func(s *agent.ToolSpec) {
			s.Approval = &agent.ApprovalPolicy{Need: 1, Approvers: ab, Timeout: time.Minute}
		}, ""},
		{"timeout dropped", []agent.ToolOption{agent.WithTimeout(time.Minute)}, func(s *agent.ToolSpec) { s.Timeout = 0 }, "timeout"},
		{"timeout lengthened", []agent.ToolOption{agent.WithTimeout(time.Minute)}, func(s *agent.ToolSpec) { s.Timeout = time.Hour }, "timeout"},
		{"timeout shortened", []agent.ToolOption{agent.WithTimeout(time.Minute)}, func(s *agent.ToolSpec) { s.Timeout = time.Second }, ""},
		{"inner declares nothing", nil, func(s *agent.ToolSpec) { s.Name = "renamed" }, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := register(namedWrap{in: innerTool(c.inner...), mutate: func(s *agent.ToolSpec) { s.Name = "outer"; c.mutate(s) }})
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("a wrapper that keeps or strengthens it was refused: %v", err)
				}
				return
			}
			if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), c.wantErr) || !strings.Contains(err.Error(), `"outer"`) || !strings.Contains(err.Error(), `"inner"`) {
				t.Fatalf("err = %v, want ErrConfig naming %q and both tools", err, c.wantErr)
			}
		})
	}
}

// A Spec built afresh, with no embedding and no copy of the inner spec, drops the gate too.
func TestUnwrapChain_FreshSpecDropsTheGate(t *testing.T) {
	err := register(freshSpecWrap{in: innerTool(agent.WithApproval(agent.SingleApproval()))})
	if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "approval gate") {
		t.Fatalf("err = %v, want ErrConfig for the dropped approval gate", err)
	}
	if err := register(freshSpecWrap{in: innerTool()}); err != nil {
		t.Fatalf("an inner tool with no gate: %v", err)
	}
}

// Every tool on the chain is compared, not only the next one: the middle wrapper restores nothing.
func TestUnwrapChain_DoubleWrap(t *testing.T) {
	base := innerTool(agent.WithApproval(agent.SingleApproval()), agent.WithTimeout(time.Minute))
	keep := func(*agent.ToolSpec) {}
	// the middle wrapper keeps the gate, the outer one drops it
	mid := namedWrap{in: base, mutate: keep}
	if err := register(namedWrap{in: mid, mutate: func(s *agent.ToolSpec) { s.Name = "outer"; s.Approval = nil }}); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("outer drops the gate over a faithful middle: %v", err)
	}
	// the middle wrapper drops it (and is itself the thing refused if registered), the outer restates it
	lossy := namedWrap{in: base, mutate: func(s *agent.ToolSpec) { s.Approval = nil }}
	if err := register(namedWrap{in: lossy, mutate: func(s *agent.ToolSpec) { s.Name = "outer"; s.Approval = agent.SingleApproval() }}); err != nil {
		t.Fatalf("an outer wrapper that declares the base's gate and timeout was refused: %v", err)
	}
	// the outer one matches the middle (which dropped the timeout) but not the base
	noTimeout := namedWrap{in: base, mutate: func(s *agent.ToolSpec) { s.Timeout = 0 }}
	if err := register(namedWrap{in: noTimeout, mutate: func(s *agent.ToolSpec) { s.Name = "outer" }}); !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("outer matching only the middle: %v", err)
	}
}

// A wrapper with no Unwrap cannot be seen through: that is the documented limit, not a fix.
type noUnwrap struct{ in agent.Tool }

func (w noUnwrap) Spec() agent.ToolSpec {
	s := w.in.Spec()
	s.Name, s.Approval = "outer", nil
	return s
}
func (w noUnwrap) Call(ctx context.Context, a json.RawMessage) (json.RawMessage, error) {
	return w.in.Call(ctx, a)
}

func TestUnwrapChain_WithoutUnwrapIsNotSeen(t *testing.T) {
	if err := register(noUnwrap{in: innerTool(agent.WithApproval(agent.SingleApproval()))}); err != nil {
		t.Fatalf("a wrapper with neither embedding nor Unwrap is invisible to registration, got %v", err)
	}
}
