package plan

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// A gated node whose deadline passes with no approval expires: the flow returns
// ErrApprovalExpired (also ErrApprovalDenied), the node never runs, every later drive returns the
// same, and a decision after the deadline is refused. Plan has no clock option, so the test uses a
// short real timeout.
func TestDeadline_FlowNodeExpires(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		pol  *agent.ApprovalPolicy
		vf   agent.ApproverVerifierFor
	}{
		{"single", func() *agent.ApprovalPolicy { p := agent.SingleApproval(); p.Timeout = 20 * time.Millisecond; return p }(), nil},
		{"m-of-n", &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}, Timeout: 20 * time.Millisecond}, func() agent.ApproverVerifierFor { _, vf := approvers("ops"); return vf }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := agenttest.MemJournal()
			refunds := 0
			f := refundFlow(t, tc.pol, tc.vf, &refunds)
			pend := asPending(t, func() error { _, err := f.Run(ctx, j, "r1", "order-42"); return err }())
			if _, ok, _ := j.Get(ctx, "r1", agent.ApprovalDeadlineStep("node:refund")); !ok {
				t.Fatal("no deadline recorded on the first pause")
			}
			time.Sleep(40 * time.Millisecond)
			for drive := 0; drive < 2; drive++ {
				_, err := f.Run(ctx, j, "r1", "order-42")
				if !errors.Is(err, ErrApprovalExpired) || !errors.Is(err, ErrApprovalDenied) || !errors.Is(err, agent.ErrApprovalExpired) {
					t.Fatalf("drive %d after the deadline = %v; want ErrApprovalExpired", drive, err)
				}
			}
			if refunds != 0 {
				t.Fatalf("the node ran %d times on an expired gate", refunds)
			}
			if err := agent.Approve(ctx, j, "r1", pend.ToolUseID, true); !errors.Is(err, agent.ErrApprovalExpired) {
				t.Fatalf("Approve after the deadline = %v; want ErrApprovalExpired", err)
			}
		})
	}
}

// An approval on time runs the node, even when the flow is driven after the deadline.
func TestDeadline_FlowOnTimeApproval(t *testing.T) {
	ctx := context.Background()
	as, vf := approvers("ops")
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}, Timeout: 50 * time.Millisecond}, vf, &refunds)
	pend := asPending(t, func() error { _, err := f.Run(ctx, j, "r1", "order-42"); return err }())
	as["ops"].decide(t, j, pend, true)
	time.Sleep(70 * time.Millisecond)
	if _, err := f.Run(ctx, j, "r1", "order-42"); err != nil || refunds != 1 {
		t.Fatalf("Run after the deadline with an on-time approval = %v (refunds %d); want the node run once", err, refunds)
	}
}

// A config approval block takes a timeout; the topology and the digest carry it, and Load refuses
// one that is not a positive whole number of milliseconds.
func TestDeadline_ConfigTimeout(t *testing.T) {
	_, vf := approvers("ops", "finance", "risk")
	load := func(block string) (*Flow[int, int], error) {
		cfg := strings.Replace(approvalNodeConfig, "APPROVAL", block, 1)
		return Load[int, int]([]byte(cfg), approvalRegistry(t), WithLoadedApproverVerifiers(vf))
	}
	timed, err := load(`{"need": 2, "approvers": ["ops", "finance", "risk"], "timeout": "48h"}`)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := load(`{"need": 2, "approvers": ["ops", "finance", "risk"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if g := timed.Topology().Nodes[0].Gate; g == nil || g.TimeoutMS != (48*time.Hour).Milliseconds() {
		t.Fatalf("topology gate %+v; want timeout_ms for 48h", g)
	}
	if timed.Digest() == plain.Digest() {
		t.Fatal("the digest does not commit to the gate's timeout")
	}
	for _, bad := range []string{`"forever"`, `"-1h"`, `"0s"`, `"1us"`} {
		if _, err := load(`{"need": 1, "approvers": ["ops"], "timeout": ` + bad + `}`); err == nil || !strings.Contains(err.Error(), "approval.timeout") {
			t.Errorf("timeout %s: Load = %v; want an approval.timeout error", bad, err)
		}
	}
}
