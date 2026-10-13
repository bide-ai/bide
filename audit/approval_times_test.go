package audit_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/journaltest"
)

// asked is when the timed gate's run pauses for approval, by its clock.
var asked = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

// decideAt records id's approval signed at decidedAt and journaled at recordedAt (WithClock).
func (g *gate) decideAt(t *testing.T, id string, decidedAt, recordedAt time.Time) {
	t.Helper()
	sig := ed25519.Sign(g.privs[id], agent.ApprovalDecisionBytes(g.subject(t), id, true, decidedAt))
	d := agent.Decision{RunID: gateRun, ToolUseID: "c1", ApproverID: id, Approved: true, DecidedAt: decidedAt, Alg: audit.AlgEd25519, Signature: sig}
	if err := agent.SubmitDecision(context.Background(), g.store, d, agent.WithClock(func() time.Time { return recordedAt })); err != nil {
		t.Fatalf("SubmitDecision %s: %v", id, err)
	}
}

// timedGate is a Need=3 of {alice, bob, carol} gate whose run pauses at asked. alice decides a
// second after being asked and is recorded two seconds later; bob's clock says bob decided ten
// minutes before being asked; carol's says carol decided twenty minutes after bide recorded it.
func timedGate(t *testing.T) *gate {
	t.Helper()
	g := newGate(3, []string{"alice", "bob", "carol"}, []string{"alice", "bob", "carol"})
	g.clock = func() time.Time { return asked }
	wantPaused(t, g.run(), 0)
	g.decideAt(t, "alice", asked.Add(time.Second), asked.Add(3*time.Second))
	g.decideAt(t, "bob", asked.Add(-10*time.Minute), asked.Add(5*time.Second))
	g.decideAt(t, "carol", asked.Add(20*time.Minute), asked.Add(6*time.Second))
	if err := g.run(); err != nil || g.charged != 1 {
		t.Fatalf("run at quorum: %v (charged %d)", err, g.charged)
	}
	return g
}

// TestApprovalTimes_Verdict: the verdict reports each counted decision's signed and recorded times
// and when the run asked, flags the clock that runs ahead and the decision dated before the ask,
// and still passes: the flags are not a verdict.
func TestApprovalTimes_Verdict(t *testing.T) {
	g := timedGate(t)
	acts, err := audit.ApprovalEvidence(context.Background(), g.store, gateRun, "c1", g.sth(t, 1700000000))
	if err != nil {
		t.Fatal(err)
	}
	v, err := audit.VerifyApprovals(acts, "c1", g.policy, g.resolver(), edV(g.logPub))
	if err != nil || !v.OK {
		t.Fatalf("VerifyApprovals = %+v, %v; want OK whatever the flags", v, err)
	}
	if v.AskedAt != "2026-10-13T12:00:00.000Z" {
		t.Fatalf("AskedAt = %q, want the run's pause time", v.AskedAt)
	}
	want := []audit.DecisionTime{
		{Approver: "alice", DecidedAt: "2026-10-13T12:00:01.000Z", RecordedAt: "2026-10-13T12:00:03.000Z"},
		{Approver: "bob", DecidedAt: "2026-10-13T11:50:00.000Z", RecordedAt: "2026-10-13T12:00:05.000Z", Flags: []string{audit.FlagDecidedBeforeAsked}},
		{Approver: "carol", DecidedAt: "2026-10-13T12:20:00.000Z", RecordedAt: "2026-10-13T12:00:06.000Z", Flags: []string{audit.FlagDecidedAfterRecorded}},
	}
	if len(v.Times) != len(want) {
		t.Fatalf("Times = %+v, want %+v", v.Times, want)
	}
	for i, w := range want {
		got := v.Times[i]
		if got.Approver != w.Approver || got.DecidedAt != w.DecidedAt || got.RecordedAt != w.RecordedAt || !slices.Equal(got.Flags, w.Flags) || got.Step == "" {
			t.Fatalf("Times[%d] = %+v, want %+v", i, got, w)
		}
	}
	if gap, ok := v.Times[0].Gap(); !ok || gap != 2*time.Second {
		t.Fatalf("alice's gap = %v, %v; want 2s", gap, ok)
	}
	if gap, ok := v.Times[2].Gap(); !ok || gap != -(19*time.Minute+54*time.Second) {
		t.Fatalf("carol's gap = %v, %v; want -19m54s", gap, ok)
	}
	// A wider tolerance clears carol's flag; bob's does not depend on the tolerance.
	wide := audit.FlagDecisionTimes(v.Times, v.AskedAt, 30*time.Minute)
	if len(wide[2].Flags) != 0 || !slices.Equal(wide[1].Flags, []string{audit.FlagDecidedBeforeAsked}) || len(v.Times[2].Flags) != 1 {
		t.Fatalf("FlagDecisionTimes at 30m = %+v (input now %+v)", wide, v.Times)
	}
	// The tolerance is exclusive: a signed time exactly the tolerance ahead is not flagged.
	edge := []audit.DecisionTime{{DecidedAt: "2026-10-13T12:05:00.000Z", RecordedAt: "2026-10-13T12:00:00.000Z"}}
	if f := audit.FlagDecisionTimes(edge, "", audit.DefaultDecisionTimeTolerance); len(f[0].Flags) != 0 {
		t.Fatalf("exactly 5m ahead flagged: %+v", f)
	}
	edge[0].DecidedAt = "2026-10-13T12:05:00.001Z"
	if f := audit.FlagDecisionTimes(edge, "", audit.DefaultDecisionTimeTolerance); !slices.Equal(f[0].Flags, []string{audit.FlagDecidedAfterRecorded}) {
		t.Fatalf("5m and 1ms ahead not flagged: %+v", f)
	}
	// Without the pause record the decided-before-asked check has nothing to compare with.
	var noPending []audit.EvidenceAction
	for _, a := range acts {
		if a.Ref != agent.ApprovalPendingStep("c1") {
			noPending = append(noPending, a)
		}
	}
	v2, err := audit.VerifyApprovals(noPending, "c1", g.policy, g.resolver(), edV(g.logPub))
	if err != nil || !v2.OK || v2.AskedAt != "" || len(v2.Times[1].Flags) != 0 {
		t.Fatalf("without the pause record: %+v, %v", v2, err)
	}
}

