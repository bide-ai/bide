package agent

import "time"

// The execution-time status re-check of a gate with certificate-bound approvers
// (docs/design/approver-identity.md, "Status is re-checked immediately before the call executes"). A
// terminal tally is never recounted, so without this a decision counted at 10:00 would still authorize an
// action at 11:30 after a 10:30 revocation. Every path that can run a tool passes one function, preExecute
// (exec_gate.go), which asks CheckExec about the decisions the authoritative tally counted.

// CertExecVerifier is implemented by the verifier of a certificate-bound approver that can be asked,
// immediately before the call it authorizes executes, whether a counted decision still holds.
// VerifyCertExec is the counting check again at the current time with status no older than the gate's
// execution ages (audit.CertApprover.VerifyCertExec). A certificate-bound verifier that does not implement
// it holds every call it authorized: the gate fails closed.
type CertExecVerifier interface {
	CertDecisionVerifier
	VerifyCertExec(cd CertDecision) CertCheck
}

// CertRevocation describes the revocation behind a CertCheck whose Reason is ReasonRevoked.
type CertRevocation struct {
	Serial     string // decimal serial of the revoked certificate
	E          string // its effective revocation time, RFC 3339 with nanoseconds
	ReasonCode int    // the CRL reason code
	CRL        string // SHA-256 (lowercase hex) of the CRL that lists it
}

// ExecOutcome is what the pre-execution check decided.
type ExecOutcome int

const (
	// ExecProceed: every counted certificate-bound decision still holds; the call may run.
	ExecProceed ExecOutcome = iota
	// ExecWait: status is missing or stale, or the decision cannot be proven now (no covering head at hand).
	// The call does not run and nothing authoritative is written: the gate pauses with a named reason, and
	// the same tally executes when a fresh check passes.
	ExecWait
	// ExecReask: a counted decision no longer holds and cannot hold again (its certificate is revoked with
	// E <= A(d), or it no longer verifies for a reason that is not a wait). The call does not run; the gate
	// writes an exec hold and asks its approvers again.
	ExecReask
)

// CountedDecision is one counted certificate-bound decision the execution check must re-verify.
type CountedDecision struct {
	Approver string
	Decision CertDecision
}

// ExecFailure is a counted decision that does not hold.
type ExecFailure struct {
	Approver   string
	Reason     string // a Reason constant
	Detail     string
	Revocation *CertRevocation // set when Reason is ReasonRevoked and the verifier says which
	Anchored   string          // A(d), RFC 3339 with nanoseconds, when known
	StatusTime string
}

// ExecVerdict is the result of CheckExec. Outcome is ExecReask if any decision must be held, else ExecWait
// if any waits, else ExecProceed. Failed lists every decision that does not hold, in the given order.
type ExecVerdict struct {
	Outcome ExecOutcome
	Failed  []ExecFailure
	Checked int // the number of decisions re-verified
}

// execWaits are the reasons a counted decision may hold again later: status that is missing or stale, or a
// decision no signed head covers (yet), or whose head times are contradicted. Any other reason is final.
func execWaits(reason string) bool {
	switch reason {
	case ReasonStatusUnknown, ReasonStatusStale, ReasonNoCoveringHead, ReasonHeadTimes:
		return true
	}
	return false
}

// CheckExec re-verifies every counted decision through its approver's CertExecVerifier. It is pure: it
// reads the decisions' journal copies and the verifiers, and writes nothing. An approver whose verifier is
// absent or cannot do the check waits (fails closed). With no decisions to check there is nothing to hold:
// the caller passes only certificate-bound decisions.
func CheckExec(counted []CountedDecision, verifierFor ApproverVerifierFor) ExecVerdict {
	v := ExecVerdict{Outcome: ExecProceed}
	for _, c := range counted {
		var chk CertCheck
		var ev CertExecVerifier
		if av, ok := verifierFor(c.Approver); ok {
			ev, _ = av.(CertExecVerifier)
		}
		if ev == nil {
			chk = CertCheck{Reason: ReasonStatusUnknown, Detail: "the approver's verifier cannot re-check a decision before execution"}
		} else {
			chk = ev.VerifyCertExec(c.Decision)
		}
		v.Checked++
		if chk.Reason == "" {
			continue
		}
		f := ExecFailure{Approver: c.Approver, Reason: chk.Reason, Detail: chk.Detail, Revocation: chk.Revocation, StatusTime: chk.StatusTime}
		if !chk.Anchored.IsZero() {
			f.Anchored = chk.Anchored.UTC().Format(time.RFC3339Nano)
		}
		v.Failed = append(v.Failed, f)
		switch {
		case !execWaits(chk.Reason):
			v.Outcome = ExecReask
		case v.Outcome == ExecProceed:
			v.Outcome = ExecWait
		}
	}
	return v
}
