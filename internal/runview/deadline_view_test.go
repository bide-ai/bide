package runview

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// dlT0 is when the gates in these tests asked: the deadline is dlT0 plus the timeout.
var dlT0 = time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)

func stamp(t time.Time) string { return agent.FormatTimestamp(t) }

func rec(name string, v any) agent.Record {
	b, _ := json.Marshal(v)
	return agent.Record{Name: name, Kind: agent.StepValue, Result: b}
}

// gateRecs is a run paused at an m-of-n gate on call c1 (2 of ops, finance, risk) that asked at
// dlT0 with a one-hour deadline recorded, plus any extra records.
func gateRecs(timeout time.Duration, extra ...agent.Record) []agent.Record {
	recs := []agent.Record{
		{Name: "@llm/0", Kind: agent.StepModel, Message: &agent.Message{Role: agent.RoleAssistant, Parts: []agent.Part{agent.ToolUse{ID: "c1", Name: "refund", Args: json.RawMessage(`{}`)}}}},
		rec(agent.ApprovalPendingStep("c1"), agent.ApprovalWait{ToolUseID: "c1", ToolName: "refund", Need: 2, Approvers: []string{"ops", "finance", "risk"}, RecordedAt: stamp(dlT0)}),
		rec(agent.ApprovalDeadlineStep("c1"), agent.ApprovalDeadline{ToolUseID: "c1", RequestedAt: stamp(dlT0), ExpiresAt: stamp(dlT0.Add(timeout)), TimeoutMS: timeout.Milliseconds()}),
	}
	return append(recs, extra...)
}

func signed(who string, approved bool, decided, recorded time.Time) agent.Record {
	return agent.Record{Name: "approval:c1:" + who + ":" + stamp(decided), Kind: agent.StepApproval, ToolUseID: "c1", Approved: approved,
		ApproverSignature: &agent.ApproverSignature{Approver: who, ApproverAlg: "ed25519", Signature: []byte{1}, DecidedAt: stamp(decided), RecordedAt: stamp(recorded)}}
}

func viewAt(recs []agent.Record, now time.Time) *View {
	return Options{Now: func() time.Time { return now }}.viewOf(context.Background(), "r1", recs, nil)
}

// A waiting gate shows its deadline and the time left on the viewer's clock.
func TestWaitingGateShowsItsDeadline(t *testing.T) {
	g := viewAt(gateRecs(time.Hour), dlT0.Add(20*time.Minute)).Gates[0]
	if !g.Waiting || g.Expired || g.Overdue {
		t.Fatalf("gate %+v; want waiting, not expired", g)
	}
	if g.ExpiresAt != stamp(dlT0.Add(time.Hour)) || g.Timeout != "1h" {
		t.Errorf("expires_at %q, timeout %q", g.ExpiresAt, g.Timeout)
	}
	if want := "due in 40m 00s (2026-10-15 10:00:00 UTC)"; g.Due != want {
		t.Errorf("due = %q; want %q", g.Due, want)
	}
}

// A gate with no deadline says nothing of one.
func TestGateWithoutDeadlineHasNone(t *testing.T) {
	recs := gateRecs(time.Hour)
	g := viewAt(recs[:2:2], dlT0).Gates[0]
	if g.ExpiresAt != "" || g.Due != "" || g.Timeout != "" {
		t.Errorf("gate %+v; want no deadline", g)
	}
}

// Past the deadline by the viewer's clock, with no expiry recorded yet (only a drive records it),
// the gate is overdue: decisions are refused, and a decision recorded after the deadline is listed
// as not counted, not as an approval.
func TestOverdueGateListsLateDecisions(t *testing.T) {
	recs := gateRecs(time.Hour,
		signed("ops", true, dlT0.Add(10*time.Minute), dlT0.Add(10*time.Minute)),
		signed("finance", true, dlT0.Add(59*time.Minute), dlT0.Add(61*time.Minute)), // signed in time, written late
	)
	g := viewAt(recs, dlT0.Add(90*time.Minute)).Gates[0]
	if !g.Overdue || g.Expired || !g.Waiting {
		t.Fatalf("gate %+v; want overdue and still waiting (no drive recorded the expiry)", g)
	}
	if want := "deadline passed 30m 00s ago (2026-10-15 10:00:00 UTC); the expiry is recorded when the run is next driven"; g.Due != want {
		t.Errorf("due = %q; want %q", g.Due, want)
	}
	if len(g.ApprovedBy) != 1 || g.ApprovedBy[0] != "ops" {
		t.Errorf("approved by %v; want only ops (finance's was written after the deadline)", g.ApprovedBy)
	}
	var late *Decision
	for i := range g.Decisions {
		if g.Decisions[i].Approver == "finance" {
			late = &g.Decisions[i]
		}
	}
	if late == nil || late.Note != "not counted: "+agent.ReasonLate {
		t.Fatalf("finance's decision = %+v; want it labelled %q", late, "not counted: "+agent.ReasonLate)
	}
	if !strings.Contains(g.Status, "1 of 3 approvers") {
		t.Errorf("status %q; want the count to leave out the late decision", g.Status)
	}
}