// TestApprovalTimes_TamperedInEvidence: a decision's time changed in the evidence no longer matches
// the record the log committed to, so the decision is not proven and the verdict fails.
func TestApprovalTimes_TamperedInEvidence(t *testing.T) {
	g := timedGate(t)
	acts, err := audit.ApprovalEvidence(context.Background(), g.store, gateRun, "c1", g.sth(t, 1700000000))
	if err != nil {
		t.Fatal(err)
	}
	edited := slices.Clone(acts)
	for i, a := range acts {
		if a.Kind != audit.KindApproval || a.Label != "alice" {
			continue
		}
		rec := string(a.Bundle.RecordBytes)
		if !strings.Contains(rec, `"decided_at":"2026-10-13T12:00:01.000Z"`) {
			t.Fatalf("alice's record %s does not hold her decision time", rec)
		}
		edited[i].Bundle.RecordBytes = []byte(strings.Replace(rec, "12:00:01.000Z", "11:59:01.000Z", 1))
	}
	v, err := audit.VerifyApprovals(edited, "c1", g.policy, g.resolver(), edV(g.logPub))
	if !errors.Is(err, audit.ErrNotVerified) || v.OK || !hasProblem(v, "omits, or cannot prove") {
		t.Fatalf("evidence with alice's time changed: %+v, %v; want ErrNotVerified", v, err)
	}
}

// TestApprovalTimes_TamperedInJournal: a decision journaled with another time than its approver
// signed (a writer that changed it before the log committed to it) does not verify, at the gate or
// offline, and an old decision with no signed time is refused as such; the others still count.
func TestApprovalTimes_TamperedInJournal(t *testing.T) {
	ctx := context.Background()
	g := newGate(2, []string{"alice", "bob", "carol"}, []string{"alice", "bob", "carol"})
	g.clock = func() time.Time { return asked }
	wantPaused(t, g.run(), 0)
	// carol signs at one time; the journal records another.
	sig := ed25519.Sign(g.privs["carol"], agent.ApprovalDecisionBytes(g.subject(t), "carol", true, asked.Add(time.Minute)))
	if err := agent.SubmitDecision(ctx, g.store, agent.Decision{RunID: gateRun, ToolUseID: "c1", ApproverID: "carol", Approved: true,
		DecidedAt: asked.Add(2 * time.Minute), Alg: audit.AlgEd25519, Signature: sig}); err != nil {
		t.Fatal(err)
	}
	// bob's decision as the previous version wrote it: no signed time.
	old := agent.Record{Kind: agent.StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &agent.ApproverSignature{
		Approver: "bob", ApproverAlg: audit.AlgEd25519, Signature: ed25519.Sign(g.privs["bob"], []byte("bide.approval.v2\n"))}}
	if _, err := journaltest.Put(ctx, g.store, gateRun, "approval:c1:bob:old", old); err != nil {
		t.Fatal(err)
	}
	wantPaused(t, g.run(), 0)
	g.decideAt(t, "alice", asked.Add(time.Second), asked.Add(2*time.Second))
	g.decideAt(t, "bob", asked.Add(time.Second), asked.Add(2*time.Second))
	if err := g.run(); err != nil || g.charged != 1 {
		t.Fatalf("run at quorum: %v (charged %d)", err, g.charged)
	}
	acts, err := audit.ApprovalEvidence(ctx, g.store, gateRun, "c1", g.sth(t, 1700000000))
	if err != nil {
		t.Fatal(err)
	}
	v, err := audit.VerifyApprovals(acts, "c1", g.policy, g.resolver(), edV(g.logPub))
	if err != nil || !v.OK || !slices.Equal(v.Counted, []string{"alice", "bob"}) {
		t.Fatalf("VerifyApprovals = %+v, %v; want OK by alice and bob", v, err)
	}
	if r := ignoredReason(v, "carol"); r != agent.ReasonBadSig {
		t.Fatalf("carol's retimed decision: reason %q, want %q", r, agent.ReasonBadSig)
	}
	if r := ignoredReason(v, "bob"); r != agent.ReasonNoDecidedAt {
		t.Fatalf("bob's old decision: reason %q, want %q", r, agent.ReasonNoDecidedAt)
	}
	for _, dt := range v.Times {
		if dt.Approver == "carol" {
			t.Fatalf("an uncounted decision's time is reported: %+v", dt)
		}
	}
}
