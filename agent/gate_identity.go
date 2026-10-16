package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

// The gate of a policy with a certificate-bound approver (docs/design/approver-identity.md): it
// anchors a decision no signed head covers, says why it waits, and journals the identity environment
// of its terminal count. Nothing here changes how a decision is counted: that is the verifier's
// (CertDecisionVerifier), and a policy of bare-key approvers does none of it.

// ReanchorMinInterval is the least time between two forced heads of one run: anchoring signs with the
// log key, which may be an HSM or KMS key with quotas and cost, so the gate rate-limits it.
const ReanchorMinInterval = 10 * time.Second

// reanchorTimeout bounds one forced head.
const reanchorTimeout = 30 * time.Second

var reanchors struct {
	mu   sync.Mutex
	last map[reanchorKey]time.Time // run -> when the gate last asked to anchor it, in this process
}

// reanchorAllowed reports whether the gate may force a head of runID at now, and records that it did.
type reanchorKey struct {
	store any // the journal's store identity, so two stores with one run id are two runs
	run   string
}

func reanchorAllowed(store any, runID string, now time.Time) bool {
	key := reanchorKey{store, runID}
	reanchors.mu.Lock()
	defer reanchors.mu.Unlock()
	if reanchors.last == nil {
		reanchors.last = map[reanchorKey]time.Time{}
	}
	for k, t := range reanchors.last { // forget the runs that are past the interval, so the map stays small
		if now.Sub(t) >= ReanchorMinInterval || t.Sub(now) >= ReanchorMinInterval {
			delete(reanchors.last, k)
		}
	}
	if _, ok := reanchors.last[key]; ok {
		return false // asked less than ReanchorMinInterval ago (older entries were forgotten above)
	}
	reanchors.last[key] = now
	return true
}

// identityApprovers returns the approvers of pol whose verifier is a certificate-bound one, in policy
// order, and the first such verifier that can describe the policy in force (nil if none can).
func identityApprovers(pol *ApprovalPolicy, verifierFor ApproverVerifierFor) (ids []string, desc IdentityDescriber) {
	if pol == nil || verifierFor == nil {
		return nil, nil
	}
	for _, id := range pol.Approvers {
		v, ok := verifierFor(id)
		if !ok || v == nil || isNilVerifier(v) {
			continue
		}
		if _, cert := v.(CertDecisionVerifier); !cert {
			continue
		}
		ids = append(ids, id)
		if d, ok := v.(IdentityDescriber); ok && desc == nil {
			desc = d
		}
	}
	return ids, desc
}

