package plan

import (
	"context"
	"errors"
	"github.com/bide-ai/bide/agent"
	"strings"
	"testing"
)

// approvalNodeConfig is a one-node flow whose node carries an m-of-n approval block.
// Tests substitute APPROVAL with the block under test.
const approvalNodeConfig = `{
  "version": 1,
  "flow": "refund-flow",
  "in": "int",
  "out": "int",
  "entry": "refund",
  "nodes": [
    {"name": "refund", "block": "refund", "safety": "idempotent", "approval": APPROVAL}
  ],
  "wiring": []
}`

func approvalRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	if err := reg.RegisterStep("refund", func(_ context.Context, n int) (int, error) { return n, nil }, Idempotent()); err != nil {
		t.Fatalf("register: %v", err)
	}
	return reg
}

// TestLoadApprovalLowersToTheGate asserts a well-formed config "approval" block loads onto the
// node's gate, and that an m-of-n block needs the flow's approver verifiers: without them Load
// refuses it (ErrConfig, naming the node), since no decision could pass the gate.
func TestLoadApprovalLowersToTheGate(t *testing.T) {
	cfg := strings.Replace(approvalNodeConfig, "APPROVAL", `{"need": 2, "approvers": ["ops", "finance", "risk"]}`, 1)
	_, err := Load[int, int]([]byte(cfg), approvalRegistry(t))
	if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), `"refund"`) {
		t.Fatalf("Load without verifiers = %v; want an ErrConfig naming step \"refund\"", err)
	}
	_, vf := approvers("ops", "finance", "risk")
	flow, err := Load[int, int]([]byte(cfg), approvalRegistry(t), WithLoadedApproverVerifiers(vf))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	g := flow.Topology().Nodes[0].Gate
	if g == nil || g.Single || g.Need != 2 || strings.Join(g.Approvers, ",") != "ops,finance,risk" {
		t.Fatalf("loaded gate = %+v; want 2 of ops, finance, risk", g)
	}
}

// A Tool node wrapping an agent tool that requires approval carries the tool's gate.
func TestBuildKeepsAnApprovalGatedTool(t *testing.T) {
	gated := agent.MustFunc("refund", "issue a refund", func(context.Context, int) (int, error) { return 0, nil }, agent.WithApproval(agent.SingleApproval()))
	b := New[int, int]("refunds")
	b.Tool[int, int]("refund", gated)
	f, err := b.Build()
	if err != nil {
		t.Fatalf("Build = %v", err)
	}
	if g := f.Topology().Nodes[0].Gate; g == nil || !g.Single {
		t.Fatalf("gate = %+v; want the tool's single approval", g)
	}
}

// A config "approval" block may restate a tool's own gate but not replace it.
func TestLoadApprovalCannotReplaceAToolsGate(t *testing.T) {
	gated := agent.MustFunc("refund", "issue a refund", func(context.Context, int) (int, error) { return 0, nil },
		agent.WithApproval(&agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance"}}))
	_, vf := approvers("ops", "finance")
	for block, ok := range map[string]bool{
		`{"need": 2, "approvers": ["ops", "finance"]}`: true,
		`{"need": 1, "approvers": ["ops", "finance"]}`: false,
		`{"need": 1, "approvers": ["ops"]}`:            false,
	} {
		reg := NewRegistry()
		if err := reg.RegisterTool[int, int]("refund", gated); err != nil {
			t.Fatal(err)
		}
		cfg := strings.Replace(approvalNodeConfig, "APPROVAL", block, 1)
		cfg = strings.Replace(cfg, `"safety": "idempotent", `, "", 1)
		_, err := Load[int, int]([]byte(cfg), reg, WithLoadedApproverVerifiers(vf))
		if ok != (err == nil) {
			t.Errorf("approval %s on a 2-of-2 tool: Load = %v; want ok=%v", block, err, ok)
		}
	}
}

// TestLoadApprovalAbsentKeepsNil asserts a node without an approval block keeps a nil
// approval gate (the existing 1-of-1 or none behavior).
func TestLoadApprovalAbsentKeepsNil(t *testing.T) {
	cfg := strings.Replace(approvalNodeConfig, `, "approval": APPROVAL`, "", 1)
	flow, err := Load[int, int]([]byte(cfg), approvalRegistry(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, n := range flow.core.nodes {
		if n.approval != nil {
			t.Errorf("node %q has Approval %+v, want nil", n.name, n.approval)
		}
	}
}

// TestLoadApprovalInvalidIsError asserts each invalid approval block is a load error
// naming the node and the violated constraint.
func TestLoadApprovalInvalidIsError(t *testing.T) {
	cases := []struct {
		name  string
		block string
		want  string
	}{
		{"need zero", `{"need": 0, "approvers": ["ops", "finance"]}`, "approval.need 0 must be between 1 and 2"},
		{"need negative", `{"need": -1, "approvers": ["ops"]}`, "approval.need -1 must be between 1 and 1"},
		{"need exceeds approvers", `{"need": 3, "approvers": ["ops", "finance"]}`, "approval.need 3 must be between 1 and 2"},
		{"empty approvers", `{"need": 1, "approvers": []}`, "approval.approvers must be non-empty"},
		{"missing approvers", `{"need": 1}`, "approval.approvers must be non-empty"},
		{"duplicate approvers", `{"need": 2, "approvers": ["ops", "finance", "ops"]}`, `approval.approvers lists "ops" more than once`},
		{"empty approver id", `{"need": 1, "approvers": ["ops", ""]}`, "approval.approvers has an empty id"},
		{"case variants", `{"need": 1, "approvers": ["ops", "OPS"]}`, "differ only by case or Unicode normalization"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := strings.Replace(approvalNodeConfig, "APPROVAL", tc.block, 1)
			_, err := Load[int, int]([]byte(cfg), approvalRegistry(t))
			if err == nil {
				t.Fatal("expected approval load error, got nil")
			}
			msg := err.Error()
			if !strings.Contains(msg, `node "refund"`) || !strings.Contains(msg, tc.want) {
				t.Errorf("error does not name the node and constraint %q: %s", tc.want, msg)
			}
			// Validate shares assemble, so it reports the same problem.
			if vErr := Validate([]byte(cfg), approvalRegistry(t)); vErr == nil || !strings.Contains(vErr.Error(), tc.want) {
				t.Errorf("Validate did not report %q: %v", tc.want, vErr)
			}
		})
	}
}
