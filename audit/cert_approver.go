package audit

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
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
	// ExecNow, when set, is the clock of the execution-time check (VerifyCertExec); otherwise Now, and
	// when that is nil too, the wall clock. It exists so a test or a deterministic simulation can move the
	// execution check in time apart from the count.
	ExecNow func() time.Time
	// RequesterIdentity, when set, maps a requester principal (as the run records it) to the canonical
	// identifier of the person, so separation of duties can exclude the requester by identifier. Without
	// it the roster is used: a principal equal (ASCII case folded) to an approver id of the identity map
	// maps to that approver's identifier, which only repeats the exclusion by approver id: a bank that
	// wants a requester who holds no approver id (a login that maps to a person) excluded by identity
	// must supply this function. A requester it cannot map is then excluded by approver id only, and
	// the finding carries a warning; without the function no warning is made.
	RequesterIdentity func(principal string) (identifier string, ok bool)
	// ExecStatusAge is how old the newest CRL of the leaf's issuing CA may be when the call is about to
	// run (the status check before execution). ValidateConfig requires that CA's period to be at most
	// half of it. It is set from the CA's actual period; there is no default.
	ExecStatusAge time.Duration
	// ExecStatusAgeFor is the same age for each CA tier above the leaf's issuing CA, by issuer key: the
	// status of an intermediate rests on its issuer's CRL, which an offline root publishes rarely. Every
	// CA in an accepted chain above the leaf's issuer must be named here (the execution check holds the
	// call otherwise), and ValidateConfig requires, for each: CAPeriodFor + CAOverlapFor <=
	// ExecStatusAgeFor <= StatusParams.MaxStatusAgeFor. A CA's tier is its position in the accepted
	// chain, never its key: the leaf's issuing CA always uses ExecStatusAge.
	ExecStatusAgeFor map[SPKIHash]time.Duration
	// CAOverlapFor is, for a CA tier, how long after its period ends the CA may still be publishing the
	// next CRL (an offline root's ceremony slack, AD CS's CRL overlap); the oldest status a gate can hold
	// for that CA is CAPeriodFor plus this. Zero is allowed.
	CAOverlapFor map[SPKIHash]time.Duration
	// PolicyEffectiveFrom is when the certificate policy took effect (the bank's change record), kept in
	// the identity record so the mapping at A(d) is knowable years later. ValidateConfig requires it.
	PolicyEffectiveFrom time.Time
	// StatusArchive receives the bytes of a CRL above agent.MaxStatusInline, which the journal records
	// by hash only. Without one the gate refuses to count on such a CRL.
	StatusArchive StatusArchive
	// GoDebug is the x509 settings of GODEBUG the process runs under (they change how certificates
	// verify), as the operator DECLARES them: the engine does not read the environment, so this is a
	// declaration, recorded as such in the identity record. Generate it from the real process settings and
	// check it at startup with CheckDeclaredGoDebug(declared, os.Getenv("GODEBUG")).
	GoDebug string
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

// protocol:identity begin Count

// VerifyCertDecision implements agent.CertDecisionVerifier.
func (c *CertApprover) VerifyCertDecision(cd agent.CertDecision) agent.CertCheck {
	return c.verify(cd, modeCount)
}

// VerifyCertExec implements agent.CertExecVerifier: the check a gate runs on a counted decision
// immediately before the call it authorizes executes. It is the counting check again at the current time
// with the execution ages instead of MaxStatusAge: the newest status of the leaf's issuing CA must be no
// older than ExecStatusAge, and that of each CA tier above it no older than its ExecStatusAgeFor (an
// undeclared tier holds the call). The clock is Now or, when Now is nil, the wall clock: an execution
// check never runs without one (a check that judged the series against the decision would accept any
// old CRL), and status issued before the decision counts, as in a live count. A decision whose
// certificate now shows a revocation with E <= A(d), or whose status is stale or unknown, fails with the
// same Reason constants as at count time. ValidateConfig checks the ages at configuration time.
func (c *CertApprover) VerifyCertExec(cd agent.CertDecision) agent.CertCheck {
	return c.verify(cd, modeExec)
}

