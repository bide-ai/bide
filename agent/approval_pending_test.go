package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/journaltest"
)

func pendingRecords(t *testing.T, j *agent.Journal, runID string) []agent.Record {
	t.Helper()
	recs, err := j.History(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var out []agent.Record
	for _, r := range recs {
		if r.Name == agent.ApprovalPendingStep("c1") {
			out = append(out, r)
		}
	}
	return out
}

// TestApprovalPendingRecord: a run that pauses at a gate records once that it waits (with the
// gate's k-of-n and approvers), a resume that pauses again writes nothing, and the run finishes
// as before once approved.
func TestApprovalPendingRecord(t *testing.T) {
	ctx := context.Background()
	for _, pol := range []*agent.ApprovalPolicy{agent.SingleApproval(), {Need: 2, Approvers: []string{"ops", "finance", "risk"}}} {
		cs := agenttest.NewCountingStore(agent.NewMemStore())
		j := agenttest.MustJournal(cs)
		ran := 0
		tool := agent.MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { ran++; return "ok", nil }, agent.WithApproval(pol))
		newAgent := func() *agent.Agent {
			m := agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{}`), agenttest.TextTurn("done"))
			return agenttest.MustNew(m, j, agent.WithTools(tool), agent.WithApproverVerifiers(func(string) (agent.ApproverVerifier, bool) { return nil, false }))
		}
		if _, err := newAgent().Run(ctx, "r", agent.UserText("go")); !isApprovalPending(err) {
			t.Fatalf("Run = %v, want ApprovalPending", err)
		}
		recs := pendingRecords(t, j, "r")
		if len(recs) != 1 || recs[0].Kind != agent.StepValue {
			t.Fatalf("approval-pending records = %+v, want one StepValue", recs)
		}
		var w agent.ApprovalWait
		if err := json.Unmarshal(recs[0].Result, &w); err != nil {
			t.Fatal(err)
		}
		want := agent.ApprovalWait{ToolUseID: "c1", ToolName: "refund", Single: true}
		if pol.Need == 2 {
			want = agent.ApprovalWait{ToolUseID: "c1", ToolName: "refund", Need: 2, Approvers: []string{"ops", "finance", "risk"}}
		}
		if w.ToolUseID != want.ToolUseID || w.ToolName != want.ToolName || w.Single != want.Single || w.Need != want.Need || len(w.Approvers) != len(want.Approvers) {
			t.Fatalf("ApprovalWait = %+v, want %+v", w, want)
		}

		cs.Reset()
		if _, err := newAgent().Run(ctx, "r", agent.UserText("go")); !isApprovalPending(err) {
			t.Fatalf("resumed Run = %v, want ApprovalPending", err)
		}
		for _, op := range cs.Counts().Names {
			if op == "insert "+agent.ApprovalPendingStep("c1") {
				t.Fatalf("a resume that pauses again wrote the approval-pending record again: %v", cs.Counts().Names)
			}
		}
		if pol.Need == 2 {
			continue // decisions on an m-of-n gate are signed; the single gate shows the run going on
		}
		if err := agent.Approve(ctx, j, "r", "c1", true); err != nil {
			t.Fatal(err)
		}
		if _, err := newAgent().Run(ctx, "r", agent.UserText("go")); err != nil {
			t.Fatalf("Run after Approve: %v", err)
		}
		if ran != 1 || len(pendingRecords(t, j, "r")) != 1 {
			t.Fatalf("tool ran %d times, approval-pending records %d; want 1 and 1", ran, len(pendingRecords(t, j, "r")))
		}
	}
}

// TestApprovalPendingIsInformational: nothing reads the record to decide. A record already there,
// even one that does not decode, leaves the gate's pause and its outcome as they are.
func TestApprovalPendingIsInformational(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	if _, err := journaltest.Put(ctx, j, "r", agent.ApprovalPendingStep("c1"), agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`"not an ApprovalWait"`)}); err != nil {
		t.Fatal(err)
	}
	ran := 0
	tool := agent.MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { ran++; return "ok", nil }, agent.WithApproval(agent.SingleApproval()))
	newAgent := func() *agent.Agent {
		m := agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{}`), agenttest.TextTurn("done"))
		return agenttest.MustNew(m, j, agent.WithTools(tool))
	}
	if _, err := newAgent().Run(ctx, "r", agent.UserText("go")); !isApprovalPending(err) {
		t.Fatalf("Run = %v, want ApprovalPending", err)
	}
	if err := agent.Approve(ctx, j, "r", "c1", false); err != nil {
		t.Fatal(err)
	}
	if _, err := newAgent().Run(ctx, "r", agent.UserText("go")); err != nil {
		t.Fatalf("Run after a denial: %v", err)
	}
	if ran != 0 {
		t.Fatalf("a denied call ran %d times", ran)
	}
}

func isApprovalPending(err error) bool {
	_, ok := errors.AsType[*agent.ApprovalPending](err)
	return ok
}

// TestApprovalPendingRecord_RecordedAt: the record of a pause carries when the run paused, by the
// run's clock (WithClock), in canonical form; a resume that pauses again keeps the first time.
func TestApprovalPendingRecord_RecordedAt(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	now := time.Date(2026, 10, 13, 12, 0, 0, 7_654_321, time.UTC)
	clock := func() time.Time { return now }
	tool := agent.MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { return "ok", nil },
		agent.WithApproval(&agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}))
	newAgent := func() *agent.Agent {
		m := agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{}`), agenttest.TextTurn("done"))
		return agenttest.MustNew(m, j, agent.WithTools(tool), agent.WithClock(clock),
			agent.WithApproverVerifiers(func(string) (agent.ApproverVerifier, bool) { return nil, false }))
	}
	for range 2 {
		if _, err := newAgent().Run(ctx, "r", agent.UserText("go")); !isApprovalPending(err) {
			t.Fatalf("Run = %v, want ApprovalPending", err)
		}
		recs := pendingRecords(t, j, "r")
		if len(recs) != 1 {
			t.Fatalf("approval-pending records = %d, want 1", len(recs))
		}
		var w agent.ApprovalWait
		if err := json.Unmarshal(recs[0].Result, &w); err != nil {
			t.Fatal(err)
		}
		if w.RecordedAt != "2026-10-13T12:00:00.007Z" {
			t.Fatalf("ApprovalWait.RecordedAt = %q, want the run clock's first reading", w.RecordedAt)
		}
		now = now.Add(time.Hour)
	}
}
