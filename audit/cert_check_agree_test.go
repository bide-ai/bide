package audit_test

import (
	"reflect"
	"slices"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// CertApprover.CheckCertificate and the verifier the gate runs (VerifyCertDecision, VerifyCertExec) must
// give one verdict for the chain of a decision. agreeing wraps the verifier the harness hands the gate, so
// every decision any identity test counts, refuses or holds is also put to CheckCertificate, with the
// provable time the verifier found, and the two must agree on everything but what CheckCertificate does not
// look at (the signature, the key's scheme and the covering head).
type agreeing struct {
	*audit.CertApprover
	t *testing.T
}

func (a agreeing) VerifyCertDecision(cd agent.CertDecision) agent.CertCheck {
	a.t.Helper()
	got := a.CertApprover.VerifyCertDecision(cd)
	assertCheckAgrees(a.t, a.CertApprover, cd, audit.CheckCount, got)
	return got
}

func (a agreeing) VerifyCertExec(cd agent.CertDecision) agent.CertCheck {
	a.t.Helper()
	got := a.CertApprover.VerifyCertExec(cd)
	assertCheckAgrees(a.t, a.CertApprover, cd, audit.CheckExec, got)
	return got
}

// verifyExec is VerifyCertExec with the agreement check, for the tests that call the verifier directly.
func verifyExec(t *testing.T, a *audit.CertApprover, cd agent.CertDecision) agent.CertCheck {
	t.Helper()
	return agreeing{a, t}.VerifyCertExec(cd)
}

func assertCheckAgrees(t *testing.T, a *audit.CertApprover, cd agent.CertDecision, mode audit.CheckMode, got agent.CertCheck) {
	t.Helper()
	if got.Anchored.IsZero() {
		return // no provable time was found (or the decision has no chain): CheckCertificate has no time to judge at
	}
	decidedAt, err := agent.ParseTimestamp(cd.Record.DecidedAt())
	if err != nil {
		return
	}
	chk := a.CheckCertificate(cd.Record.ApproverCert(), decidedAt, got.Anchored, mode)
	switch got.Reason {
	case agent.ReasonAlg, agent.ReasonCertUsage, agent.ReasonBadSig:
		// ReasonCertUsage is also a chain verdict (the EKU), which both report alike and which is compared
		// below. Otherwise only the signature half failed: the chain passed, so CheckCertificate must have
		// got as far as the identity.
		if chk.Reason == got.Reason && chk.Detail == got.Detail {
			break
		}
		if chk.Identity == "" {
			t.Fatalf("verifier refused on %q, CheckCertificate reports %q (%s)", got.Reason, chk.Reason, chk.Detail)
		}
		return
	}
	// The head and the time source are not in CheckCertificate's view, and the verifier's head warnings
	// come first.
	g, c := got, chk
	g.HeadSize, g.HeadRoot, g.HeadHash, g.Time = 0, "", "", ""
	if n := len(g.Warnings) - len(c.Warnings); n >= 0 && slices.Equal(g.Warnings[n:], c.Warnings) {
		g.Warnings, c.Warnings = nil, nil
	}
	gv, cv := reflect.ValueOf(g), reflect.ValueOf(c)
	for i := range gv.NumField() {
		if !reflect.DeepEqual(gv.Field(i).Interface(), cv.Field(i).Interface()) {
			t.Fatalf("mode %d: the gate's verifier and CheckCertificate disagree on %s (reason %q): verifier %v, check %v",
				mode, gv.Type().Field(i).Name, got.Reason, gv.Field(i).Interface(), cv.Field(i).Interface())
		}
	}
}
