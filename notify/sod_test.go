package notify

import (
	"strings"
	"testing"
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
