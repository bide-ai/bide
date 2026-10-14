package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// A single-approval plan flow gate honours an unsigned decision only if it follows the gate's request
// in the journal (flowGate); EvaluateGateAfter must agree with it, before or after the request.
func TestEvaluateGateAfter_AgreesWithFlowGateOnASingleDecision(t *testing.T) {
	ctx := context.Background()
	args := json.RawMessage(`{"amount":7}`)
	subj := ApprovalSubject{RunID: "r1", ToolUseID: "node:pay", ToolName: "pay", Args: args}
	for name, tc := range map[string]struct {
		before, after bool // a decision is recorded before / after the request
		approved      bool
		want          string // pauses, passes or denies
	}{
		"denial before the request":   {before: true, approved: false, want: "pauses"},
		"approval before the request": {before: true, approved: true, want: "pauses"},
		"denial after the request":    {after: true, approved: false, want: "denies"},
		"approval after the request":  {after: true, approved: true, want: "passes"},
		"no decision":                 {want: "pauses"},
	} {
		t.Run(name, func(t *testing.T) {
			j := memJournal()
			decide := func() {
				writeRaw(t, j, "r1", approvalStep("node:pay"), Record{Kind: StepApproval, ToolUseID: "node:pay", Approved: tc.approved})
			}
			if tc.before {
				decide()
			}
			var pend *ApprovalPending
			approved, err := flowGate(ctx, j, "r1", "node:pay", "pay", args, SingleApproval(), nil)
			if tc.before && !errors.As(err, &pend) {
				t.Fatalf("setup: flowGate with a decision before its request = %v, %v; want the pause", approved, err)
			}
			if tc.after {
				decide()
				approved, err = flowGate(ctx, j, "r1", "node:pay", "pay", args, SingleApproval(), nil)
			}
			got := "passes"
			switch {
			case errors.As(err, &pend):
				got = "pauses"
			case err != nil:
				t.Fatalf("flowGate: %v", err)
			case !approved:
				got = "denies"
			}
			if got != tc.want {
				t.Fatalf("flowGate %s, want %s", got, tc.want)
			}
			recs, _ := j.History(ctx, "r1")
			st := EvaluateGateAfter(recs, subj, SingleApproval(), nil, ApprovalRequestStep("node:pay"))
			switch got {
			case "pauses":
				if st.Status != GateSingle || st.SingleDecided {
					t.Fatalf("flowGate pauses, EvaluateGateAfter says %+v", st)
				}
			case "denies":
				if st.Status != GateDecided || !st.Denied {
					t.Fatalf("flowGate denies, EvaluateGateAfter says %+v", st)
				}
			case "passes":
				if st.Status != GateSingle || !st.SingleDecided || !st.SingleApproved {
					t.Fatalf("flowGate passes, EvaluateGateAfter says %+v", st)
				}
			}
		})
	}
}
