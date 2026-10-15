package approvecmd

import (
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// bide approve refuses a decision on a gate whose recorded deadline its clock has passed, and
// records nothing.
func TestDeadline_ApproveRefusesAnExpiredGate(t *testing.T) {
	f := newFixture(t, &agent.ApprovalPolicy{Need: 2, Approvers: approvers, Timeout: time.Hour}, refundArgs)
	saved := now
	now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	defer func() { now = saved }()
	code, _, errs := f.run_("", f.as("ops", "-yes")...)
	if code == 0 || !strings.Contains(errs, "expired") {
		t.Fatalf("bide approve after the deadline = %d: %s; want a refusal naming the expiry", code, errs)
	}
	if got := len(f.decisions()); got != 0 {
		t.Fatalf("%d decisions recorded after the deadline, want 0", got)
	}
}
