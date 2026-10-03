package mcptools

import (
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// A null output schema is no output schema; a later WithApproval for a name wins; a negative
// timeout is none.
func TestRev117e_NullOutputSchemaAndLaterApproval(t *testing.T) {
	tools, err := listed(t, []map[string]any{{"name": "x", "inputSchema": objectSchema, "outputSchema": nil}},
		WithApproval("x", agent.SingleApproval()),
		WithApproval("x", &agent.ApprovalPolicy{Need: 1, Approvers: []string{"a"}}),
		WithCallTimeout(-time.Second))
	if err != nil {
		t.Fatalf("Tools = %v", err)
	}
	s := tools[0].Spec()
	if s.Output != nil || s.Approval == nil || len(s.Approval.Approvers) != 1 || s.Timeout != 0 {
		t.Fatalf("spec = %+v", s)
	}
}
