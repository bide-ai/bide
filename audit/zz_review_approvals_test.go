package audit_test

// Adversarial review of #244 (bide.approval.v3, decision times). Each test here reproduces a
// finding and fails on the reviewed branch; see the review report for the ranking and fixes.

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/journaltest"
)

// TestReview_FarFutureDecidedAtNotFlagged: a decided_at more than about 292 years after
// recorded_at is not flagged as a clock running ahead. Gap is recorded.Sub(decided), which
// saturates at math.MinInt64 nanoseconds; FlagDecisionTimes negates it, the negation overflows
// back to math.MinInt64, and "-gap > tolerance" is false. The CLI and the viewer print the same
// overflowed gap ("-2562047h47m16.854775808s before the signed time").
func TestReview_FarFutureDecidedAtNotFlagged(t *testing.T) {
	for _, decided := range []string{"2400-01-01T00:00:00.000Z", "9999-12-31T23:59:59.999Z"} {
		in := []audit.DecisionTime{{Approver: "bob", DecidedAt: decided, RecordedAt: "2026-10-13T12:00:05.000Z"}}
		got := audit.FlagDecisionTimes(in, "2026-10-13T12:00:00.000Z", audit.DefaultDecisionTimeTolerance)
		if !slices.Contains(got[0].Flags, audit.FlagDecidedAfterRecorded) {
			gap, _ := got[0].Gap()
			t.Errorf("decided_at %s, recorded_at 2026-10-13T12:00:05.000Z: flags %q (gap %v); want %q",
				decided, got[0].Flags, gap, audit.FlagDecidedAfterRecorded)
		}
	}

	// End to end: bob signs a decision dated in the year 9999. It counts (the time never changes
	// the count, as designed) but the verdict carries no flag on it.
	g := newGate(2, []string{"alice", "bob"}, []string{"alice", "bob"})
	g.clock = func() time.Time { return asked }
	wantPaused(t, g.run(), 0)
	g.decideAt(t, "alice", asked.Add(time.Second), asked.Add(2*time.Second))
	g.decideAt(t, "bob", time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), asked.Add(3*time.Second))
	if err := g.run(); err != nil || g.charged != 1 {
		t.Fatalf("run at quorum: %v (charged %d)", err, g.charged)
	}
	acts, err := audit.ApprovalEvidence(context.Background(), g.store, gateRun, "c1", g.sth(t, 1700000000))
	if err != nil {
		t.Fatal(err)
	}
	v, err := audit.VerifyApprovals(acts, "c1", g.policy, g.resolver(), edV(g.logPub))
	if err != nil || !v.OK {
		t.Fatalf("VerifyApprovals = %+v, %v", v, err)
	}
	for _, d := range v.Times {
		if d.Approver == "bob" && !slices.Contains(d.Flags, audit.FlagDecidedAfterRecorded) {
			t.Errorf("bob decided in 9999 and was recorded in 2026, but the verdict does not flag it: %+v", d)
		}
	}
}

// TestReview_NonCanonicalRecordedAtUnflagged: recorded_at is not signed and nothing checks its
// form. A decision journaled with a recorded_at that is not a time at all (here, text with a line
// break) still counts, and the verdict reports it in Times verbatim with no flag and no problem,
// while decided_at in the same position is held to the one canonical form. A reader (the CLI
// prints it raw, see the auditcli review test) cannot tell that bide's own time is garbage.
func TestReview_NonCanonicalRecordedAtUnflagged(t *testing.T) {
	ctx := context.Background()
	g := newGate(2, []string{"alice", "bob"}, []string{"alice", "bob"})
	g.clock = func() time.Time { return asked }
	wantPaused(t, g.run(), 0)
	decided := asked.Add(time.Second)
	sig := ed25519.Sign(g.privs["alice"], agent.ApprovalDecisionBytes(g.subject(t), "alice", true, decided))
	bogus := "yesterday\nPASS: 2 of 2 required approvals verified"
	rec := agent.Record{Kind: agent.StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &agent.ApproverSignature{
		Approver: "alice", ApproverAlg: audit.AlgEd25519, Signature: sig, DecidedAt: agent.FormatTimestamp(decided), RecordedAt: bogus}}
	if _, err := journaltest.Put(ctx, g.store, gateRun, "approval:c1:alice:review", rec); err != nil {
		t.Fatal(err)
	}
	g.decideAt(t, "bob", asked.Add(time.Second), asked.Add(2*time.Second))
	if err := g.run(); err != nil || g.charged != 1 {
		t.Fatalf("run at quorum: %v (charged %d)", err, g.charged)
	}
	acts, err := audit.ApprovalEvidence(ctx, g.store, gateRun, "c1", g.sth(t, 1700000000))
	if err != nil {
		t.Fatal(err)
	}
	v, err := audit.VerifyApprovals(acts, "c1", g.policy, g.resolver(), edV(g.logPub))
	if err != nil || !v.OK {
		t.Fatalf("VerifyApprovals = %+v, %v", v, err)
	}
	for _, d := range v.Times {
		if d.Approver != "alice" {
			continue
		}
		if _, perr := agent.ParseTimestamp(d.RecordedAt); perr != nil && len(d.Flags) == 0 && len(v.Problems) == 0 {
			t.Errorf("alice's recorded_at %q is not a canonical time, yet it is reported with no flag and no problem: %+v", d.RecordedAt, d)
		}
	}
}

