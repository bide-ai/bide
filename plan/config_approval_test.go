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

// TestLoadApprovalIsRefusedUntilEnforced asserts a well-formed config "approval" block is a load
// error naming the node: the plan runtime does not enforce the gate yet, and a gate that loads
// but never stops anything would let the node run with no approval.
func TestLoadApprovalIsRefusedUntilEnforced(t *testing.T) {
	cfg := strings.Replace(approvalNodeConfig, "APPROVAL", `{"need": 2, "approvers": ["ops", "finance", "risk"]}`, 1)
	_, err := Load[int, int]([]byte(cfg), approvalRegistry(t))
	if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), `"refund"`) {
		t.Fatalf("Load = %v; want an ErrConfig naming step \"refund\"", err)
	}
}

// A Tool node wrapping an agent tool that requires approval would run it with no approval.
func TestBuildRefusesAnApprovalGatedTool(t *testing.T) {
	gated := agent.MustFunc("refund", "issue a refund", func(context.Context, int) (int, error) { return 0, nil }, agent.WithApproval(agent.SingleApproval()))
	b := New[int, int]("refunds")
	b.Tool[int, int]("refund", gated)
	if _, err := b.Build(); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("Build = %v; want ErrConfig for an approval-gated tool", err)
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
