package runview

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
)

// gateOf builds run's view from the sample journal after mutate has written to it, and returns its
// gate on call.
func gateOf(t *testing.T, run, call string, mutate func(j *agent.Journal)) Gate {
	t.Helper()
	_, j, _ := demoHandler(t, "")
	if mutate != nil {
		mutate(j)
	}
	recs, err := j.History(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range Build(context.Background(), run, recs, nil).Gates {
		if g.ToolUseID == call {
			return g
		}
	}
	t.Fatalf("no gate %s in %s", call, run)
	return Gate{}
}

// pausedGate pauses a run at a 2-of-3 gate and returns its journal.
func pausedGate(t *testing.T) *agent.Journal {
	t.Helper()
	j := agenttest.MemJournal()
	policy := agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}}
	refund := agent.MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithApproval(&policy))
	verify := func(string) (agent.ApproverVerifier, bool) { return nil, false }
	m := agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{}`), agenttest.TextTurn("done"))
	if _, err := agenttest.MustNew(m, j, agent.WithTools(refund), agent.WithApproverVerifiers(verify)).Run(context.Background(), "r1", agent.UserText("go")); err == nil {
		t.Fatal("no pause")
	}
	return j
}

// A stray unsigned agent.Approve record, the first on an m-of-n call, is ignored by the runtime; it
// must not turn the gate into a one-decision gate, nor let the viewer say "approved (single decision)".
func TestGate_StrayUnsignedApproveDoesNotMakeItSingle(t *testing.T) {
	j := pausedGate(t)
	if err := agent.Approve(context.Background(), j, "r1", "c1", true); err != nil {
		t.Fatal(err)
	}
	recs, _ := j.History(context.Background(), "r1")
	var g Gate
	for _, x := range Build(context.Background(), "r1", recs, nil).Gates {
		g = x
	}
	if g.Single || strings.Contains(g.Status, "single") || strings.HasPrefix(g.Status, "approved") {
		t.Fatalf("gate = single %v, status %q", g.Single, g.Status)
	}
	if !g.Waiting {
		t.Fatalf("an m-of-n gate that still needs signed approvals must wait: %+v", g)
	}
}

// Decisions counted from the records, with no tally of the gate's own, are never called an approval:
// two junk signatures must not show "approved (2 of 2...)".
func TestGate_UnverifiedRecordsAreNotCalledApproved(t *testing.T) {
	g := gateOf(t, "refund-1043", "c2", func(j *agent.Journal) {
		for _, id := range []string{"finance", "risk"} {
			if err := agent.SubmitDecision(context.Background(), j, agent.Decision{RunID: "refund-1043", ToolUseID: "c2", ApproverID: id, Approved: true,
				DecidedAt: time.Now(), Alg: audit.AlgEd25519, Signature: bytes.Repeat([]byte{7}, 64)}); err != nil {
				t.Fatal(err)
			}
		}
	})
	if strings.HasPrefix(g.Status, "approved") || strings.HasPrefix(g.Status, "denied") || !strings.Contains(g.Status, "unverified") {
		t.Fatalf("status %q", g.Status)
	}
	// A denial counted the same way is not called a denial either.
	g = gateOf(t, "refund-1043", "c2", func(j *agent.Journal) {
		for _, id := range []string{"finance", "risk"} {
			_ = agent.SubmitDecision(context.Background(), j, agent.Decision{RunID: "refund-1043", ToolUseID: "c2", ApproverID: id, Approved: false,
				DecidedAt: time.Now(), Alg: audit.AlgEd25519, Signature: bytes.Repeat([]byte{7}, 64)})
		}
	})
	if strings.HasPrefix(g.Status, "denied") || !strings.Contains(g.Status, "unverified") {
		t.Fatalf("status %q", g.Status)
	}
}

// A real single-approver gate still reads as one: from the pause note, with its decision.
func TestGate_RealSingleGateStillSingle(t *testing.T) {
	_, j, runs := demoHandler(t, "")
	for _, r := range runs {
		recs, _ := j.History(context.Background(), r.ID)
		for _, g := range Build(context.Background(), r.ID, recs, nil).Gates {
			if g.Single && !strings.Contains(g.Status, "single") && !strings.Contains(g.Status, "one decision") {
				t.Errorf("%s %s: single gate with status %q", r.ID, g.ToolUseID, g.Status)
			}
		}
	}
}
