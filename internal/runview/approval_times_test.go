package runview

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// TestCards_DecisionTimes: an approver card shows the decision's signed time and recorded time,
// the gap between them, and the sanity flags: a signed time more than the tolerance after the
// recorded one, and a signed time before the run asked. A recount's times, when the signatures
// were checked, take precedence over the journal's first decision.
func TestCards_DecisionTimes(t *testing.T) {
	g := Gate{ToolUseID: "c1", Need: 2, Approvers: []string{"ops", "finance", "risk"}, ApprovedBy: []string{"ops", "finance"}, DeniedBy: []string{"risk"},
		AskedAt: "2026-10-13T12:00:00.000Z",
		Decisions: []Decision{
			{Approver: "ops", Approved: true, Signed: true, DecidedAt: "2026-10-13T12:00:01.500Z", RecordedAt: "2026-10-13T12:00:04.000Z"},
			{Approver: "finance", Approved: true, Signed: true, DecidedAt: "2026-10-13T12:20:00.000Z", RecordedAt: "2026-10-13T12:01:00.000Z"},
			{Approver: "risk", Approved: true, Signed: true, DecidedAt: "2026-10-13T11:00:00.000Z", RecordedAt: "2026-10-13T12:02:00.000Z"},
			{Approver: "risk", Approved: false, Signed: true, DecidedAt: "2026-10-13T11:50:00.000Z", RecordedAt: "2026-10-13T12:03:00.000Z"},
		}}
	by := func(cs []ApproverCard) map[string]ApproverCard {
		out := map[string]ApproverCard{}
		for _, c := range cs {
			out[c.Name] = c
		}
		return out
	}
	cs := by(cards(g, nil))
	if c := cs["ops"]; c.DecidedAt != "2026-10-13 12:00:01.500" || c.RecordedAt != "2026-10-13 12:00:04.000" || c.Gap != "recorded 2.5s after the signed time" || len(c.Flags) != 0 {
		t.Errorf("ops = %+v", c)
	}
	if c := cs["finance"]; c.Gap != "recorded 19m0s before the signed time" || !slices.Equal(c.Flags, []string{audit.FlagDecidedAfterRecorded}) {
		t.Errorf("finance = %+v", c)
	}
	// risk's card is the denial (its state), not the earlier approval.
	if c := cs["risk"]; c.DecidedAt != "2026-10-13 11:50:00.000" || !slices.Equal(c.Flags, []string{audit.FlagDecidedBeforeAsked}) {
		t.Errorf("risk = %+v", c)
	}

	// With the signatures checked, the recount's times are shown.
	p := &Proof{Approvals: map[string]ApprovalCheck{"c1": {Counted: []string{"ops", "finance"}, Denied: []string{"risk"}, Times: map[string]audit.DecisionTime{
		"ops": {Approver: "ops", DecidedAt: "2026-10-13T12:00:02.000Z", RecordedAt: "2026-10-13T12:00:04.000Z"},
	}}}}
	if c := by(cards(g, p))["ops"]; c.DecidedAt != "2026-10-13 12:00:02.000" || c.Gap != "recorded 2s after the signed time" {
		t.Errorf("ops with the recount's times = %+v", c)
	}

	// A decision with no times (one an earlier version wrote) shows none; one not in canonical form
	// is shown as it is, marked.
	g.Decisions = []Decision{{Approver: "ops", Approved: true, Signed: true}, {Approver: "finance", Approved: true, Signed: true, DecidedAt: "2026-10-13T12:00:01Z"}}
	cs = by(cards(g, nil))
	if c := cs["ops"]; c.DecidedAt != "" || c.RecordedAt != "" || c.Gap != "" || len(c.Flags) != 0 {
		t.Errorf("ops with no times = %+v", c)
	}
	if c := cs["finance"]; c.DecidedAt != `"2026-10-13T12:00:01Z" (not a canonical time)` || c.Gap != "" {
		t.Errorf("finance with a non-canonical time = %+v", c)
	}
	if c := cs["risk"]; c.State != "denied" || c.DecidedAt != "" {
		t.Errorf("risk with no decision record = %+v", c)
	}
}

// TestCards_SeveralDecisionsNoTimes: without a signature check, an approver with several signed
// decisions of the card's outcome shows no time (which one counted is not known) and says why;
// with the check, the counted one's times are shown.
func TestCards_SeveralDecisionsNoTimes(t *testing.T) {
	g := Gate{ToolUseID: "c1", Need: 1, Approvers: []string{"ops"}, ApprovedBy: []string{"ops"}, AskedAt: "2026-10-13T12:00:00.000Z",
		Decisions: []Decision{
			{Approver: "ops", Approved: true, Signed: true, DecidedAt: "2026-10-13T12:00:30.000Z", RecordedAt: "2026-10-13T12:00:31.000Z"},
			{Approver: "ops", Approved: true, Signed: true, DecidedAt: "2026-10-13T12:00:35.000Z", RecordedAt: "2026-10-13T12:00:40.000Z"},
		}}
	c := cards(g, nil)[0]
	if c.DecidedAt != "" || c.RecordedAt != "" || len(c.Flags) != 0 || !strings.Contains(c.TimesNote, "2 signed decisions") {
		t.Fatalf("card = %+v", c)
	}
	p := &Proof{Approvals: map[string]ApprovalCheck{"c1": {Counted: []string{"ops"}, Times: map[string]audit.DecisionTime{
		"ops": {Approver: "ops", DecidedAt: "2026-10-13T12:00:35.000Z", RecordedAt: "2026-10-13T12:00:40.000Z"}}}}}
	if c := cards(g, p)[0]; c.DecidedAt != "2026-10-13 12:00:35.000" || c.TimesNote != "" {
		t.Fatalf("checked card = %+v", c)
	}
}

// TestGates_PreV3TallyWaits: a run whose gate passed on decisions that predate the signed decision
// format, and whose call has not run, shows as waiting for approval again, counting only decisions
// with a signed time; one whose call ran under the earlier version shows its tally as it was.
func TestGates_PreV3TallyWaits(t *testing.T) {
	old, _ := json.Marshal(agent.ApprovalTally{Need: 1, Approvers: []string{"ops"}, Approved: 1, ApprovedBy: []string{"ops"}})
	recs := []agent.Record{
		{Name: agent.ApprovalTallyStep("c1"), Kind: agent.StepValue, Result: old},
		{Name: "approval:c1:ops:v2", Kind: agent.StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &agent.ApproverSignature{Approver: "ops", ApproverAlg: "ed25519", Signature: []byte("v2")}},
	}
	g := gates(recs, map[string]agent.ToolUse{"c1": {ID: "c1", Name: "refund"}}, []string{"c1"})
	if len(g) != 1 || !g[0].Reapproval || !g[0].Waiting || g[0].Tallied || len(g[0].ApprovedBy) != 0 || !strings.HasPrefix(g[0].Status, "approval re-required") {
		t.Fatalf("gates = %+v", g)
	}
	recs = append(recs, agent.Record{Name: "tool:c1", Kind: agent.StepToolResult, ToolUseID: "c1", Result: []byte(`"ok"`)})
	g = gates(recs, map[string]agent.ToolUse{"c1": {ID: "c1", Name: "refund"}}, []string{"c1"})
	if len(g) != 1 || g[0].Reapproval || !g[0].Tallied || g[0].Waiting {
		t.Fatalf("gates after the call ran = %+v", g)
	}
}
