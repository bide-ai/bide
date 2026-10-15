package approvals_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// A gate with a deadline: GET /pending gives expires_at, and once the handler's clock passes it the
// call is expired and a decision is refused with nothing recorded.
func TestDeadline_IntakeReportsAndRefusesExpired(t *testing.T) {
	r := newRigWith(t, "c1", &agent.ApprovalPolicy{Need: 2, Approvers: []string{"alice", "bob", "carol"}, Timeout: time.Hour})
	_, out := r.get("/pending?run=" + runID + "&call=c1")
	exp, _ := out["expires_at"].(string)
	at, err := agent.ParseTimestamp(exp)
	if err != nil || out["state"] != "waiting" {
		t.Fatalf("pending = %v; want waiting with expires_at", out)
	}
	r.nowMu.Lock()
	r.now = at.Add(time.Minute)
	r.nowMu.Unlock()
	if _, out = r.get("/pending?run=" + runID + "&call=c1"); out["state"] != "expired" {
		t.Fatalf("pending after the deadline = %v; want expired", out)
	}
	before := r.records()
	if status, out := r.post(r.decision("alice", true)); status != http.StatusConflict || code(out) != "not_waiting" {
		t.Fatalf("a decision after the deadline = %d %v; want 409 not_waiting", status, out)
	}
	if r.records() != before {
		t.Fatal("a refused decision was recorded")
	}
}
