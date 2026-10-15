package approvecmd

import (
	"encoding/json"
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

// -list shows a waiting gate's deadline and the time left on the command's clock, in the text and
// the JSON.
func TestDeadline_ListShowsTheDeadline(t *testing.T) {
	f := newFixture(t, &agent.ApprovalPolicy{Need: 2, Approvers: approvers, Timeout: time.Hour}, refundArgs)
	code, out, errs := f.run_("", "-list")
	if code != 0 || !strings.Contains(out, "due in ") || !strings.Contains(out, "times out after 1h") || !strings.Contains(out, "bide approve") {
		t.Fatalf("-list = %d\n%s%s; want the deadline and the approve commands", code, out, errs)
	}
	_, js, _ := f.run_("", "-list", "-json")
	var got []struct {
		ExpiresAt string `json:"expires_at"`
		Due       string `json:"due"`
		Overdue   bool   `json:"overdue"`
	}
	if err := json.Unmarshal([]byte(js), &got); err != nil || len(got) != 1 || got[0].ExpiresAt == "" || !strings.HasPrefix(got[0].Due, "due in ") || got[0].Overdue {
		t.Fatalf("-list -json = %s (%v)", js, err)
	}
}

// Past the deadline the gate refuses decisions, so -list says so and offers no command.
func TestDeadline_ListOverdueOffersNoCommand(t *testing.T) {
	f := newFixture(t, &agent.ApprovalPolicy{Need: 2, Approvers: approvers, Timeout: time.Hour}, refundArgs)
	saved := now
	now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	defer func() { now = saved }()
	_, out, _ := f.run_("", "-list")
	if !strings.Contains(out, "deadline passed") || strings.Contains(out, "bide approve -db") {
		t.Fatalf("-list after the deadline:\n%s", out)
	}
	_, js, _ := f.run_("", "-list", "-json")
	if !strings.Contains(js, `"overdue": true`) || !strings.Contains(js, `"commands": []`) {
		t.Fatalf("-list -json after the deadline:\n%s", js)
	}
}
