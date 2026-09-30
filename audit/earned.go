package audit

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"maps"
	"strconv"

	"github.com/bide-ai/bide/agent"
)

// EarnedAuthority is a control loop that adjusts an agent's delegated limit from its provable track
// record. Authority starts at the ladder's baseline rung and is promoted one rung after
// promoteEvery compliant actions, capped at the top rung; an anomaly resets it to baseline at once.
// Each change re-issues a signed grant that is a CHILD of the root grant (ParentRef to root, every
// root scope constraint kept, "limit" = the current rung), so the earned limit provably never exceeds
// the root ceiling: every earned grant passes VerifyDelegationChain against the root with
// ScopeRules{"limit": NumericAtMost}, and even a buggy or compromised controller cannot widen past
// what the root principal authorized. Every earned grant carries the root's NotAfter, so none
// outlives the authority it was carved from.
//
// Revocation. A demotion must take the old, higher grant out of use at once, not when it expires.
// Each re-issued grant is appended to a ledger: a journal run (ledgerRunID in ledger) that holds
// nothing but this controller's grants, one leaf per issue (the grant's ID names its ledger index, so
// no two issues share a digest, even at the same rung). The CURRENT grant is the last leaf of the
// ledger; every earlier grant is superseded, whether the change was a promotion or a demotion. An
// offline verifier learns the current grant the way it learns any run's state: from the latest
// signed head of the ledger run in the anchor log (anchor the ledger with an AuditedStore, or sign
// and publish its heads), checked for consistency with the last head it saw, so the ledger cannot
// be rolled back to a superseded grant. ProveCurrentGrant produces the proof and VerifyCurrentGrant
// checks it: the grant is the leaf at index Size-1 of an authentic head of the ledger run, and that
// head extends the verifier's last-seen head.
//
// It is deliberately a sequential controller, not a convergent gsm machine. Earning is temporal:
// how many compliant actions preceded a promotion, and whether an anomaly interleaved, is
// order-dependent, so it belongs to the durable/saga tier, not the order-independent governance
// tier (a promote event and a record event do not commute, so no convergent machine expresses it).
// The ENFORCEMENT of the current limit stays a convergent gsm invariant on the work machine (see
// examples/govern/earned-authority); the controller only decides the limit and re-issues the grant.
//
// The asymmetry is the safety property: promotion is slow, capped, and evidence-gated; attenuation
// is immediate and needs no gate, because shrinking authority is always safe.
type EarnedAuthority struct {
	ladder  []int
	every   int
	root    SignedGrant
	signer  Signer
	subject string
	ledger  agent.Durable
	ledgerR string

	tier    int
	streak  int
	current SignedGrant
}

// EarnedScopeLimit is the scope key an earned grant sets to the earned limit (at most the root's).
const EarnedScopeLimit = "limit"

// EarnedRules is the ScopeRules an earned grant attenuates its root under.
var EarnedRules = ScopeRules{EarnedScopeLimit: NumericAtMost}

// NewEarnedAuthority builds a controller for subject, granting the limits in ladder (rung 0 is the
// baseline) and promoting one rung every promoteEvery compliant actions. root is the ceiling: the
// top rung must not exceed root's numeric "limit" scope. signer signs as root's Subject. Each issued
// grant is recorded in ledgerRunID of ledger, which must hold only this controller's grants; a
// restarted controller continues the ledger at baseline.
// It issues the baseline grant immediately.
func NewEarnedAuthority(ctx context.Context, ladder []int, promoteEvery int, root SignedGrant, signer Signer, subject string, ledger agent.Durable, ledgerRunID string) (*EarnedAuthority, error) {
	if len(ladder) == 0 {
		return nil, fmt.Errorf("audit: earned authority needs a non-empty ladder")
	}
	if promoteEvery <= 0 {
		return nil, fmt.Errorf("audit: promoteEvery must be positive")
	}
	if ledger == nil || ledgerRunID == "" {
		return nil, fmt.Errorf("audit: earned authority needs a ledger store and run id")
	}
	ceiling, err := strconv.Atoi(root.Grant.Scope[EarnedScopeLimit])
	if err != nil {
		return nil, fmt.Errorf("audit: root grant has no numeric limit scope: %w", err)
	}
	for i, l := range ladder {
		if l > ceiling {
			return nil, fmt.Errorf("audit: ladder rung %d (%d) exceeds the root ceiling (%d)", i, l, ceiling)
		}
	}
	e := &EarnedAuthority{ladder: ladder, every: promoteEvery, root: root, signer: signer, subject: subject, ledger: ledger, ledgerR: ledgerRunID}
	if err := e.reissue(ctx); err != nil {
		return nil, err
	}
	return e, nil
}