// VerifyCertIntake implements agent.CertIntakeVerifier: the check a decision passes before it is written
// (agent.WithDecisionCheck, so the approvals intake and `bide approve`). It is the counting check at the
// time of the write, with no covering head to find (there is none yet; the write is what a head will
// cover): the chain is accepted for the approver at the current time, the signature verifies over the
// bide.approval.v4 bytes of this call under the leaf's key, and the status of every certificate of the
// chain is usable now by the execution ages and shows no revocation with E <= now. A decision that fails
// for its chain, identity, scheme or signature, or that is revoked, is invalid (the intake answers 422);
// one that fails only for status that is missing or stale returns agent.ReasonStatusUnknown or
// ReasonStatusStale (the intake answers 503 and writes nothing: the decision is not refused, the gate
// cannot judge it yet). The clock is ExecNow, else Now, else the wall clock.
func (c *CertApprover) VerifyCertIntake(cd agent.CertDecision) agent.CertCheck {
	return c.verify(cd, modeIntake)
}

// verifyMode selects what verify judges a decision against.
type verifyMode int

const (
	modeCount  verifyMode = iota // at the time the covering head fixes, with MaxStatusAge
	modeExec                     // immediately before the call executes, with the execution ages
	modeIntake                   // at the time of the write, with the execution ages and no covering head
)

func (c *CertApprover) verify(cd agent.CertDecision, mode verifyMode) agent.CertCheck {
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
	var (
		a      time.Time
		source string
		head   SignedTreeHead
		hwarn  []string
		why    string
	)
	if mode == modeIntake {
		// No head covers a decision before it is written: it is judged at the time of the check, which the
		// head that follows the write will be at or after.
		a, source = c.execClock().UTC(), "the time of the intake check"
	} else {
		a, source, head, hwarn, why, err = c.provableAt(cd)
	}
	if err != nil {
		if why == agent.ReasonNoCoveringHead {
			// "Not provable yet" makes the gate anchor, which signs a head with the log key. Report it
			// only for a decision that verifies if the signed time were the provable time, so a junk
			// decision gets its own reason and never causes a head to be signed.
			if pre := c.verifies(cd, decidedAt); pre.Reason != "" {
				return pre
			}
		}
		return agent.CertCheck{Reason: why, Detail: err.Error(), Warnings: hwarn}
	}
	seed := agent.CertCheck{Anchored: a, Time: source, Warnings: hwarn, HeadSize: head.Size, HeadRoot: hex.EncodeToString(head.Root)}
	if mode != modeIntake {
		hh := head.Hash()
		seed.HeadHash = hex.EncodeToString(hh[:])
	}
	return c.checkCertificate(r.ApproverCert(), decidedAt, a, mode, seed, func(ac *ApproverCert) *agent.CertCheck {
		if r.ApproverAlg() != ac.Scheme {
			return &agent.CertCheck{Reason: agent.ReasonAlg, Anchored: a, Time: source,
				Detail: fmt.Sprintf("signed under %q, the certificate's key is %q", r.ApproverAlg(), ac.Scheme)}
		}
		v, err := NewVerifier(ac.Scheme, ac.PublicKey)
		if err != nil {
			return &agent.CertCheck{Reason: agent.ReasonCertUsage, Detail: "the certificate's key: " + err.Error(), Anchored: a, Time: source}
		}
		if !v.Verify(agent.ApprovalDecisionBytesCert(cd.Subject, r.Approver(), r.Approved, decidedAt, ac.LeafHash), r.Signature()) {
			return &agent.CertCheck{Reason: agent.ReasonBadSig, Detail: "the signature does not verify over the bide.approval.v4 bytes of this call under the certificate's key", Anchored: a, Time: source}
		}
		return nil
	})
}

