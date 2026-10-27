package plan

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// Until the engine enforces flow taint (docs/design/flow-taint.md), no running path creates the escalation
// instance of a node: a gated flow, run to the end, journals no record under a "node:...:taint" subject.
func TestNoRunningPathCreatesANodeInstance(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	policy := agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}
	b := New[int, int]("f").WithApproverVerifiers(func(string) (agent.ApproverVerifier, bool) { return nil, false })
	a := b.Step("a", func(_ context.Context, n int) (int, error) { return n, nil }, ReadOnly())
	p := b.Step("pay", func(_ context.Context, n int) (int, error) { return n, nil }, Approval(&policy))
	b.Edge(a, p)
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	var pend *agent.ApprovalPending
	if _, err := f.Run(ctx, j, "r1", 1); !errors.As(err, &pend) {
		t.Fatalf("Run = %v, want a pause at the gate", err)
	}
	recs, err := j.History(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) == 0 {
		t.Fatal("the run journaled nothing")
	}
	for _, r := range recs {
		if strings.Contains(r.Name, ":taint") || strings.Contains(r.Name, "%3Ataint") || strings.Contains(r.ToolUseID, ":taint") || strings.HasPrefix(r.Name, "gate-reason:") {
			t.Errorf("record %q (tool-use id %q) belongs to an escalation instance", r.Name, r.ToolUseID)
		}
	}
}