// TestReview_PreV3TallyReusedButUnverifiable: a gate that passed under the previous decision
// format (v2 decisions, no decided_at) and crashed before its tool ran is resumed by this version.
// The gate reuses the journaled tally without recounting, so the tool runs; the offline verifier
// recounts under v3, ignores every decision (ReasonNoDecidedAt) and fails the same gate. The
// runtime and the auditor disagree about one recorded outcome. v2 decisions are thus still honored
// at recovery, through the recorded tally, while every other path refuses them.
func TestReview_PreV3TallyReusedButUnverifiable(t *testing.T) {
	ctx := context.Background()
	g := newGate(2, []string{"alice", "bob"}, []string{"alice", "bob"})
	wantPaused(t, g.run(), 0)
	var names []string
	for _, id := range []string{"alice", "bob"} {
		name := "approval:c1:" + id + ":v2"
		old := agent.Record{Kind: agent.StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &agent.ApproverSignature{
			Approver: id, ApproverAlg: audit.AlgEd25519, Signature: ed25519.Sign(g.privs[id], []byte("bide.approval.v2\n"+id))}}
		if _, err := journaltest.Put(ctx, g.store, gateRun, name, old); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	tally := agent.ApprovalTally{Need: 2, Approvers: []string{"alice", "bob"}, Approved: 2, ApprovedBy: []string{"alice", "bob"}, Records: names}
	b, err := json.Marshal(tally)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journaltest.Put(ctx, g.store, gateRun, agent.ApprovalTallyStep("c1"), agent.Record{Kind: agent.StepValue, Result: b}); err != nil {
		t.Fatal(err)
	}
	runErr := g.run()
	acts, err := audit.ApprovalEvidence(ctx, g.store, gateRun, "c1", g.sth(t, 1700000000))
	if err != nil {
		t.Fatalf("ApprovalEvidence (run: %v, charged %d): %v", runErr, g.charged, err)
	}
	v, verr := audit.VerifyApprovals(acts, "c1", g.policy, g.resolver(), edV(g.logPub))
	if (g.charged == 1) != v.OK {
		t.Fatalf("the gate ran the tool %d time(s) on a pre-v3 tally, but offline verification says OK=%v (%v; ignored %+v, problems %q)",
			g.charged, v.OK, verr, v.Ignored, v.Problems)
	}
}

// FuzzReview_ProofDecisionRecord: record bytes a proof carries that the strict decoder accepts
// decode to one decision: the same fields as agent.DecodeRecord, and counting them never panics.
func FuzzReview_ProofDecisionRecord(f *testing.F) {
	seed, _ := json.Marshal(agent.Record{Kind: agent.StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &agent.ApproverSignature{
		Approver: "ops", ApproverAlg: audit.AlgEd25519, Signature: make([]byte, 64), DecidedAt: "2026-10-13T12:14:03.123Z", RecordedAt: "2026-10-13T12:14:04.000Z"}})
	f.Add(seed)
	f.Add([]byte(`{"kind":"approval","tool_use_id":"c1","approved":true,"approver_signature":{"approver":"ops","decided_at":"2026-10-13T12:14:03.123Z","Decided_At":"2026-10-13T12:14:03.124Z"}}`))
	pub, _, _ := ed25519.GenerateKey(nil)
	s := agent.ApprovalSubject{RunID: "r1", ToolUseID: "c1", ToolName: "charge", Args: json.RawMessage(`{}`)}
	p := agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}
	vf := func(string) (agent.ApproverVerifier, bool) { return audit.Ed25519Verifier{Pub: pub}, true }
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := audit.ProofBundle{RecordBytes: b}.Record()
		if err != nil {
			return
		}
		loose, err := agent.DecodeRecord(b)
		if err != nil {
			t.Fatalf("strict decode accepted %q, agent.DecodeRecord refused it: %v", b, err)
		}
		if loose.DecidedAt() != r.DecidedAt() || loose.RecordedAt() != r.RecordedAt() || loose.Approver() != r.Approver() || loose.Approved != r.Approved {
			t.Fatalf("two decoders disagree on %q", b)
		}
		agent.TallyApprovals([]agent.Record{r}, s, p, vf)
	})
}
