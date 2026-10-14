package audit_test

import (
	"crypto/ed25519"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// REVIEW PROOF: an acknowledgement appended after the signed head is unattested, yet it turns an
// event that was never acknowledged at the head into "acked", and the audit passes.
func TestReview_Notify_UnattestedTailAckPassesAudit(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	k := agent.ApprovalKey("r1", "c1", 0)
	b := newTrail(t).add(observed(k, agent.NotificationApprovalPending, 0, 1), claimed(k, 1))
	head := headFor(t, b, priv)
	b.add(acked(k, 2, 3)) // forged by whoever can append to the stored trail
	rep, err := anchored(b, b.export(), head, pub)
	if err == nil && rep.Trails[0].Tail == 1 && notifyStatusOf(t, rep, k) == audit.NotifyStatusAcked {
		t.Fatalf("event %s reported acked on an unattested tail record, audit passed (anchored=%v)", k, rep.Trails[0].Anchored)
	}
}
