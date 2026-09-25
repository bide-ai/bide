package audit

import (
	"fmt"
	"strconv"
)

// EarnedAuthority is a control loop that adjusts an agent's delegated limit from its provable track
// record. Authority starts at the ladder's baseline rung and is promoted one rung after
// promoteEvery compliant actions, capped at the top rung; an anomaly resets it to baseline at once.
// Each change re-issues a signed grant that is a CHILD of the root grant (ParentRef to root, scope
// limit = the current rung), so the earned limit provably never exceeds the root ceiling: every
// earned grant passes VerifyDelegationChain against the root under AttenuatesNumericScope, even a
// buggy or compromised controller cannot widen past what the root principal authorized.
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

	tier    int
	streak  int
	current SignedGrant
}

// NewEarnedAuthority builds a controller for subject, granting the limits in ladder (rung 0 is the
// baseline) and promoting one rung every promoteEvery compliant actions. root is the ceiling: the
// top rung must not exceed root's numeric "limit" scope, so no earned grant can widen past what the
// root principal authorized. It issues the baseline grant immediately.
func NewEarnedAuthority(ladder []int, promoteEvery int, root SignedGrant, signer Signer, subject string) (*EarnedAuthority, error) {
	if len(ladder) == 0 {
		return nil, fmt.Errorf("audit: earned authority needs a non-empty ladder")
	}
	if promoteEvery <= 0 {
		return nil, fmt.Errorf("audit: promoteEvery must be positive")
	}
	ceiling, err := strconv.Atoi(root.Grant.Scope["limit"])
	if err != nil {
		return nil, fmt.Errorf("audit: root grant has no numeric limit scope: %w", err)
	}
	for i, l := range ladder {
		if l > ceiling {
			return nil, fmt.Errorf("audit: ladder rung %d (%d) exceeds the root ceiling (%d)", i, l, ceiling)
		}
	}
	e := &EarnedAuthority{ladder: ladder, every: promoteEvery, root: root, signer: signer, subject: subject}
	if err := e.reissue(); err != nil {
		return nil, err
	}
	return e, nil
}

// RecordCompliant notes one clean governed action; it promotes one rung when the streak reaches the
// threshold (unless already at the top), returning whether a promotion happened.
func (e *EarnedAuthority) RecordCompliant() (promoted bool, err error) {
	e.streak++
	if e.streak >= e.every && e.tier < len(e.ladder)-1 {
		e.tier++
		e.streak = 0
		return true, e.reissue()
	}
	return false, nil
}

// FlagAnomaly resets authority to the baseline rung immediately, returning whether it changed.
func (e *EarnedAuthority) FlagAnomaly() (changed bool, err error) {
	e.streak = 0
	if e.tier == 0 {
		return false, nil
	}
	e.tier = 0
	return true, e.reissue()
}

// Grant returns the current signed earned grant (a child of root, scope limit = the current rung).
func (e *EarnedAuthority) Grant() SignedGrant { return e.current }

// Limit returns the currently earned limit.
func (e *EarnedAuthority) Limit() int { return e.ladder[e.tier] }

// Tier returns the current rung (0 is baseline).
func (e *EarnedAuthority) Tier() int { return e.tier }

func (e *EarnedAuthority) reissue() error {
	child := Grant{
		ID:        fmt.Sprintf("earned/%s/t%d", e.subject, e.tier),
		Issuer:    e.root.Grant.Subject,
		Subject:   e.subject,
		Scope:     map[string]string{"limit": strconv.Itoa(e.ladder[e.tier])},
		ParentRef: e.root.Grant.Digest(),
	}
	sg, err := SignGrant(child, e.signer)
	if err != nil {
		return fmt.Errorf("audit: reissue earned grant: %w", err)
	}
	e.current = sg
	return nil
}