// RecordCompliant notes one clean governed action; it promotes one rung when the streak reaches the
// threshold (unless already at the top), returning whether a promotion happened.
func (e *EarnedAuthority) RecordCompliant(ctx context.Context) (promoted bool, err error) {
	e.streak++
	if e.streak >= e.every && e.tier < len(e.ladder)-1 {
		e.tier++
		e.streak = 0
		return true, e.reissue(ctx)
	}
	return false, nil
}

// FlagAnomaly resets authority to the baseline rung immediately, returning whether it changed. The
// re-issued baseline grant supersedes every earlier grant in the ledger.
func (e *EarnedAuthority) FlagAnomaly(ctx context.Context) (changed bool, err error) {
	e.streak = 0
	if e.tier == 0 {
		return false, nil
	}
	e.tier = 0
	return true, e.reissue(ctx)
}

// Grant returns the current signed earned grant (a child of root, scope limit = the current rung).
func (e *EarnedAuthority) Grant() SignedGrant { return e.current }

// Limit returns the currently earned limit.
func (e *EarnedAuthority) Limit() int { return e.ladder[e.tier] }

// Tier returns the current rung (0 is baseline).
func (e *EarnedAuthority) Tier() int { return e.tier }

func (e *EarnedAuthority) reissue(ctx context.Context) error {
	recs, err := e.ledger.History(ctx, e.ledgerR)
	if err != nil {
		return fmt.Errorf("audit: reissue earned grant: load ledger %s: %w", e.ledgerR, err)
	}
	index := len(recs)
	scope := maps.Clone(e.root.Grant.Scope)
	scope[EarnedScopeLimit] = strconv.Itoa(e.ladder[e.tier])
	child := Grant{
		ID:        fmt.Sprintf("earned/%s/%d", e.subject, index),
		Issuer:    e.root.Grant.Subject,
		Subject:   e.subject,
		Scope:     scope,
		NotAfter:  e.root.Grant.NotAfter,
		ParentRef: e.root.Grant.Digest(),
	}
	sg, err := SignGrant(child, e.signer)
	if err != nil {
		return fmt.Errorf("audit: reissue earned grant: %w", err)
	}
	if _, err := RecordGrant(ctx, e.ledger, e.ledgerR, sg); err != nil {
		return fmt.Errorf("audit: reissue earned grant: %w", err)
	}
	after, err := e.ledger.History(ctx, e.ledgerR)
	if err != nil {
		return fmt.Errorf("audit: reissue earned grant: load ledger %s: %w", e.ledgerR, err)
	}
	if len(after) != index+1 || after[index].Name != grantLeafName(child.Digest()) {
		return fmt.Errorf("audit: reissue earned grant: ledger %s did not record it as leaf %d (another writer?)", e.ledgerR, index)
	}
	e.current = sg
	return nil
}

// CurrentGrantProof proves which earned grant is current in a ledger: Leaf proves the grant is the
// last leaf of a signed ledger head, and Consistency proves that head is an append-only extension
// of the head the verifier saw last, so a verifier that remembers its last-seen head cannot be
// shown an older head as current.
type CurrentGrantProof struct {
	Format      string      `json:"format"`      // CurrentGrantFormat
	Leaf        ProofBundle `json:"leaf"`        // the ledger's last leaf under the presented head
	Consistency Consistency `json:"consistency"` // from the verifier's last-seen size to Leaf.STH.Size
}

