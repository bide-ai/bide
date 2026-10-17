package agent_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/journaltest"
)

type execStub struct {
	agent.CertDecisionVerifier
	check agent.CertCheck
	calls *int
}

func (s execStub) VerifyCertExec(agent.CertDecision) agent.CertCheck {
	*s.calls++
	return s.check
}

type plainVerifier struct{ agent.CertDecisionVerifier }

func TestCheckExec(t *testing.T) {
	var n int
	rev := &agent.CertRevocation{Serial: "7", E: "2026-10-13T12:00:00Z", ReasonCode: 1, CRL: "ab"}
	stubs := map[string]agent.ApproverVerifier{
		"ok":     execStub{calls: &n},
		"stale":  execStub{calls: &n, check: agent.CertCheck{Reason: agent.ReasonStatusStale}},
		"unk":    execStub{calls: &n, check: agent.CertCheck{Reason: agent.ReasonStatusUnknown}},
		"rev":    execStub{calls: &n, check: agent.CertCheck{Reason: agent.ReasonRevoked, Detail: "d", Revocation: rev, Anchored: time.Date(2026, 10, 13, 12, 14, 13, 0, time.UTC), StatusTime: "s"}},
		"nohead": execStub{calls: &n, check: agent.CertCheck{Reason: agent.ReasonNoCoveringHead}},
		"times":  execStub{calls: &n, check: agent.CertCheck{Reason: agent.ReasonHeadTimes}},
		"shared": execStub{calls: &n, check: agent.CertCheck{Reason: agent.ReasonSharedKey}},
		"sig":    execStub{calls: &n, check: agent.CertCheck{Reason: agent.ReasonBadSig}},
		"plain":  plainVerifier{},
	}
	vf := func(id string) (agent.ApproverVerifier, bool) { v, ok := stubs[id]; return v, ok }
	of := func(ids ...string) []agent.CountedDecision {
		var out []agent.CountedDecision
		for _, id := range ids {
			out = append(out, agent.CountedDecision{Approver: id})
		}
		return out
	}
	for _, tc := range []struct {
		name   string
		ids    []string
		want   agent.ExecOutcome
		failed []string
	}{
		{"none", nil, agent.ExecProceed, nil},
		{"all hold", []string{"ok", "ok"}, agent.ExecProceed, nil},
		{"stale waits", []string{"ok", "stale"}, agent.ExecWait, []string{"stale"}},
		{"unknown waits", []string{"unk"}, agent.ExecWait, []string{"unk"}},
		{"no covering head waits", []string{"nohead"}, agent.ExecWait, []string{"nohead"}},
		{"head times wait", []string{"times"}, agent.ExecWait, []string{"times"}},
		{"revoked asks again", []string{"ok", "rev"}, agent.ExecReask, []string{"rev"}},
		{"revoked outranks an earlier wait", []string{"stale", "rev"}, agent.ExecReask, []string{"stale", "rev"}},
		{"a shared key asks again", []string{"shared"}, agent.ExecReask, []string{"shared"}},
		{"a signature that no longer verifies asks again", []string{"sig"}, agent.ExecReask, []string{"sig"}},
		{"every failure is listed", []string{"nohead", "stale", "ok"}, agent.ExecWait, []string{"nohead", "stale"}},
		{"no exec verifier fails closed", []string{"plain"}, agent.ExecWait, []string{"plain"}},
		{"unknown approver fails closed", []string{"ghost"}, agent.ExecWait, []string{"ghost"}},
	} {
		n = 0
		got := agent.CheckExec(of(tc.ids...), vf)
		var failed []string
		for _, f := range got.Failed {
			failed = append(failed, f.Approver)
		}
		if got.Outcome != tc.want || len(failed) != len(tc.failed) || got.Checked != len(tc.ids) {
			t.Errorf("%s: %+v", tc.name, got)
			continue
		}
		for i := range failed {
			if failed[i] != tc.failed[i] {
				t.Errorf("%s: failed %v, want %v", tc.name, failed, tc.failed)
			}
		}
	}
	// The revocation travels with the failure, and A(d) in the gate's form.
	got := agent.CheckExec(of("rev"), vf)
	f := got.Failed[0]
	if f.Revocation == nil || f.Revocation.Serial != "7" || f.Anchored != "2026-10-13T12:14:13Z" || f.StatusTime != "s" || f.Detail != "d" {
		t.Fatalf("%+v", f)
	}
}

// A revocation found first is not downgraded to a wait by a stale status found later.
func TestCheckExec_AReaskIsNotDowngradedByALaterWait(t *testing.T) {
	var n int
	stubs := map[string]agent.ApproverVerifier{
		"rev":   execStub{calls: &n, check: agent.CertCheck{Reason: agent.ReasonRevoked}},
		"stale": execStub{calls: &n, check: agent.CertCheck{Reason: agent.ReasonStatusStale}},
	}
	vf := func(id string) (agent.ApproverVerifier, bool) { v, ok := stubs[id]; return v, ok }
	got := agent.CheckExec([]agent.CountedDecision{{Approver: "rev"}, {Approver: "stale"}}, vf)
	if got.Outcome != agent.ExecReask || len(got.Failed) != 2 {
		t.Fatalf("%+v", got)
	}
}

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

// keylessVerifier is an ApproverVerifier that reports no key identity (a verifier that checks
// through a service and does not expose its keys). Each value verifies only signatures made with
// its own secret, so two values are two different keys.
type keylessVerifier struct{ secret string }

func (keylessVerifier) Alg() agent.Alg   { return "svc" }
func (keylessVerifier) KeyIDs() []string { return nil }
func (v keylessVerifier) Verify(m, sig []byte) bool {
	return bytes.Equal(sig, append([]byte(v.secret+"|"), m...))
}

