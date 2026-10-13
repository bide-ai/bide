package agent

import (
	"testing"
	"time"
)

// TestNotifyBudget_RoundCoversBothAcks checks roundBudget counts the overdue notice's
// acknowledgement as well as the event's (Model 14 finding N3): a round that starts under a claim
// with more than roundBudget left ends inside it, and a fresh claim always has that much.
func TestNotifyBudget_RoundCoversBothAcks(t *testing.T) {
	for _, ttl := range []time.Duration{30 * time.Second, 2 * time.Minute, 16 * time.Minute, time.Hour} {
		e := &notifyEngine{cfg: defaultNotifyConfig()}
		e.cfg.claimTTL = ttl
		worst := time.Duration(e.cfg.roundAttempts+1)*e.callTimeout() +
			e.cfg.attemptBackoff*time.Duration(1<<(e.cfg.roundAttempts-1)-1) +
			ttl/8 + ttl/8 // the event's acknowledgement, then the notice's
		if got := e.roundBudget(); got < worst {
			t.Errorf("ttl %v: roundBudget %v is less than a round's worst case %v", ttl, got, worst)
		}
		if got := e.roundBudget(); got >= ttl {
			t.Errorf("ttl %v: roundBudget %v does not fit inside a fresh claim", ttl, got)
		}
	}
}

// TestNotifyBudget_NoticeClaimCoversItsCall checks a peer's notice claim outlasts the notice's call
// and its acknowledgement (Model 14 finding N2), so another peer cannot claim and resend it while
// the first call is still running, and never outlasts the event's own claim.
func TestNotifyBudget_NoticeClaimCoversItsCall(t *testing.T) {
	for _, ttl := range []time.Duration{30 * time.Second, 2 * time.Minute, 16 * time.Minute, time.Hour} {
		e := &notifyEngine{cfg: defaultNotifyConfig()}
		e.cfg.claimTTL = ttl
		got := e.noticeClaimTTL()
		if need := e.callTimeout() + ttl/8; got < need && got != ttl {
			t.Errorf("ttl %v: notice claim %v is shorter than its call and ack %v", ttl, got, need)
		}
		if got > ttl {
			t.Errorf("ttl %v: notice claim %v outlasts the event's claim", ttl, got)
		}
	}
}
