package audit

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
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
	// RequesterIdentity, when set, maps a requester principal (as the run records it) to the canonical
	// identifier of the person, so separation of duties can exclude the requester by identifier. Without
	// it the roster is used: a principal equal (ASCII case folded) to an approver id of the identity map
	// maps to that approver's identifier, which only repeats the exclusion by approver id: a bank that
	// wants a requester who holds no approver id (a login that maps to a person) excluded by identity
	// must supply this function. A requester it cannot map is then excluded by approver id only, and
	// the finding carries a warning; without the function no warning is made.
	RequesterIdentity func(principal string) (identifier string, ok bool)
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
	if r.Approver() != c.ApproverID {
		// One CertApprover serves one approver id: a record of another id would be checked against this
		// id's identity.
		return fail(agent.ReasonNotEligible, "the decision is by approver %q, not the %q this verifier serves", r.Approver(), c.ApproverID)
	}
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
	a, source, _, hwarn, why, err := c.provableAt(cd)
	if err != nil {
		return agent.CertCheck{Reason: why, Detail: err.Error(), Warnings: hwarn}
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

	out := agent.CertCheck{Identity: ac.Identity, KeyHash: ac.KeyHash, Anchored: a, Time: source, Warnings: hwarn}
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
	if len(cs.Certs) == 0 {
		out.Reason, out.Detail = agent.ReasonStatusUnknown, "the status rule returned no finding for the chain"
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
// earlier with one slow clock). warnings say that a head of this run, signed by the log key, contradicts
// the journal given (it is not used, and it is evidence the receipt checker would report).
//
// The journal's leaf hashes are computed once, and each head's prefix root is taken over them, so the
// cost per decision is one pass over the journal plus one pass of node hashing per head, not a
// re-encoding of the journal prefix per head.
func (c *CertApprover) provableAt(cd agent.CertDecision) (a time.Time, source string, head SignedTreeHead, warnings []string, why string, err error) {
	none := SignedTreeHead{}
	if cd.Index < 0 || cd.Index >= len(cd.Journal) || c.Heads == nil {
		return time.Time{}, "", none, nil, agent.ReasonNoCoveringHead, errors.New("the decision is not in the journal the verifier was given, or no signed head of the run is known")
	}
	runID := cd.Subject.RunID
	heads, herr := c.Heads(runID)
	if herr != nil {
		return time.Time{}, "", none, nil, agent.ReasonNoCoveringHead, fmt.Errorf("heads of run %s: %v", runID, herr)
	}
	leaves, lerr := journalLeafHashes(cd.Journal)
	var good []SignedTreeHead
	diverged := 0
	for _, h := range heads {
		if h.Kind != TreeJournal || h.RunID != runID || c.LogKey == nil || h.Verify(c.LogKey) != nil {
			continue // not a head of this run's journal tree under the log key
		}
		if h.Size < 0 || h.Size > len(cd.Journal) {
			continue // longer than the journal given: says nothing about it
		}
		if lerr != nil || !bytes.Equal(hashRoot(leaves[:h.Size]), h.Root) {
			diverged++
			continue
		}
		good = append(good, h)
	}
	if diverged > 0 {
		warnings = append(warnings, fmt.Sprintf("%d signed head(s) of this run contradict the journal given and were not used", diverged))
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
		return time.Time{}, "", none, warnings, agent.ReasonNoCoveringHead, fmt.Errorf("no signed head of the run reaches position %d", cd.Index)
	}
	tol := c.ClockTolerance
	if tol <= 0 {
		tol = time.Minute
	}
	for i := range good {
		for j := i + 1; j < len(good); j++ {
			if good[j].Size > good[i].Size && good[j].TimestampNanos < good[i].TimestampNanos-tol.Nanoseconds() {
				if i == cover || j == cover {
					return time.Time{}, "", none, warnings, agent.ReasonHeadTimes, fmt.Errorf("the head of size %d is stamped %v after the larger head of size %d", good[i].Size,
						time.Duration(good[i].TimestampNanos-good[j].TimestampNanos), good[j].Size)
				}
			}
		}
	}
	h := good[cover]
	return time.Unix(0, h.TimestampNanos).UTC(), "log signer clock: head of size " + fmt.Sprint(h.Size) + " root " + hex.EncodeToString(h.Root[:min(8, len(h.Root))]), h, warnings, "", nil
}

// HasIdentity implements agent.IdentityRoster: whether the policy's identity map names the approver id.
func (c *CertApprover) HasIdentity(approverID string) bool {
	if c == nil || c.Policy == nil {
		return false
	}
	_, ok := c.Policy.Identity(approverID)
	return ok
}

// RequesterIdentifier implements agent.IdentityRoster: RequesterIdentity if set; else the principal
// canonicalized as an identifier of the policy's kind, if it is one; else the roster's identifier of
// the approver id the principal equals.
func (c *CertApprover) RequesterIdentifier(principal string) (string, bool) {
	if c == nil {
		return "", false
	}
	if c.RequesterIdentity != nil {
		return c.RequesterIdentity(principal)
	}
	if c.Policy == nil {
		return "", false
	}
	// A principal that is itself an identifier of the policy's kind (an opaque person URI, say) maps to
	// its canonical form: both sides of the comparison are canonicalized by the policy's own function.
	if canon, err := c.Policy.canon.Canonical(c.Policy.kind, principal); err == nil {
		return canon, true
	}
	for id, ident := range c.Policy.identity {
		if agent.SameApprover(id, principal) {
			return ident, true
		}
	}
	return "", false
}

// IdentityMappingConfigured implements agent.IdentityRoster: whether RequesterIdentity is set.
func (c *CertApprover) IdentityMappingConfigured() bool {
	return c != nil && c.RequesterIdentity != nil
}

// RosterDigest implements agent.IdentityRoster: the SHA-256 over the whole identity map (bare-key
// approvers included) in sorted approver order, each entry len32(approver) approver len32(identifier)
// identifier, so an id or identifier containing '=' or a newline cannot collide with another entry.
func (c *CertApprover) RosterDigest() string {
	h := sha256.New()
	if c != nil && c.Policy != nil {
		for _, id := range slices.Sorted(maps.Keys(c.Policy.identity)) {
			ident := c.Policy.identity[id]
			h.Write(binary.BigEndian.AppendUint32(nil, uint32(len(id))))
			h.Write([]byte(id))
			h.Write(binary.BigEndian.AppendUint32(nil, uint32(len(ident))))
			h.Write([]byte(ident))
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}