// A recorded expiry shows as expired: its status, headline and class, with the deadline, and the
// decision the gate did not read as not counted.
func TestRecordedExpiryShowsExpired(t *testing.T) {
	tally := agent.ApprovalTally{Need: 2, Approvers: []string{"ops", "finance", "risk"}, Approved: 1, ApprovedBy: []string{"ops"}, Pending: []string{"finance", "risk"}, Expired: true,
		ExpiresAt: stamp(dlT0.Add(time.Hour)), Records: []string{"approval:c1:ops:" + stamp(dlT0.Add(10*time.Minute))}}
	recs := gateRecs(time.Hour,
		signed("ops", true, dlT0.Add(10*time.Minute), dlT0.Add(10*time.Minute)),
		rec(agent.ApprovalTallyStep("c1"), tally),
		agent.Record{Name: agent.ToolResultStep("c1"), Kind: agent.StepToolResult, ToolUseID: "c1", IsError: true, Result: json.RawMessage(agent.ExpiredResult)},
		signed("finance", true, dlT0.Add(59*time.Minute), dlT0.Add(62*time.Minute)),
	)
	v := viewAt(recs, dlT0.Add(3*time.Hour))
	g := v.Gates[0]
	if !g.Expired || g.Waiting || g.Overdue {
		t.Fatalf("gate %+v; want expired, not waiting", g)
	}
	if !strings.HasPrefix(g.Status, "expired (1 of 3 approvers") || g.Class() != "bad" || !strings.HasPrefix(g.Headline(), "expired") {
		t.Errorf("status %q, class %q, headline %q", g.Status, g.Class(), g.Headline())
	}
	if want := "expired (the deadline was 2026-10-15 10:00:00 UTC)"; g.Due != want {
		t.Errorf("due = %q; want %q", g.Due, want)
	}
	for _, d := range g.Decisions {
		if (d.Approver == "finance") != (d.Note == agent.ReasonAfterExpiry) {
			t.Errorf("decision %+v: note %q", d, d.Note)
		}
	}
}

// A one-decision gate that expired (its expiry is the gate's single decision record, a denial).
func TestSingleGateExpiry(t *testing.T) {
	recs := []agent.Record{
		{Name: "@llm/0", Kind: agent.StepModel, Message: &agent.Message{Role: agent.RoleAssistant, Parts: []agent.Part{agent.ToolUse{ID: "c1", Name: "refund", Args: json.RawMessage(`{}`)}}}},
		rec(agent.ApprovalPendingStep("c1"), agent.ApprovalWait{ToolUseID: "c1", ToolName: "refund", Single: true, RecordedAt: stamp(dlT0)}),
		rec(agent.ApprovalDeadlineStep("c1"), agent.ApprovalDeadline{ToolUseID: "c1", RequestedAt: stamp(dlT0), ExpiresAt: stamp(dlT0.Add(time.Hour)), TimeoutMS: time.Hour.Milliseconds()}),
		{Name: "approval:c1", Kind: agent.StepApproval, ToolUseID: "c1", Approved: false, Result: json.RawMessage(`"expired"`),
			ApproverSignature: &agent.ApproverSignature{RecordedAt: stamp(dlT0.Add(2 * time.Hour))}},
	}
	g := viewAt(recs, dlT0.Add(3*time.Hour)).Gates[0]
	if !g.Expired || g.Waiting || !strings.HasPrefix(g.Status, "expired") {
		t.Fatalf("gate %+v; want an expired one-decision gate", g)
	}
}
