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
// ledger; every earlier grant is superseded, whether the change was a promotion or a demotion. An offline verifier learns the
// current grant the way it learns any run's state: from the latest signed head of the ledger run in
// the anchor log (anchor the ledger with an AuditedStore, or sign and publish its heads), checked for
// consistency with the heads it saw before, so the ledger cannot be rolled back to a superseded grant.
// ProveCurrentGrant produces the proof and VerifyCurrentGrant checks it: the grant is the leaf at
// index Size-1 of an authentic ledger head.
//
// It is deliberately a sequential controller, not a convergent gsm machine. Earning is temporal:
// how many compliant actions preceded a promotion, and whether an anomaly interleaved, is
// order-dependent, so it belongs to the durable/saga tier, not the order-independent governance
// tier (a promote event and a record event do not commute, so no convergent machine expresses it).
// The ENFORCEMENT of the current limit stays a convergent gsm invariant on the work machine (see
// examples/earned-authority); the controller only decides the limit and re-issues the grant.
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

// ProveCurrentGrant proves which earned grant is current in the ledger at the signed ledger head
// sth: an inclusion proof of the ledger's last leaf (index sth.Size-1). Pass the latest anchored
// head of the ledger run.
func ProveCurrentGrant(ctx context.Context, ledger agent.Durable, ledgerRunID string, sth SignedTreeHead) (ProofBundle, error) {
	if sth.Size < 1 {
		return ProofBundle{}, fmt.Errorf("audit: ledger %s is empty at this head", ledgerRunID)
	}
	return ProveRecord(ctx, ledger, ledgerRunID, sth.Size-1, sth)
}

// VerifyCurrentGrant checks that sg is the current earned grant of the ledger whose signed head
// current proves, under logPub (the log key, out of band): current verifies, its record is the
// anchored leaf of exactly sg, and that leaf is the LAST leaf of the signed ledger (index Size-1). A
// grant superseded by a later promotion or demotion fails, because a
// later ledger head's last leaf is the newer grant. It does not check sg's chain to the root or its
// expiry; do that with VerifyDelegationChain(..., EarnedRules) and Grant.Expired. The verifier must
// take current's head from the anchor log's latest head of the ledger run: an older head proves only
// that sg was current then.
func VerifyCurrentGrant(sg SignedGrant, current ProofBundle, logPub ed25519.PublicKey) (bool, error) {
	ok, err := current.Verify(logPub)
	if err != nil || !ok {
		return false, err
	}
	if current.Record.Kind != agent.StepValue || !sameSignedGrant(sg, current.Record) {
		return false, fmt.Errorf("audit: the ledger's last leaf is not grant %q", sg.Grant.ID)
	}
	if current.Inclusion.Index != current.STH.Size-1 {
		return false, fmt.Errorf("audit: grant %q is leaf %d of a ledger of %d: superseded", sg.Grant.ID, current.Inclusion.Index, current.STH.Size)
	}
	return true, nil
}
