package agent

import (
	"context"
	"encoding/json"
	"testing"
)

// When the pending approval's gate expired, the all-clear's reason is "expired", and an
// approval_expired event is sent once, keyed by the run and the call.
func TestResolved_ExpiredGate(t *testing.T) {
	ctx := context.Background()
	gate := notifyEvent{key: ApprovalKey("r", "c1", 0), kind: NotificationApprovalPending, runID: "r", opRun: "r",
		approval: &NotifyApproval{ToolUseID: "c1", ToolName: "refund"}}
	for name, expire := range map[string]func(t *testing.T, j *Journal){
		"single": func(t *testing.T, j *Journal) {
			writeRaw(t, j, "r", approvalStep("c1"), Record{Kind: StepApproval, ToolUseID: "c1", Result: json.RawMessage(expiredDecision)})
		},
		"m-of-n": func(t *testing.T, j *Journal) {
			b, _ := json.Marshal(ApprovalTally{Need: 2, Approvers: abc, Expired: true})
			writeRaw(t, j, "r", ApprovalTallyStep("c1"), Record{Kind: StepValue, Result: b})
		},
	} {
		t.Run(name, func(t *testing.T) {
			j := memJournal()
			trail, sink := NewMemNotifyTrail(), &fakeSink{}
			e := testEngine(t, j, sink, trail, resolvedClock(), "p1")
			if res, err := e.round(ctx, gate); err != nil || !res.done {
				t.Fatal(res, err)
			}
			expire(t, j)
			scanAndFlush(t, e, "r")
			scanAndFlush(t, e, "r")
			got := resolvedCalls(sink)
			if len(got) != 1 || got[0].Reason != "expired" {
				t.Fatalf("all-clears = %+v; want one, reason expired", got)
			}
			var expired []Notification
			for _, c := range sink.calls {
				if c.Kind == NotificationApprovalExpired {
					expired = append(expired, c)
				}
			}
			if len(expired) != 1 || expired[0].Key != ExpiredKey("r", "c1") || expired[0].Approval == nil || expired[0].Approval.ToolUseID != "c1" {
				t.Fatalf("approval_expired deliveries = %+v; want one for c1", expired)
			}
		})
	}
}
