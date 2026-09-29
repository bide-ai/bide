package plan

import (
	"slices"
	"strings"
	"testing"
)

// approvalNodeConfig is a one-node flow whose node carries an m-of-n approval block.
// Tests substitute APPROVAL with the block under test.
const approvalNodeConfig = `{
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
	if err := RegisterStep(reg, "refund", func(n int) (int, error) { return n, nil }); err != nil {
		t.Fatalf("register: %v", err)
	}
	return reg
}

// TestLoadApprovalLowersToSafety asserts a config "approval" block lowers to
// Safety.Approval on the built node, alongside (not replacing) the config "safety".
func TestLoadApprovalLowersToSafety(t *testing.T) {
	cfg := strings.Replace(approvalNodeConfig, "APPROVAL", `{"need": 2, "approvers": ["ops", "finance", "risk"]}`, 1)
	flow, err := Load[int, int]([]byte(cfg), approvalRegistry(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var n *node
	for _, cand := range flow.core.nodes {
		if cand.name == "refund" {
			n = cand
		}
	}
	if n == nil {
		t.Fatal("refund node not found in loaded flow")
	}
	ap := n.safety.Approval
	if ap == nil {
		t.Fatal("safety.Approval is nil; want the lowered approval policy")
	}
	if ap.Need != 2 {
		t.Errorf("Need = %d, want 2", ap.Need)
	}
	if want := []string{"ops", "finance", "risk"}; !slices.Equal(ap.Approvers, want) {
		t.Errorf("Approvers = %v, want %v", ap.Approvers, want)
	}
	if !n.safety.Idempotent {
		t.Error("config safety \"idempotent\" was lost when the approval block was applied")
	}
}

// TestLoadApprovalAbsentKeepsNil asserts a node without an approval block keeps a nil
// Safety.Approval (the existing 1-of-1 or none behavior).
func TestLoadApprovalAbsentKeepsNil(t *testing.T) {
	cfg := strings.Replace(approvalNodeConfig, `, "approval": APPROVAL`, "", 1)
	flow, err := Load[int, int]([]byte(cfg), approvalRegistry(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, n := range flow.core.nodes {
		if n.safety.Approval != nil {
			t.Errorf("node %q has Approval %+v, want nil", n.name, n.safety.Approval)
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
