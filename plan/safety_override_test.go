package plan

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// gatedTools are agent tools that declare a human approval gate, 1-of-1 and m-of-n.
func gatedTools() map[string]agent.Tool {
	fn := func(context.Context, int) (int, error) { return 0, nil }
	return map[string]agent.Tool{
		"requires approval": agent.Func("refund", "issue a refund", agent.Safety{RequiresApproval: true}, fn),
		"m-of-n approval": agent.Func("refund", "issue a refund",
			agent.Safety{Approval: &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}}, fn),
	}
}

// A config "safety" value sets how a node is retried on resume. It must not remove the approval
// gate the wrapped tool declares: plan flows refuse a gated node, and a config that could turn the
// gate off would run the tool with no approval at all.
func TestLoad_SafetyOverrideKeepsTheToolsApprovalGate(t *testing.T) {
	for name, tool := range gatedTools() {
		for _, s := range []string{"readonly", "idempotent", "retryable"} {
			reg := NewRegistry()
			if err := RegisterTool[int, int](reg, "refund", tool); err != nil {
				t.Fatalf("register: %v", err)
			}
			cfg := `{"flow":"f","nodes":[{"name":"refund","block":"refund","safety":"` + s + `"}],"wiring":[]}`
			if _, err := Load[int, int]([]byte(cfg), reg); !errors.Is(err, agent.ErrConfig) {
				t.Errorf("%s, safety %q: Load = %v; want ErrConfig for the approval-gated tool", name, s, err)
			}
		}
	}
}

// The Go-side options behave the same way: ReadOnly, Idempotent and Retryable on a gated tool,
// at Builder.Tool or at RegisterTool, keep its approval gate.
func TestNodeOptions_KeepTheToolsApprovalGate(t *testing.T) {
	opts := map[string]NodeOption{"ReadOnly": ReadOnly(), "Idempotent": Idempotent(), "Retryable": Retryable()}
	for name, tool := range gatedTools() {
		for oname, opt := range opts {
			b := New[int, int]("f")
			b.Tool[int, int]("refund", tool, opt)
			if _, err := b.Build(); !errors.Is(err, agent.ErrConfig) {
				t.Errorf("%s, Builder.Tool with %s: Build = %v; want ErrConfig", name, oname, err)
			}
			reg := NewRegistry()
			if err := RegisterTool[int, int](reg, "refund", tool, opt); err != nil {
				t.Fatalf("register: %v", err)
			}
			cfg := `{"flow":"f","nodes":[{"name":"refund","block":"refund"}],"wiring":[]}`
			if _, err := Load[int, int]([]byte(cfg), reg); !errors.Is(err, agent.ErrConfig) {
				t.Errorf("%s, RegisterTool with %s: Load = %v; want ErrConfig", name, oname, err)
			}
		}
	}
}

// An override keeps the tool's IdempotencyKey too: only the ReadOnly/Idempotent classification
// changes.
func TestSafetyOverride_KeepsTheIdempotencyKey(t *testing.T) {
	key := func(json.RawMessage) string { return "k" }
	tool := agent.Func("upsert", "", agent.Safety{IdempotencyKey: key}, func(context.Context, int) (int, error) { return 0, nil })

	b := New[int, int]("f")
	b.Tool[int, int]("upsert", tool, ReadOnly())
	if n := b.core.byName["upsert"]; n.safety.IdempotencyKey == nil || !n.safety.ReadOnly || n.safety.Idempotent {
		t.Errorf("Builder.Tool with ReadOnly: safety %+v; want ReadOnly with the IdempotencyKey kept", n.safety)
	}

	reg := NewRegistry()
	if err := RegisterTool[int, int](reg, "upsert", tool); err != nil {
		t.Fatalf("register: %v", err)
	}
	cfg := `{"flow":"f","nodes":[{"name":"upsert","block":"upsert","safety":"idempotent"}],"wiring":[]}`
	flow, err := Load[int, int]([]byte(cfg), reg)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if n := flow.core.byName["upsert"]; n.safety.IdempotencyKey == nil || n.safety.ReadOnly || !n.safety.Idempotent {
		t.Errorf("config safety idempotent: safety %+v; want Idempotent with the IdempotencyKey kept", n.safety)
	}
}

// An override replaces the classification it names: ReadOnly clears Idempotent and Idempotent
// clears ReadOnly, so the later option wins, as the options document.
func TestSafetyOverride_ReplacesTheClassification(t *testing.T) {
	fn := func(context.Context, int) (int, error) { return 0, nil }
	for _, tc := range []struct {
		base      agent.Safety
		opt       NodeOption
		cfg       string
		wantRO    bool
		wantIdemp bool
	}{
		{agent.Safety{Idempotent: true}, ReadOnly(), "readonly", true, false},
		{agent.Safety{ReadOnly: true}, Idempotent(), "idempotent", false, true},
	} {
		tool := agent.Func("t", "", tc.base, fn)
		b := New[int, int]("f")
		b.Tool[int, int]("t", tool, tc.opt)
		if s := b.core.byName["t"].safety; s.ReadOnly != tc.wantRO || s.Idempotent != tc.wantIdemp {
			t.Errorf("base %+v, option: safety %+v; want ReadOnly=%v Idempotent=%v", tc.base, s, tc.wantRO, tc.wantIdemp)
		}
		reg := NewRegistry()
		if err := RegisterTool[int, int](reg, "t", tool); err != nil {
			t.Fatalf("register: %v", err)
		}
		flow, err := Load[int, int]([]byte(`{"flow":"f","nodes":[{"name":"t","block":"t","safety":"`+tc.cfg+`"}],"wiring":[]}`), reg)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if s := flow.core.byName["t"].safety; s.ReadOnly != tc.wantRO || s.Idempotent != tc.wantIdemp {
			t.Errorf("base %+v, config %q: safety %+v; want ReadOnly=%v Idempotent=%v", tc.base, tc.cfg, s, tc.wantRO, tc.wantIdemp)
		}
	}
}
