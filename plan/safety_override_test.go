package plan

import (
	"context"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// gatedTools are agent tools that declare a human approval gate, 1-of-1 and m-of-n. They are
// read-only, so every config safety value lowers or keeps their retry safety.
func gatedTools() map[string]agent.Tool {
	fn := func(context.Context, int) (int, error) { return 0, nil }
	return map[string]agent.Tool{
		"requires approval": agent.MustFunc("refund", "issue a refund", fn, agent.WithSafety(agent.Safety{ReadOnly: true}), agent.WithApproval(agent.SingleApproval())),
		"m-of-n approval": agent.MustFunc("refund", "issue a refund",
			fn, agent.WithSafety(agent.Safety{ReadOnly: true}), agent.WithApproval(&agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}})),
	}
}

// hasGate reports whether the flow's only node carries an approval gate.
func hasGate[In, Out any](f *Flow[In, Out]) bool { return f.Topology().Nodes[0].Gate != nil }

// A config "safety" value sets how a node is retried on resume. It must not remove the approval
// gate the wrapped tool declares: a config that could turn the gate off would run the tool with no
// approval at all.
func TestLoad_SafetyOverrideKeepsTheToolsApprovalGate(t *testing.T) {
	_, vf := approvers("ops")
	for name, tool := range gatedTools() {
		for _, s := range []string{"readonly", "idempotent"} {
			reg := NewRegistry()
			if err := reg.RegisterTool[int, int]("refund", tool); err != nil {
				t.Fatalf("register: %v", err)
			}
			cfg := `{"version":1,"flow":"f","nodes":[{"name":"refund","block":"refund","safety":"` + s + `"}],"wiring":[]}`
			f, err := Load[int, int]([]byte(cfg), reg, WithLoadedApproverVerifiers(vf))
			if err != nil || !hasGate(f) {
				t.Errorf("%s, safety %q: Load = %v; want the tool's gate kept", name, s, err)
			}
		}
	}
}

// The Go-side options behave the same way: ReadOnly and Idempotent on a gated tool,
// at Builder.Tool or at RegisterTool, keep its approval gate.
func TestNodeOptions_KeepTheToolsApprovalGate(t *testing.T) {
	_, vf := approvers("ops")
	opts := map[string]NodeOption{"ReadOnly": ReadOnly(), "Idempotent": Idempotent()}
	for name, tool := range gatedTools() {
		for oname, opt := range opts {
			b := New[int, int]("f").WithApproverVerifiers(vf)
			b.Tool[int, int]("refund", tool, opt)
			if f, err := b.Build(); err != nil || !hasGate(f) {
				t.Errorf("%s, Builder.Tool with %s: Build = %v; want the tool's gate kept", name, oname, err)
			}
			reg := NewRegistry()
			if err := reg.RegisterTool[int, int]("refund", tool, opt); err != nil {
				t.Fatalf("register: %v", err)
			}
			cfg := `{"version":1,"flow":"f","nodes":[{"name":"refund","block":"refund"}],"wiring":[]}`
			if f, err := Load[int, int]([]byte(cfg), reg, WithLoadedApproverVerifiers(vf)); err != nil || !hasGate(f) {
				t.Errorf("%s, RegisterTool with %s: Load = %v; want the tool's gate kept", name, oname, err)
			}
		}
	}
}

// An override replaces the classification it names: ReadOnly clears Idempotent and Idempotent
// clears ReadOnly, so the later option wins, as the options document.
func TestSafetyOverride_ReplacesTheClassification(t *testing.T) {
	fn := func(context.Context, int) (int, error) { return 0, nil }
	for _, tc := range []struct {
		base      agent.Safety
		opt       NodeOption
		cfg       string // "" when the config may not set it (it would raise retry safety)
		wantRO    bool
		wantIdemp bool
	}{
		{agent.Safety{Idempotent: true}, ReadOnly(), "", true, false},
		{agent.Safety{ReadOnly: true}, Idempotent(), "idempotent", false, true},
	} {
		tool := agent.MustFunc("t", "", fn, agent.WithSafety(tc.base))
		b := New[int, int]("f")
		b.Tool[int, int]("t", tool, tc.opt)
		if s := b.core.byName["t"].safety; s.ReadOnly != tc.wantRO || s.Idempotent != tc.wantIdemp {
			t.Errorf("base %+v, option: safety %+v; want ReadOnly=%v Idempotent=%v", tc.base, s, tc.wantRO, tc.wantIdemp)
		}
		if tc.cfg == "" {
			continue
		}
		reg := NewRegistry()
		if err := reg.RegisterTool[int, int]("t", tool); err != nil {
			t.Fatalf("register: %v", err)
		}
		flow, err := Load[int, int]([]byte(`{"version":1,"flow":"f","nodes":[{"name":"t","block":"t","safety":"`+tc.cfg+`"}],"wiring":[]}`), reg)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if s := flow.core.byName["t"].safety; s.ReadOnly != tc.wantRO || s.Idempotent != tc.wantIdemp {
			t.Errorf("base %+v, config %q: safety %+v; want ReadOnly=%v Idempotent=%v", tc.base, tc.cfg, s, tc.wantRO, tc.wantIdemp)
		}
	}
}
