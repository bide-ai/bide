package audit

import (
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/bide-ai/bide/agent"
)

// CertApprover is the verifier of a certificate-bound approver (docs/design/approver-identity.md): it
// implements agent.CertDecisionVerifier, so the counting rule (agent.TallyApprovals) checks the
// approver's decisions as certificate-bound ones.
//
// A decision counts when all of these hold: it carries a chain and is signed under bide.approval.v4
// (never v3: a bare signature is not a certificate-bound decision); a signed tree head of the run
// covers it, which fixes A(d), the time it became provable; the chain is accepted for this approver at
// A(d) (CertIdentity.VerifyChain); the signature verifies under the leaf's key over the v4 bytes of
// exactly this call, which bind the leaf's hash; and no certificate of the chain was revoked with an
// effective time at or before A(d) (CertIdentity.EvaluateStatus), with status that exists and is
// fresh enough for the counting mode. The finding carries the verified identifier, the tier and basis
// of the status, and what A(d) rests on, for the verdict and the identity record.
type CertApprover struct {
	// ApproverID is the approver id this verifier serves: the key of the policy's identity map.
	ApproverID string
	// Policy is the compiled certificate policy of the gate.
	Policy *CertIdentity
	// Heads returns the signed tree heads of the run known to the caller (the anchor log's list, not
	// the producer's). A(d) is the time of the smallest head that covers the decision. Nil means no
	// head is known and no decision is provable.
	Heads func(runID string) ([]SignedTreeHead, error)
	// LogKey verifies the heads. Nil verifies none.
	LogKey Verifier
	// ClockTolerance is how far a larger head may be stamped before a smaller one without the heads
	// contradicting each other (default one minute).
	ClockTolerance time.Duration
	// Status holds the CRLs behind the revocation check. Nil means no status: no decision counts.
	Status *StatusSnapshot
	// StatusParams are the status rule's parameters (the window, the declared CA period, holds...). For a
	// live gate set CountLive and MaxStatusAge, and Now supplies the clock for each check; an offline
	// audit leaves Now nil and the rule judges the series against the decision.
	StatusParams StatusParams
	// Now, when set, is read for each decision to set StatusParams.Now.
	Now func() time.Time
}

// Alg reports agent.AlgCertificate: the scheme is the leaf key's, known only from the certificate.
func (*CertApprover) Alg() agent.Alg { return agent.AlgCertificate }

// Verify is always false: a bare signature over bytes, with no certificate, is never a
// certificate-bound approver's decision.
func (*CertApprover) Verify(message, sig []byte) bool { return false }

// KeyIDs is the approver's seat, "ident:" and the canonical identifier the policy's identity map gives
// the approver id, so two approver ids of one person fill one seat. An id the policy does not name
// reports none, and the gate refuses it.
func (c *CertApprover) KeyIDs() []string {
	if c == nil || c.Policy == nil {
		return nil
	}
	id, ok := c.Policy.Identity(c.ApproverID)
	if !ok {
		return nil
	}
	return []string{"ident:" + id}
}

