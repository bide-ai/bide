package agent_test

import (
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
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
