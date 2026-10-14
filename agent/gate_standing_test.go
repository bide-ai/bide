package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// EvaluateGate reads a recorded denial as final, whatever the policy is now, and a recorded result as
// the call having run (or been denied): the cases the loop decides before it looks at the policy.
func TestEvaluateGate_RecordedOutcomesComeBeforeThePolicy(t *testing.T) {
	s := ApprovalSubject{RunID: "r", ToolUseID: "c1", ToolName: "charge"}
	denial, _ := json.Marshal(ApprovalTally{Need: 2, Approvers: []string{"a", "b", "c"}, Denied: 2, DeniedBy: []string{"a", "b"}, DecisionFormat: DecisionFormat})
	pass, _ := json.Marshal(ApprovalTally{Need: 1, Approvers: []string{"a"}, Approved: 1, ApprovedBy: []string{"a"}, DecisionFormat: DecisionFormat})
	tally := func(b []byte) Record { return Record{Name: ApprovalTallyStep("c1"), Kind: StepValue, Result: b} }
	human := func(ok bool) Record {
		return Record{Name: "approval:c1", Kind: StepApproval, ToolUseID: "c1", Approved: ok}
	}
	ran := Record{Name: "tool:c1", Kind: StepToolResult, ToolUseID: "c1", Result: []byte(`"ok"`)}
	denied := Record{Name: "tool:c1", Kind: StepToolResult, ToolUseID: "c1", IsError: true, Result: []byte(`"tool call denied by human"`)}
	mofn := &ApprovalPolicy{Need: 1, Approvers: []string{"a"}}
	for name, tc := range map[string]struct {
		recs   []Record
		pol    *ApprovalPolicy
		status GateStatus
		denied bool
		ran    bool
	}{
		"a recorded denial tally, no policy now":       {[]Record{tally(denial)}, nil, GateDecided, true, false},
		"a recorded denial tally, a one-decision gate": {[]Record{tally(denial)}, SingleApproval(), GateDecided, true, false},
		"a human denial, no policy now":                {[]Record{human(false)}, nil, GateDecided, true, false},
		"a human denial, an m-of-n policy":             {[]Record{human(false)}, mofn, GateDecided, true, false},
		"a human approval, a one-decision gate":        {[]Record{human(true)}, SingleApproval(), GateSingle, false, false},
		"a recorded pass, no policy now":               {[]Record{tally(pass)}, nil, GateUngated, false, false},
		"a result":                                     {[]Record{ran}, mofn, GateDecided, false, true},
		"a denial's result":                            {[]Record{denied}, mofn, GateDecided, true, true},
	} {
		t.Run(name, func(t *testing.T) {
			got := EvaluateGate(tc.recs, s, tc.pol, fakeVerifiers("a", "b", "c"))
			if got.Status != tc.status || got.Denied != tc.denied || got.Ran != tc.ran {
				t.Fatalf("%+v, want status %s denied %v ran %v", got, tc.status, tc.denied, tc.ran)
			}
		})
	}
}