// CheckMode is the counting rule a CheckCertificate verdict is judged by.
type CheckMode int

const (
	// CheckCount judges as the gate does when it counts a decision: status no older than MaxStatusAge
	// (and MaxStatusAgeFor).
	CheckCount CheckMode = iota
	// CheckExec judges as the gate does immediately before the call runs: ExecStatusAge for the leaf's
	// issuing CA and ExecStatusAgeFor for each CA tier above it.
	CheckExec
)

// CheckCertificate is the part of the verifier that needs no journal, signature or signed head: whether
// the chain is accepted for this approver (CertIdentity.VerifyChain) and what the status rule makes of
// it, for a decision signed at decidedAt that became provable at anchoredAt, judged by the count rule or
// the pre-execution rule. The verdict (Reason empty when it would count) is what VerifyCertDecision or
// VerifyCertExec reports for a decision with that chain, signed correctly, and that time: both call the
// same function. The clock for the status rule is Now (CheckCount) or ExecNow, then Now (CheckExec).
// The signature, the key's scheme and the covering head are not checked; a caller says so.
func (c *CertApprover) CheckCertificate(chain [][]byte, decidedAt, anchoredAt time.Time, mode CheckMode) agent.CertCheck {
	m := modeCount
	if mode == CheckExec {
		m = modeExec
	}
	if c == nil || c.Policy == nil {
		return agent.CertCheck{Reason: agent.ReasonNoKey, Detail: "the certificate-bound verifier has no policy"}
	}
	return c.checkCertificate(chain, decidedAt, anchoredAt, m, agent.CertCheck{Anchored: anchoredAt}, nil)
}