// L3 follow-up (re-review of #105). #105's counting rule keyed "one key counts once" on
// Alg()+PublicKey(), so two approvers with different keys whose verifiers exposed no key collided,
// while audit.VerifyApprovals took them as distinct: the gate and the audit disagreed on the same
// resolver. Under the one seat-per-key rule (#109, KeyIDs) a verifier that reports no key identity
// names no key, and both layers refuse it: the counting rule never counts such an approver
// (ReasonNoKeyID), and the gate refuses the policy with ErrConfig before the tool can run.
func Test_R105b_KeylessApproverIsRefused(t *testing.T) {
	s := agent.ApprovalSubject{RunID: "r", ToolUseID: "c", ToolName: "wire", Args: json.RawMessage(`{}`)}
	keys := map[string]keylessVerifier{"alice": {"a-secret"}, "bob": {"b-secret"}}
	resolve := func(id string) (agent.ApproverVerifier, bool) {
		v, ok := keys[id]
		return v, ok
	}
	var recs []agent.Record
	for _, id := range []string{"alice", "bob"} {
		sig := append([]byte(keys[id].secret+"|"), agent.ApprovalDecisionBytes(s, id, true, testDecidedAt)...)
		recs = append(recs, agent.Record{Name: "approval:c:" + id, Kind: agent.StepApproval, ToolUseID: "c", Approved: true, ApproverSignature: &agent.ApproverSignature{Approver: id, ApproverAlg: "svc", DecidedAt: agent.FormatTimestamp(testDecidedAt), Signature: sig}})
	}
	p := agent.ApprovalPolicy{Need: 1, Approvers: []string{"alice", "bob"}}
	got, checks := agent.TallyApprovals(recs, s, p, resolve)
	if got.Approved != 0 {
		t.Fatalf("the counting rule counted %d approvals from verifiers with no key identity, want 0 (checks %+v)", got.Approved, checks)
	}
	for _, c := range checks {
		if c.Counted || c.Reason != agent.ReasonNoKeyID {
			t.Fatalf("decision %s: counted %v, reason %q; want not counted, %q", c.Step, c.Counted, c.Reason, agent.ReasonNoKeyID)
		}
	}

	// New refuses the policy before any run, so the tool never runs.
	ran := 0
	wire := agent.MustFunc("wire", "", func(context.Context, struct{}) (string, error) { ran++; return "sent", nil }, agent.WithApproval(&p))
	a, err := agent.New(
		agenttest.NewScriptedModel(agenttest.ToolTurn("c", "wire", `{}`), agenttest.TextTurn("done")),
		agenttest.MemJournal(),
		agent.WithTools(wire), agent.WithApproverVerifiers(resolve))
	if !errors.Is(err, agent.ErrConfig) || a != nil || ran != 0 {
		t.Fatalf("a gate whose approvers resolve to verifiers with no key identity: New = %v, %v, tool ran %d times; want nil, ErrConfig and no run", a, err, ran)
	}
}

// keyedVerifier is an ApproverVerifier with a key of its own, verifying signatures made with it.
type keyedVerifier struct{ key string }

func (keyedVerifier) Alg() agent.Alg     { return "svc" }
func (v keyedVerifier) KeyIDs() []string { return []string{"svc:" + v.key} }
func (v keyedVerifier) Verify(m, sig []byte) bool {
	return bytes.Equal(sig, append([]byte(v.key+"|"), m...))
}

// Round-3 review of #105. The gate reuses a journaled tally once it exists, and read it with
// json.Unmarshal while audit.VerifyApprovals reads it strictly (UnmarshalStrict). A stored tally
// with "approved":0 and then a case variant "Approved":2 read as a passed 2-of-2 gate to the gate,
// which ran the tool, while the audit refused the same record as malformed. The gate must read it
// as the audit does: refuse it, and never run the tool.
func Test_R105c_GateReadsTheRecordedTallyStrictly(t *testing.T) {
	ctx := context.Background()
	p := agent.ApprovalPolicy{Need: 2, Approvers: []string{"alice", "bob"}}
	ran := 0
	wire := agent.MustFunc("wire", "", func(context.Context, struct{}) (string, error) { ran++; return "sent", nil }, agent.WithApproval(&p))
	resolve := func(id string) (agent.ApproverVerifier, bool) { return keyedVerifier{id + "-key"}, true }
	s := agenttest.MemJournal()
	newAgent := func() *agent.Agent {
		return agenttest.MustNew(
			agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "wire", `{}`), agenttest.TextTurn("done")),
			s,
			agent.WithTools(wire), agent.WithApproverVerifiers(resolve))
	}
	var pend *agent.ApprovalPending
	if _, err := newAgent().Run(ctx, "r", agent.UserText("hi")); !errors.As(err, &pend) {
		t.Fatalf("setup: first run err %v, want a pending approval", err)
	}
	salt := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, agent.SaltSize))
	tally := `{"name":"` + agent.ApprovalTallyStep("c1") + `","kind":"value","result":{"need":2,"approvers":["alice","bob"],"approved":0,"denied":0,"Approved":2},"salt":"` + salt + `"}`
	if _, _, err := s.Store().Insert(ctx, "r", agent.ApprovalTallyStep("c1"), []byte(tally)); err != nil {
		t.Fatal(err)
	}
	_, err := newAgent().Run(ctx, "r", agent.UserText("hi"))
	if err == nil || ran != 0 {
		t.Fatalf("a recorded tally that reads approved 0 to an exact-name reader and 2 to encoding/json: run err %v, tool ran %d times; want an error and no run", err, ran)
	}
}
