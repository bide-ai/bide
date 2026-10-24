package notify

import (
	"strings"
	"testing"

	"github.com/bide-ai/bide/internal/gatereason"
)

// The approval payload names the approvers a gate with separation of duties excludes, and omits the
// field when there are none.
func TestPayloadExcludedApprovers(t *testing.T) {
	n := sampleApproval()
	_, plain := notifyJSON(t, plainWebhook(t), n)
	if strings.Contains(plain, `"excluded"`) {
		t.Fatalf("payload without exclusions carries the field: %s", plain)
	}
	n.Approval.Excluded = []string{"alice"}
	_, raw := notifyJSON(t, plainWebhook(t), n)
	if !strings.Contains(raw, `"excluded":["alice"]`) {
		t.Fatalf("payload = %s, want excluded alice", raw)
	}
}

// The approval payload says when the gate asks again, and omits it on the first ask.
func TestPayloadReask(t *testing.T) {
	n := sampleApproval()
	_, plain := notifyJSON(t, plainWebhook(t), n)
	if strings.Contains(plain, `"reask"`) {
		t.Fatalf("first ask carries reask: %s", plain)
	}
	n.Approval.Reask = "approval re-required"
	_, raw := notifyJSON(t, plainWebhook(t), n)
	if !strings.Contains(raw, `"reask":"approval re-required"`) {
		t.Fatalf("payload = %s", raw)
	}
}

// The approval payload carries the reasons the gate asks: the taint reason always, a cap reason's figures
// only with Args for the tool (they are the call's own numbers), and nothing when there is no reason.
func TestPayloadWhy(t *testing.T) {
	amt := int64(600000)
	n := sampleApproval()
	_, plain := notifyJSON(t, plainWebhook(t), n)
	if strings.Contains(plain, `"why"`) {
		t.Fatalf("payload without a reason carries the field: %s", plain)
	}
	n.Approval.Why = []gatereason.Reason{
		{Kind: "taint", Source: "inbound-email", Ref: "tool:read_email/c1", Pos: 4, At: "2026-10-24T10:43:00.000Z"},
		{Kind: "cap", Rule: "refund_one", Action: "gate", Why: "over", Currency: "USD", Amount: &amt, Limit: 500000, At: "2026-10-24T10:43:00.000Z"},
	}
	_, raw := notifyJSON(t, plainWebhook(t), n)
	if !strings.Contains(raw, `"source":"inbound-email"`) || !strings.Contains(raw, `"rule":"refund_one"`) || !strings.Contains(raw, `"why":"over"`) {
		t.Fatalf("payload = %s, want both reasons", raw)
	}
	for _, figure := range []string{`"amount"`, `"limit"`, `"currency"`, "600000", "500000", "USD"} {
		if strings.Contains(raw, figure) {
			t.Errorf("the cap figure %s is in a payload that did not allow the arguments: %s", figure, raw)
		}
	}
	_, with := notifyJSON(t, plainWebhook(t, WithFields(Args(n.Approval.ToolName))), n)
	for _, figure := range []string{`"amount":600000`, `"limit":500000`, `"currency":"USD"`} {
		if !strings.Contains(with, figure) {
			t.Errorf("the payload with Args lacks %s: %s", figure, with)
		}
	}
	// The notification's own reasons are not changed by building a payload.
	if n.Approval.Why[1].Amount == nil || n.Approval.Why[1].Limit != 500000 {
		t.Error("building the payload stripped the figures from the notification")
	}
}