// identityKinds is the SHA-256 of the sorted "approver=bare|cert" lines of pol's approvers, so a note
// states which approvers were certificate-bound without listing keys.
func identityKinds(pol *ApprovalPolicy, bound []string) string {
	lines := make([]string, 0, len(pol.Approvers))
	for _, id := range pol.Approvers {
		kind := "bare"
		if slices.Contains(bound, id) {
			kind = "cert"
		}
		lines = append(lines, id+"="+kind)
	}
	slices.Sort(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// addIdentityFields sets the identity members of a pause note when pol has a certificate-bound approver.
func addIdentityFields(w *ApprovalWait, pol *ApprovalPolicy, verifierFor ApproverVerifierFor) {
	bound, desc := identityApprovers(pol, verifierFor)
	if len(bound) == 0 {
		return
	}
	w.IdentityBound, w.IdentityApprovers, w.IdentityKinds = true, bound, identityKinds(pol, bound)
	if desc != nil {
		w.IdentityPolicyDigest = desc.PolicyDigest()
	}
}

// ApprovalPauseStep is the journal name of the n-th phase note of the gate on toolUseID (an ApprovalWait
// with Pause set), n counting from 1 in the order the gate wrote them. It is informational and
// insert-once; the newest is the gate's phase (LatestPausePhase).
func ApprovalPauseStep(toolUseID string, n int) string {
	return fmt.Sprintf("approval-pause:%s:%d", encodeID(toolUseID), n)
}

// LatestPausePhase returns the newest phase note of toolUseID's gate among recs (ApprovalPauseStep),
// decoded, and ok=false when the gate has written none (its phase is then PauseAwaitingDecisions). A
// note is read only if its Pause is one of the Pause constants.
func LatestPausePhase(recs []Record, toolUseID string) (ApprovalWait, bool) {
	prefix := "approval-pause:" + encodeID(toolUseID) + ":"
	best, bestN := ApprovalWait{}, 0
	for _, r := range recs {
		if r.Kind != StepValue || !strings.HasPrefix(r.Name, prefix) {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(strings.TrimPrefix(r.Name, prefix), "%d", &n); err != nil || n <= bestN {
			continue
		}
		var w ApprovalWait
		if json.Unmarshal(r.Result, &w) != nil || w.ToolUseID != toolUseID || !knownPause(w.Pause) {
			continue
		}
		best, bestN = w, n
	}
	return best, bestN > 0
}

func knownPause(p string) bool {
	switch p {
	case PauseAwaitingDecisions, PauseAwaitingAnchor, PauseAwaitingStatus, PauseHeldUntilFinal, PauseExecHold:
		return true
	}
	return false
}

// pausePhase says why a gate with certificate-bound approvers is waiting, from how its decisions were
// classified: a decision that verifies but has no covering head yet is awaiting-anchor, one whose
// status is missing or stale is awaiting-status, anything else is awaiting-decisions.
func pausePhase(checks []DecisionCheck) (phase, detail string) {
	var anchor, status []string
	for _, c := range checks {
		if c.Cert == nil || c.Counted {
			continue
		}
		switch c.Cert.Reason {
		case ReasonNoCoveringHead:
			anchor = append(anchor, c.Approver)
		case ReasonStatusUnknown, ReasonStatusStale:
			status = append(status, c.Approver)
		}
	}
	switch {
	case len(anchor) > 0:
		return PauseAwaitingAnchor, "the decision of " + strings.Join(anchor, ", ") + " is recorded, and no signed head covers it yet, so when it became provable is not known"
	case len(status) > 0:
		return PauseAwaitingStatus, "the decision of " + strings.Join(status, ", ") + " is waiting for revocation status that is current"
	}
	return PauseAwaitingDecisions, ""
}

// notePause records, when the gate's phase changed, the phase note of a gate with a certificate-bound
// approver. recs is the journal the count read. Nothing reads the note to decide, so a failed write is
// dropped; the first note says awaiting-decisions only when a later phase is left.
func notePause(ctx context.Context, j *Journal, runID string, s ApprovalSubject, pol *ApprovalPolicy, verifierFor ApproverVerifierFor, recs []Record, checks []DecisionCheck) {
	bound, _ := identityApprovers(pol, verifierFor)
	if len(bound) == 0 {
		return
	}
	phase, detail := pausePhase(checks)
	last, have := LatestPausePhase(recs, s.ToolUseID)
	switch {
	case !have && phase == PauseAwaitingDecisions:
		return
	case have && last.Pause == phase && last.PauseDetail == detail:
		return
	}
	n := 1
	if have {
		n = len(pauseNotes(recs, s.ToolUseID)) + 1
	}
	w := ApprovalWait{ToolUseID: s.ToolUseID, ToolName: s.ToolName, Need: pol.Need, Approvers: slices.Clone(pol.Approvers),
		RecordedAt: noteStamp(clockFrom(ctx)), Pause: phase, PauseDetail: detail}
	addIdentityFields(&w, pol, verifierFor)
	b, err := json.Marshal(w)
	if err != nil {
		return
	}
	_, _ = j.put(ctx, runID, ApprovalPauseStep(s.ToolUseID, n), Record{Kind: StepValue, Result: b})
}

func pauseNotes(recs []Record, toolUseID string) []Record {
	prefix := "approval-pause:" + encodeID(toolUseID) + ":"
	var out []Record
	for _, r := range recs {
		if r.Kind == StepValue && strings.HasPrefix(r.Name, prefix) {
			out = append(out, r)
		}
	}
	return out
}

// anchorFirst asks the store to publish a head over runID's journal when a decision that verifies has
// no covering head (checks), at most once per ReanchorMinInterval per run. It reports whether it asked,
// so the caller counts again. A store with no Reanchorer, a refused rate, and a failed anchoring all
// leave the gate waiting (awaiting-anchor); none is an error, since the decision is not lost.
func anchorFirst(ctx context.Context, j *Journal, runID string, checks []DecisionCheck) bool {
	need := slices.ContainsFunc(checks, func(c DecisionCheck) bool {
		return c.Cert != nil && !c.Counted && c.Cert.Reason == ReasonNoCoveringHead
	})
	if !need {
		return false
	}
	ra, ok := Capability[Reanchorer](j.store)
	if !ok || !reanchorAllowed(j.id, runID, clockFrom(ctx)()) {
		return false
	}
	// A hung anchor must not hold the drive: bound the call.
	actx, cancel := context.WithTimeout(ctx, reanchorTimeout)
	defer cancel()
	_ = ra.Reanchor(actx, runID)
	return true
}

// StatusArchiver is implemented by the verifier of a certificate-bound approver that has a status
// archive: ArchiveStatus stores the bytes of a CRL larger than MaxStatusInline under its hash, so what
// the gate relied on survives although the journal holds only its hash. A policy without one cannot
// count on a CRL that large.
type StatusArchiver interface {
	ArchiveStatus(ctx context.Context, hash string, der []byte) error
}

// recordIdentity journals the identity environment of a terminal count of a gate with a
// certificate-bound approver: the CRLs the counted decisions rest on, each once by hash, then the
// ApprovalIdentity of the generation, naming the tally (tallyName) it belongs to and the digest of the
// exact tally bytes it will have. The caller then writes exactly those tally bytes, which is the commit
// point; a crash before it leaves records that a later evaluation writes again as no-ops, and one whose
// count differs gets its own identity record (ApprovalIdentityDigestStep), never a stale one under the
// plain name. A CRL above MaxStatusInline goes to the verifier's StatusArchiver before the record is
// written, and without one the gate refuses to count (ErrConfig).
func recordIdentity(ctx context.Context, j *Journal, runID string, s ApprovalSubject, pol *ApprovalPolicy, verifierFor ApproverVerifierFor, gen int, tallyName string, t ApprovalTally, checks []DecisionCheck) error {
	bound, desc := identityApprovers(pol, verifierFor)
	if len(bound) == 0 {
		return nil
	}
	tb, err := marshalJournal(t) // the bytes the journal stores for the tally, not another encoding of it
	if err != nil {
		return fmt.Errorf("encode the tally of %s (run %s): %w (%w)", s.ToolUseID, runID, err, ErrStorage)
	}
	td := sha256.Sum256(tb)
	rec := ApprovalIdentity{Format: ApprovalIdentityFormat, ToolUseID: s.ToolUseID, Generation: gen, Tally: tallyName, TallyDigest: hex.EncodeToString(td[:]),
		Evaluated: EvaluatedLive, EvaluatedAt: clockFrom(ctx)().UTC().Format(time.RFC3339Nano), Rules: ApprovalIdentityRulesVer, Verifier: runtime.Version(),
		RecordedAt: noteStamp(clockFrom(ctx)), Decisions: []IdentityDecision{}}
	if desc != nil {
		rec.Policy, rec.PolicyDigest, rec.RosterDigest = desc.DescribeIdentity(), desc.PolicyDigest(), desc.RosterDigest()
		rec.VerifierRev, rec.GoDebug, rec.BuildGoDebug = desc.VerifierInfo()
	}
	byApprover := map[string]DecisionCheck{}
	for _, c := range checks {
		if c.Counted && c.Cert != nil {
			byApprover[c.Approver] = c
		}
	}
	arch, _ := desc.(StatusArchiver)
	for _, id := range t.Approvers {
		c, ok := byApprover[id]
		if !ok {
			continue
		}
		cc := c.Cert
		d := IdentityDecision{Approver: id, Step: c.Step, Identifier: cc.Identity, Chain: cc.Chain, Anchor: cc.Anchor,
			AnchoredAt: cc.Anchored.UTC().Format(time.RFC3339Nano), TimeSource: timeSource(cc.Time), TimeEvidence: cc.TimeEvidence,
			HeadSize: cc.HeadSize, HeadRoot: cc.HeadRoot, HeadHash: cc.HeadHash, Tier: identityTier(cc), BasisKind: cc.BasisKind,
			Basis: cc.Basis, Window: cc.Window, Warnings: cc.Warnings}
		if cc.StatusTime != "" {
			d.StatusTime = cc.StatusTime
		}
		if len(cc.Chain) > 0 {
			d.LeafHash = cc.Chain[0]
		}
		for _, ev := range cc.Evidence {
			sum := sha256.Sum256(ev.DER)
			h := hex.EncodeToString(sum[:])
			ev.Size = len(ev.DER)
			if ev.Size > MaxStatusInline {
				if arch == nil {
					return fmt.Errorf("agent: tool call %q: the CRL of issuer %.16s is %d bytes, over the %d the journal stores, and the certificate policy has no status archive, so the decision is not counted: configure CertApprover.StatusArchive (or bound the snapshot's CRL size): %w", s.ToolUseID, ev.Issuer, ev.Size, MaxStatusInline, ErrConfig)
				}
				if err := arch.ArchiveStatus(ctx, h, ev.DER); err != nil {
					if errors.Is(err, ErrConfig) {
						return fmt.Errorf("archive the CRL of issuer %.16s (%d bytes, run %s): %w", ev.Issuer, ev.Size, runID, err)
					}
					return fmt.Errorf("archive status %s (run %s): %w (%w)", h, runID, err, ErrStorage)
				}
				ev.DER = nil
			}
			if _, err := step(ctx, j, runID, ApprovalStatusStep(h), func(context.Context) (ApprovalStatusBytes, error) { return ev, nil }, WithSafety(Safety{ReadOnly: true})); err != nil {
				return fmt.Errorf("record %s (run %s): %w (%w)", ApprovalStatusStep(h), runID, err, ErrStorage)
			}
			d.Status = append(d.Status, h)
		}
		rec.Decisions = append(rec.Decisions, d)
	}
	name := ApprovalIdentityStep(s.ToolUseID, gen)
	if old, ok, err := j.Get(ctx, runID, name); err != nil {
		return fmt.Errorf("read %s (run %s): %w (%w)", name, runID, err, ErrStorage)
	} else if ok {
		var prev ApprovalIdentity
		if json.Unmarshal(old.Result, &prev) != nil || prev.TallyDigest != rec.TallyDigest {
			name = ApprovalIdentityDigestStep(s.ToolUseID, gen, rec.TallyDigest)
		}
	}
	if _, err := step(ctx, j, runID, name, func(context.Context) (ApprovalIdentity, error) { return rec, nil }, WithSafety(Safety{ReadOnly: true})); err != nil {
		return fmt.Errorf("record %s (run %s): %w (%w)", name, runID, err, ErrStorage)
	}
	return nil
}

// timeSource names where A(d) came from, as a TimeSource constant.
func timeSource(t string) string {
	switch {
	case strings.HasPrefix(t, "rfc3161"):
		return TimeSourceRFC3161
	case strings.HasPrefix(t, "anchor-receipt"):
		return TimeSourceAnchorReceipt
	}
	return TimeSourceLogSignerClock
}

// identityTier maps a verifier's finding to the identity record's tier: only a final status is a
// final tier, and the time source says which; a live count or a short window is provisional.
func identityTier(cc *CertCheck) string {
	switch {
	case cc.Tier != "final":
		return TierProvisional
	case timeSource(cc.Time) != TimeSourceLogSignerClock:
		return TierIndependentFinal
	}
	return TierLogclockFinal
}

// IdentityRecordFor returns the identity record of generation gen that belongs to tally, the recorded
// tally of toolUseID's gate at that generation: the record whose TallyDigest equals the SHA-256 of
// tally's stored bytes, looked up under ApprovalIdentityStep first and then under
// ApprovalIdentityDigestStep (two drives racing, or a count that differed from an earlier attempt, can
// produce either). ok is false when none matches: a viewer or auditor then reports "incomplete identity
// data" and never a decided, verified gate.
func IdentityRecordFor(recs []Record, toolUseID string, gen int, tally Record) (ApprovalIdentity, bool) {
	sum := sha256.Sum256(tally.Result)
	digest := hex.EncodeToString(sum[:])
	for _, name := range []string{ApprovalIdentityStep(toolUseID, gen), ApprovalIdentityDigestStep(toolUseID, gen, digest)} {
		for _, r := range recs {
			if r.Kind != StepValue || r.Name != name {
				continue
			}
			var id ApprovalIdentity
			if json.Unmarshal(r.Result, &id) == nil && id.TallyDigest == digest && id.ToolUseID == toolUseID && id.Generation == gen {
				return id, true
			}
			break
		}
	}
	return ApprovalIdentity{}, false
}