// ProveCurrentGrant proves which earned grant is current in the ledger at the signed ledger head
// sth: an inclusion proof of the ledger's last leaf (index sth.Size-1), plus a consistency proof
// from lastSeenSize (the size of the ledger head the verifier saw last, 0 if none) to sth.Size.
// Pass the latest anchored head of the ledger run. It fails if sth is not a head of ledgerRunID's
// journal, or if lastSeenSize is outside [0, sth.Size].
func ProveCurrentGrant(ctx context.Context, ledger agent.Durable, ledgerRunID string, sth SignedTreeHead, lastSeenSize int) (CurrentGrantProof, error) {
	if sth.Size < 1 {
		return CurrentGrantProof{}, fmt.Errorf("audit: ledger %s is empty at this head", ledgerRunID)
	}
	if lastSeenSize < 0 || lastSeenSize > sth.Size {
		return CurrentGrantProof{}, fmt.Errorf("audit: last-seen ledger size %d out of range [0,%d]", lastSeenSize, sth.Size)
	}
	leaf, err := ProveRecord(ctx, ledger, ledgerRunID, sth.Size-1, sth)
	if err != nil {
		return CurrentGrantProof{}, err
	}
	recs, err := ledger.History(ctx, ledgerRunID)
	if err != nil {
		return CurrentGrantProof{}, fmt.Errorf("audit: load journal %s: %w", ledgerRunID, err)
	}
	recs, err = journalPrefix(ledgerRunID, recs, sth.TreeHead)
	if err != nil {
		return CurrentGrantProof{}, err
	}
	leaves, err := canonicalLeaves(recs)
	if err != nil {
		return CurrentGrantProof{}, err
	}
	return CurrentGrantProof{
		Format:      CurrentGrantFormat,
		Leaf:        leaf,
		Consistency: Consistency{First: lastSeenSize, Size: sth.Size, Path: consistencyProof(lastSeenSize, leaves)},
	}, nil
}

// VerifyCurrentGrant checks that sg is the current earned grant of the ledger run ledgerRunID,
// under logPub (the log key, out of band). It requires that:
//
//   - the proof is about ledgerRunID. A grant recorded in any other run signed by the same log key
//     (an agent run that records the grant it acts under, say) proves nothing about the ledger;
//   - the proof verifies, its record is the anchored leaf of exactly sg, and that leaf is the LAST
//     leaf of the signed ledger head (index Size-1). A grant superseded by a later promotion or
//     demotion fails, because a later ledger head's last leaf is the newer grant;
//   - when lastSeen is not nil, lastSeen is an authentic head of ledgerRunID under logPub and the
//     presented head is an append-only extension of it (checked with the proof's consistency
//     proof), so a head older than one the verifier already saw cannot be presented as current.
//
// How an offline verifier obtains the latest head: it takes the newest head of ledgerRunID from
// the anchor log (anchor the ledger through an AuditedStore, or sign and publish its heads to an
// Anchor), confirms that head is in the anchor log (MemAnchorLog.Prove, VerifyAnchorInclusion),
// and keeps the newest head it has verified as lastSeen for its next check, asking the prover for
// a proof from lastSeen.Size. Pass nil only on first contact with a ledger: the check then shows
// that sg was current at the presented head, which may be older than the latest one. Freshness
// beyond what the verifier has seen comes from the anchor log, not from the proof.
//
// It does not check sg's chain to the root or its expiry; do that with
// VerifyDelegationChain(..., EarnedRules) and Grant.Expired.
func VerifyCurrentGrant(sg SignedGrant, ledgerRunID string, p CurrentGrantProof, lastSeen *SignedTreeHead, logPub ed25519.PublicKey) (bool, error) {
	if err := formatOf(p, p.Format); err != nil {
		return false, err
	}
	if p.Leaf.RunID != ledgerRunID {
		return false, fmt.Errorf("audit: the proof is over run %q, not ledger %q", p.Leaf.RunID, ledgerRunID)
	}
	ok, err := p.Leaf.Verify(logPub)
	if err != nil || !ok {
		return false, err
	}
	if p.Leaf.Record.Kind != agent.StepValue || !sameSignedGrant(sg, p.Leaf.Record) {
		return false, fmt.Errorf("audit: the ledger's last leaf is not grant %q", sg.Grant.ID)
	}
	if p.Leaf.Inclusion.Index != p.Leaf.STH.Size-1 {
		return false, fmt.Errorf("audit: grant %q is leaf %d of a ledger of %d: superseded", sg.Grant.ID, p.Leaf.Inclusion.Index, p.Leaf.STH.Size)
	}
	if lastSeen == nil {
		return true, nil
	}
	if !lastSeen.Verify(logPub) || lastSeen.Kind != TreeJournal || lastSeen.RunID != ledgerRunID {
		return false, fmt.Errorf("audit: the last-seen head is not an authentic head of ledger %q", ledgerRunID)
	}
	// First must be the last-seen size: a proof from size 0 is empty and passes for any head.
	if p.Consistency.First != lastSeen.Size || !VerifyConsistency(lastSeen.Root, p.Leaf.STH.Root, p.Consistency) {
		return false, fmt.Errorf("audit: the ledger head of size %d does not extend the last-seen head of size %d", p.Leaf.STH.Size, lastSeen.Size)
	}
	return true, nil
}
