package agent

import (
	"encoding/json"
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