// checkCertificate is the verdict on a chain from VerifyChain on: the chain, then (when given) the
// signature checks, then the status rule. seed carries what the caller already knows (the time source,
// the covering head, head warnings); a failure before the status rule reports only Anchored and Time.
func (c *CertApprover) checkCertificate(chain [][]byte, decidedAt, a time.Time, mode verifyMode, seed agent.CertCheck, sig func(*ApproverCert) *agent.CertCheck) agent.CertCheck {
	exec := mode != modeCount
	source := seed.Time
	ac, err := c.Policy.VerifyChain(c.ApproverID, chain, decidedAt, a)
	if err != nil {
		var ce *CertError
		if errors.As(err, &ce) {
			return agent.CertCheck{Reason: ce.Reason, Detail: ce.Detail, Anchored: a, Time: source}
		}
		return agent.CertCheck{Reason: agent.ReasonCertChain, Detail: err.Error(), Anchored: a, Time: source}
	}
	if sig != nil {
		if fail := sig(ac); fail != nil {
			return *fail
		}
	}

	out := seed
	out.Identity, out.KeyHash, out.Anchor = ac.Identity, ac.KeyHash, hex.EncodeToString(ac.Anchor[:])
	for _, x := range ac.Chain {
		sum := sha256.Sum256(x.Raw)
		out.Chain = append(out.Chain, hex.EncodeToString(sum[:]))
	}
	if c.Status == nil {
		out.Reason, out.Detail = agent.ReasonStatusUnknown, "the verifier has no revocation status"
		return out
	}
	p := c.StatusParams
	if exec && c.ExecNow != nil {
		p.Now = c.ExecNow()
	} else if c.Now != nil {
		p.Now = c.Now()
	}
	if mode == modeIntake {
		p.Now = a
	}
	var perCert func(int, *x509.Certificate, StatusParams) StatusParams
	if exec {
		var reason, detail string
		if perCert, reason, detail = c.execTiers(ac, &p); reason != "" {
			out.Reason, out.Detail = reason, detail
			return out
		}
	}
	cs := c.Policy.evaluateStatus(ac, a, c.Status, p, perCert)
	for _, f := range cs.Certs {
		if f.Warning != "" {
			out.Warnings = append(out.Warnings, f.Warning)
		}
	}
	for _, f := range cs.Certs {
		if f.Revoked {
			out.Revocation = &agent.CertRevocation{Serial: f.Serial.String(), E: f.E.UTC().Format(time.RFC3339Nano), ReasonCode: f.ReasonCode, CRL: hex.EncodeToString(f.RevokedBy[:])}
			break
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
	seen := map[[32]byte]bool{}
	for i, f := range cs.Certs {
		for _, h := range f.Used {
			if seen[h] || i+1 >= len(ac.Chain) {
				continue
			}
			seen[h] = true
			if ev, ok := c.Status.evidence(h, SPKIHashOf(ac.Chain[i+1])); ok {
				out.Evidence = append(out.Evidence, ev)
			}
		}
	}
	// The weakest certificate's finding states the decision's: its tier, what it rests on, and S.
	weakest := cs.Certs[0]
	for _, f := range cs.Certs[1:] {
		if f.Tier < weakest.Tier {
			weakest = f
		}
	}
	out.Tier, out.Basis, out.Window = weakest.Tier.String(), weakest.Basis, weakest.Window.String()
	out.BasisKind = agent.BasisAtOrAfterDecision
	if strings.HasPrefix(weakest.Basis, "live count") {
		out.BasisKind = agent.BasisLiveBeforeDecision
	}
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

// protocol:identity end

// verifies checks the decision's chain, identifier, scheme and signature taking at as the time it became
// provable, and returns the failure, or an empty finding when it verifies. It says nothing of status.
func (c *CertApprover) verifies(cd agent.CertDecision, at time.Time) agent.CertCheck {
	r := cd.Record
	if len(r.ApproverCert()) == 0 {
		return agent.CertCheck{Reason: agent.ReasonNoCert, Detail: "the decision carries no certificate chain"}
	}
	ac, err := c.Policy.VerifyChain(c.ApproverID, r.ApproverCert(), at, at)
	if err != nil {
		var ce *CertError
		if errors.As(err, &ce) {
			return agent.CertCheck{Reason: ce.Reason, Detail: ce.Detail}
		}
		return agent.CertCheck{Reason: agent.ReasonCertChain, Detail: err.Error()}
	}
	if r.ApproverAlg() != ac.Scheme {
		return agent.CertCheck{Reason: agent.ReasonAlg, Detail: fmt.Sprintf("signed under %q, the certificate's key is %q", r.ApproverAlg(), ac.Scheme)}
	}
	v, err := NewVerifier(ac.Scheme, ac.PublicKey)
	if err != nil {
		return agent.CertCheck{Reason: agent.ReasonCertUsage, Detail: "the certificate's key: " + err.Error()}
	}
	at2, err := agent.ParseTimestamp(r.DecidedAt())
	if err != nil || !v.Verify(agent.ApprovalDecisionBytesCert(cd.Subject, r.Approver(), r.Approved, at2, ac.LeafHash), r.Signature()) {
		return agent.CertCheck{Reason: agent.ReasonBadSig, Detail: "the signature does not verify over the bide.approval.v4 bytes of this call under the certificate's key"}
	}
	return agent.CertCheck{}
}

// HasIdentity implements agent.IdentityRoster: whether the policy's identity map names the approver id.
func (c *CertApprover) HasIdentity(approverID string) bool {
	if c == nil || c.Policy == nil {
		return false
	}
	_, ok := c.Policy.Identity(approverID)
	return ok
}

// Identifier implements agent.IdentityRoster: the canonical identifier the policy gives the approver id.
func (c *CertApprover) Identifier(approverID string) (string, bool) {
	if c == nil || c.Policy == nil {
		return "", false
	}
	return c.Policy.Identity(approverID)
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

// execClock is the clock of the execution-time and intake checks: ExecNow, else Now, else the wall clock.
func (c *CertApprover) execClock() time.Time {
	switch {
	case c.ExecNow != nil:
		return c.ExecNow()
	case c.Now != nil:
		return c.Now()
	}
	return time.Now()
}