// VerifyCertDecision implements agent.CertDecisionVerifier.
func (c *CertApprover) VerifyCertDecision(cd agent.CertDecision) agent.CertCheck {
	fail := func(reason, format string, args ...any) agent.CertCheck {
		return agent.CertCheck{Reason: reason, Detail: fmt.Sprintf(format, args...)}
	}
	if c == nil || c.Policy == nil {
		return fail(agent.ReasonNoKey, "the certificate-bound verifier has no policy")
	}
	r := cd.Record
	if len(r.ApproverCert()) == 0 {
		return fail(agent.ReasonNoCert, "the decision carries no certificate chain; a certificate-bound approver signs bide.approval.v4 with its chain")
	}
	if r.DecidedAt() == "" {
		return fail(agent.ReasonNoDecidedAt, "the decision has no signed time")
	}
	decidedAt, err := agent.ParseTimestamp(r.DecidedAt())
	if err != nil {
		return fail(agent.ReasonDecidedAt, "decided_at: %v", err)
	}
	a, source, why, err := c.provableAt(cd)
	if err != nil {
		return agent.CertCheck{Reason: why, Detail: err.Error()}
	}
	ac, err := c.Policy.VerifyChain(c.ApproverID, r.ApproverCert(), decidedAt, a)
	if err != nil {
		var ce *CertError
		if errors.As(err, &ce) {
			return agent.CertCheck{Reason: ce.Reason, Detail: ce.Detail, Anchored: a, Time: source}
		}
		return agent.CertCheck{Reason: agent.ReasonCertChain, Detail: err.Error(), Anchored: a, Time: source}
	}
	if r.ApproverAlg() != ac.Scheme {
		return agent.CertCheck{Reason: agent.ReasonAlg, Anchored: a, Time: source,
			Detail: fmt.Sprintf("signed under %q, the certificate's key is %q", r.ApproverAlg(), ac.Scheme)}
	}
	v, err := NewVerifier(ac.Scheme, ac.PublicKey)
	if err != nil {
		return agent.CertCheck{Reason: agent.ReasonCertUsage, Detail: "the certificate's key: " + err.Error(), Anchored: a, Time: source}
	}
	if !v.Verify(agent.ApprovalDecisionBytesCert(cd.Subject, r.Approver(), r.Approved, decidedAt, ac.LeafHash), r.Signature()) {
		return agent.CertCheck{Reason: agent.ReasonBadSig, Detail: "the signature does not verify over the bide.approval.v4 bytes of this call under the certificate's key", Anchored: a, Time: source}
	}

	out := agent.CertCheck{Identity: ac.Identity, KeyHash: ac.KeyHash, Anchored: a, Time: source}
	if c.Status == nil {
		out.Reason, out.Detail = agent.ReasonStatusUnknown, "the verifier has no revocation status"
		return out
	}
	p := c.StatusParams
	if c.Now != nil {
		p.Now = c.Now()
	}
	cs := c.Policy.EvaluateStatus(ac, a, c.Status, p)
	for _, f := range cs.Certs {
		if f.Warning != "" {
			out.Warnings = append(out.Warnings, f.Warning)
		}
	}
	if err := cs.Err(); err != nil {
		var se *StatusError
		if errors.As(err, &se) {
			out.Reason, out.Detail = se.Reason, se.Detail
		} else {
			out.Reason, out.Detail = agent.ReasonStatusUnknown, err.Error()
		}
		return out
	}
	// The weakest certificate's finding states the decision's: its tier, what it rests on, and S.
	weakest := cs.Certs[0]
	for _, f := range cs.Certs[1:] {
		if f.Tier < weakest.Tier {
			weakest = f
		}
	}
	out.Tier, out.Basis = weakest.Tier.String(), weakest.Basis
	if !weakest.S.IsZero() {
		out.StatusTime = weakest.S.UTC().Format(time.RFC3339)
	}
	return out
}

// provableAt finds A(d): the timestamp of the smallest signed head of the run that covers the decision
// record (a head larger than its index), among heads that verify under the log key and match the
// journal. why is the Reason when it cannot be found, or when that head's time is contradicted by
// another head: a larger head stamped earlier than a smaller one, beyond the tolerance, implicates
// both, and a decision whose A(d) would come from either is not provable (the operator cannot pull A(d)
// earlier with one slow clock).
func (c *CertApprover) provableAt(cd agent.CertDecision) (a time.Time, source, why string, err error) {
	if cd.Index < 0 || cd.Index >= len(cd.Journal) || c.Heads == nil {
		return time.Time{}, "", agent.ReasonNoCoveringHead, errors.New("the decision is not in the journal the verifier was given, or no signed head of the run is known")
	}
	runID := cd.Subject.RunID
	var heads []SignedTreeHead
	if c.Heads != nil {
		if heads, err = c.Heads(runID); err != nil {
			return time.Time{}, "", agent.ReasonNoCoveringHead, fmt.Errorf("heads of run %s: %v", runID, err)
		}
	}
	var good []SignedTreeHead
	for _, h := range heads {
		if class, _, _ := classifyHead(h, runID, c.LogKey, cd.Journal); class == headMatches {
			good = append(good, h)
		}
	}
	sort.SliceStable(good, func(i, j int) bool {
		if good[i].Size != good[j].Size {
			return good[i].Size < good[j].Size
		}
		return good[i].TimestampNanos < good[j].TimestampNanos
	})
	cover := -1
	for i, h := range good {
		if h.Size > cd.Index {
			cover = i
			break
		}
	}
	if cover < 0 {
		return time.Time{}, "", agent.ReasonNoCoveringHead, fmt.Errorf("no signed head of the run reaches position %d", cd.Index)
	}
	tol := c.ClockTolerance
	if tol <= 0 {
		tol = time.Minute
	}
	for i := range good {
		for j := i + 1; j < len(good); j++ {
			if good[j].Size > good[i].Size && good[j].TimestampNanos < good[i].TimestampNanos-tol.Nanoseconds() {
				if i == cover || j == cover {
					return time.Time{}, "", agent.ReasonHeadTimes, fmt.Errorf("the head of size %d is stamped %v after the larger head of size %d", good[i].Size,
						time.Duration(good[i].TimestampNanos-good[j].TimestampNanos), good[j].Size)
				}
			}
		}
	}
	h := good[cover]
	return time.Unix(0, h.TimestampNanos).UTC(), "log signer clock: head of size " + fmt.Sprint(h.Size) + " root " + hex.EncodeToString(h.Root[:min(8, len(h.Root))]), "", nil
}