// GateReask is the rule the gate applies and the viewers read.
func TestGateReask(t *testing.T) {
	passed := ApprovalTally{Need: 1, Approved: 1, ApprovedBy: []string{"a"}, DecisionFormat: DecisionFormat}
	for name, tc := range map[string]struct {
		t    ApprovalTally
		sod  bool
		want string
	}{
		"a current pass":                         {passed, false, ""},
		"a pass before the signed format":        {ApprovalTally{Need: 1, Approved: 1, ApprovedBy: []string{"a"}}, false, PendingReapproval},
		"a pass without excluding the requester": {passed, true, PendingSoDReapproval},
		"a pass that excluded the requester":     {ApprovalTally{Need: 1, Approved: 1, ApprovedBy: []string{"a"}, ExcludePrincipal: true, DecisionFormat: DecisionFormat}, true, ""},
		"a denial":                               {ApprovalTally{Need: 1, Denied: 1, DeniedBy: []string{"a"}, DecisionFormat: DecisionFormat}, true, ""},
	} {
		if got := GateReask(tc.t, tc.sod); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

// A plan flow node's gate is read as the flow's own gate reads it (flowGate): only the decisions after
// its request count, and the node's result is the key's own record. EvaluateGateAfter, given the
// request's step, agrees with flowGate on every one of these, and EvaluateGate, which counts the whole
// journal, would not (the decision recorded before the request).
func TestEvaluateGateAfter_AgreesWithFlowGate(t *testing.T) {
	ctx := context.Background()
	vf := fakeVerifiers("a", "b")
	pol := &ApprovalPolicy{Need: 2, Approvers: []string{"a", "b"}}
	args := json.RawMessage(`{"amount":7}`)
	subj := ApprovalSubject{RunID: "r1", ToolUseID: "node:pay", ToolName: "pay", Args: args}
	decide := func(t *testing.T, j *Journal, id string, v bool) {
		t.Helper()
		if err := decideAs(ctx, j, "r1", "node:pay", id, v, fakeSign(id, ApprovalDecisionBytes(subj, id, v, testDecidedAt))); err != nil {
			t.Fatal(err)
		}
	}
	type result int
	const (
		pauses result = iota
		passes
		denies
	)
	for name, tc := range map[string]struct {
		setup func(t *testing.T, j *Journal)
		want  result
	}{
		"just paused":   {func(t *testing.T, j *Journal) {}, pauses},
		"one decision":  {func(t *testing.T, j *Journal) { decide(t, j, "a", true) }, pauses},
		"quorum":        {func(t *testing.T, j *Journal) { decide(t, j, "a", true); decide(t, j, "b", true) }, passes},
		"quorum denied": {func(t *testing.T, j *Journal) { decide(t, j, "a", false); decide(t, j, "b", true) }, denies},
		"both decisions are signed before the request": {func(t *testing.T, j *Journal) {}, pauses},
	} {
		t.Run(name, func(t *testing.T) {
			j := memJournal()
			if name == "both decisions are signed before the request" {
				// Decisions recorded under their names before the gate's request: they cannot count.
				for _, id := range []string{"a", "b"} {
					sig := fakeSign(id, ApprovalDecisionBytes(subj, id, true, testDecidedAt))
					writeRaw(t, j, "r1", approvalDecisionStep("node:pay", id, true, fakeAlg, FormatTimestamp(testDecidedAt), sig),
						Record{Kind: StepApproval, ToolUseID: "node:pay", Approved: true, ApproverSignature: &ApproverSignature{Approver: id, ApproverAlg: fakeAlg, Signature: sig, DecidedAt: FormatTimestamp(testDecidedAt)}})
				}
			}
			var pend *ApprovalPending
			if _, err := flowGate(ctx, j, "r1", "node:pay", "pay", args, pol, vf); !errors.As(err, &pend) {
				t.Fatalf("first evaluation = %v; want the pause", err)
			}
			tc.setup(t, j)
			recs, _ := j.History(ctx, "r1")
			st := EvaluateGateAfter(recs, subj, pol, vf, ApprovalRequestStep("node:pay"))
			whole := EvaluateGate(recs, subj, pol, vf)
			approved, err := flowGate(ctx, j, "r1", "node:pay", "pay", args, pol, vf)
			got := passes
			switch {
			case errors.As(err, &pend):
				got = pauses
			case err != nil:
				t.Fatalf("flowGate: %v", err)
			case !approved:
				got = denies
			}
			if got != tc.want {
				t.Fatalf("flowGate did %d, want %d", got, tc.want)
			}
			switch got {
			case pauses:
				if st.Status != GateWaiting || st.Tally.Passed() || st.Tally.Unreachable() {
					t.Fatalf("flowGate pauses, EvaluateGateAfter says %+v", st)
				}
			case passes:
				if st.Status != GateWaiting || !st.Tally.Passed() {
					t.Fatalf("flowGate passes, EvaluateGateAfter says %+v", st)
				}
			case denies:
				if st.Status != GateWaiting || !st.Tally.Unreachable() {
					t.Fatalf("flowGate denies, EvaluateGateAfter says %+v", st)
				}
			}
			// After the gate acts, its recorded outcome is read the same way, and the node's result is the
			// key's own record.
			recs, _ = j.History(ctx, "r1")
			if got != pauses {
				if st := EvaluateGateAfter(recs, subj, pol, vf, ApprovalRequestStep("node:pay")); st.Status != GateDecided || !st.Recorded {
					t.Fatalf("after the gate recorded its outcome: %+v", st)
				}
			}
			if name == "both decisions are signed before the request" && whole.Tally.Approved != 2 {
				t.Fatalf("setup: the whole-journal count should see both decisions, got %+v", whole.Tally)
			}
			writeRaw(t, j, "r1", "node:pay", Record{Kind: StepValue, Result: []byte(`"done"`)})
			recs, _ = j.History(ctx, "r1")
			if st := EvaluateGateAfter(recs, subj, pol, vf, ApprovalRequestStep("node:pay")); st.Status != GateDecided || !st.Ran {
				t.Fatalf("a node whose result is recorded: %+v", st)
			}
		})
	}
}
