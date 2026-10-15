package plan

import (
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// The authored diagram labels a gated node with its policy: need, approvers, whether the requester
// is excluded (separation of duties) and the deadline.
func TestRenderMermaid_GateLabels(t *testing.T) {
	for _, c := range []struct {
		name string
		pol  *agent.ApprovalPolicy
		want string
	}{
		{"m-of-n with both", &agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}, ExcludePrincipal: true, Timeout: 24 * time.Hour},
			"approval: 2 of 3 (ops, finance, risk), excluding the requester, times out after 24h"},
		{"m-of-n plain", &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}, "approval: 1 of 1 (ops)"},
		{"single with a deadline", withTimeout(agent.SingleApproval(), 90*time.Minute), "approval: one decision, times out after 1h30m"},
		{"single", agent.SingleApproval(), "approval: one decision"},
	} {
		core := fixtureCore()
		core.nodes[3].approval = c.pol
		got := core.renderMermaid()
		if !strings.Contains(got, "reject : int -> bool | "+c.want) {
			t.Errorf("%s: want the reject node labelled %q, got:\n%s", c.name, c.want, got)
		}
		if strings.Contains(got, "parse : string -> int | approval") {
			t.Errorf("%s: an ungated node is labelled with a gate:\n%s", c.name, got)
		}
	}
}

func TestTopologyGateTimeoutText(t *testing.T) {
	for ms, want := range map[int64]string{0: "", -5: "", 500: "500ms", 90_000: "1m30s", 3_600_000: "1h", 5_400_000: "1h30m", 86_400_000: "24h", 60_000: "1m"} {
		if got := (&TopologyGate{TimeoutMS: ms}).TimeoutText(); got != want {
			t.Errorf("TimeoutText(%dms) = %q, want %q", ms, got, want)
		}
	}
	if (*TopologyGate)(nil).TimeoutText() != "" {
		t.Error("a nil gate has a timeout text")
	}
}

func withTimeout(p *agent.ApprovalPolicy, d time.Duration) *agent.ApprovalPolicy {
	p.Timeout = d
	return p
}
