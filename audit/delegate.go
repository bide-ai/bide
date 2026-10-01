package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/toolhook"
)

// grantCtxKey carries the acting principal's signed grant plus the signer used to mint attenuated
// child grants, so AttenuatingSubAgent can narrow authority automatically down a delegation tree.
type grantCtxKey struct{}

type grantCarrier struct {
	sg     SignedGrant
	signer Signer
	absent bool // withoutGrant: a grant bound further out does not apply here
	// delegated marks a child grant an AttenuatingSubAgent bound for its sub-run: every tool call
	// under it is refused once it has expired (see init).
	delegated bool
}

// WithGrant binds the acting principal's signed grant and its signer to ctx. AttenuatingSubAgent
// reads this to mint a narrower, signed child grant on each delegation and rebind the sub-run's
// identity to it. Set it once at the root (the top principal's grant and key); each delegation then
// propagates the narrowed child grant automatically, so authority only ever shrinks down the tree.
func WithGrant(ctx context.Context, sg SignedGrant, signer Signer) context.Context {
	return context.WithValue(ctx, grantCtxKey{}, grantCarrier{sg: sg, signer: signer})
}

// GrantFrom returns the acting signed grant and signer bound to ctx, and whether one was set.
func GrantFrom(ctx context.Context) (SignedGrant, Signer, bool) {
	c, ok := ctx.Value(grantCtxKey{}).(grantCarrier)
	if !ok || c.absent {
		return SignedGrant{}, nil, false
	}
	return c.sg, c.signer, true
}

// withoutGrant returns ctx with no grant bound, whatever an outer context bound: for work a
// delegation ran without one.
func withoutGrant(ctx context.Context) context.Context {
	return context.WithValue(ctx, grantCtxKey{}, grantCarrier{absent: true})
}

// AttenuateFunc derives a child grant from the parent grant and the delegating sub-agent's name.
// It sets the narrower Scope (and may set an earlier NotAfterUnix; a Subject it sets must be the
// sub-agent's name, or the delegation is refused); AttenuatingSubAgent fills in ParentRef and, if
// empty, Issuer (the parent's Subject), Subject (the sub-agent name), and NotAfterUnix (the
// parent's), then checks the result with CheckAttenuation before signing it, so a
// widening func is refused at delegation time rather than caught later by VerifyDelegationChain.
type AttenuateFunc func(parent Grant, subAgent string) Grant

// AttenuatingSubAgent wraps a sub-agent as a delegation Tool that attenuates authority by default.
// On each call, if a signed grant is bound to ctx (WithGrant), it mints a narrower child grant
// (ParentRef hash-linked to the parent, checked with CheckAttenuation under rules, signed by the
// same signer), records it as a durable leaf in
// the sub-run so the chain is anchored, rebinds the sub-run's identity to the child (Actor = this
// sub-agent, OnBehalfOf = the parent's Subject, AuthorityRef = the child's digest), and propagates
// the child grant so a deeper delegation narrows again. With no grant on ctx it is a plain
// sub-agent (it inherits the identity), so it is safe to use either way. With a grant, it must be
// called from an agent run (Agent.Run / RunSaga), whose run scope gives each call its own sub-run
// (agent.SubRunID of the parent run and the tool-use id); called outside one it refuses rather
// than fall back to a sub-run ID that every parent run would share.
//
// The effect is that capabilities only ever shrink down a delegation tree, by default rather than
// by remembering to wire it, and the whole chain is provable end to end via VerifyDelegationChain.
// It is a composition of the grant machinery into the sub-agent seam, not a new agent type: the
// wrapped `sub` still runs its own full agent loop and reasons autonomously within the narrower
// authority.
//
// cfg says where the child grants are recorded and how authority narrows; opts are passed to
// agent.SubAgent, so agent.WithApproval makes the parent wait for a human before it delegates.
// AttenuatingSubAgent panics, with an error wrapping agent.ErrConfig, on a nil cfg.Store or
// cfg.Narrow, and on an option agent.SubAgent refuses.
func AttenuatingSubAgent(name, description string, sub *agent.Agent, cfg AttenuationConfig, opts ...agent.ToolOption) agent.Tool {
	switch {
	case cfg.Store == nil:
		panic(fmt.Errorf("audit: AttenuatingSubAgent %q: AttenuationConfig.Store is nil: %w", name, agent.ErrConfig))
	case cfg.Narrow == nil:
		panic(fmt.Errorf("audit: AttenuatingSubAgent %q: AttenuationConfig.Narrow is nil: %w", name, agent.ErrConfig))
	}
	return &attenuatingSubAgent{
		Tool:   agent.SubAgent(name, description, sub, opts...),
		name:   name,
		store:  cfg.Store,
		narrow: cfg.Narrow,
		rules:  cfg.Rules,
	}
}

// AttenuationConfig configures AttenuatingSubAgent.
type AttenuationConfig struct {
	// Store is the Durable store the sub-runs journal to, where each child grant is recorded as
	// a durable leaf of its sub-run. Give it the store the parent and the sub-agent use, for a
	// unified, provable journal.
	Store agent.Durable
	// Narrow derives each child grant from the parent's (see AttenuateFunc).
	Narrow AttenuateFunc
	// Rules says how each scope key may narrow; CheckAttenuation enforces it before a child
	// grant is signed.
	Rules ScopeRules
}

// attenuatingSubAgent embeds the plain SubAgent tool (for Name/Description/ArgsSchema/Safety and the
// durable sub-run) and overrides Call to attenuate authority before delegating.
type attenuatingSubAgent struct {
	agent.Tool
	name   string
	store  agent.Durable
	narrow AttenuateFunc
	rules  ScopeRules
}

// Spec returns the spec of the SubAgent tool it wraps.
func (t *attenuatingSubAgent) Spec() agent.ToolSpec { return agent.SpecOf(t.Tool) }

// Unwrap returns the SubAgent tool it wraps, so the agent recognises the call as a delegation:
// a saga rollback recurses into its sub-run, and the tree's token budget counts it.
func (t *attenuatingSubAgent) Unwrap() agent.Tool { return t.Tool }

func init() {
	// Every tool call in a delegation's sub-run is refused once the delegation's grant has expired
	// (see toolhook.CallGuard), so the sub-run cannot act past its grant's NotAfterUnix. A grant
	// bound by the caller (WithGrant) is checked when a delegation mints from it instead.
	toolhook.CallGuard = func(ctx context.Context) error {
		c, ok := ctx.Value(grantCtxKey{}).(grantCarrier)
		if ok && c.delegated && !c.absent && c.sg.Grant.Expired(time.Now().Unix()) {
			return fmt.Errorf("audit: delegation %q cannot act: its grant %q expired at %d: %w", c.sg.Grant.Subject, c.sg.Grant.ID, c.sg.Grant.NotAfterUnix, agent.ErrConfig)
		}
		return nil
	}
}

// unrecorded marks err as a refusal the agent records nothing for (see toolhook.Unrecorded): the
// run stops with it, and a re-drive under the right authority continues the delegation.
func unrecorded(err error) error { return &toolhook.Unrecorded{Err: err} }

// storageFailure is a failure to read or write the delegation's authority in the store. It says
// nothing about the delegation, so Call records nothing for it (unrecorded), as a plain SubAgent
// records nothing for a sub-run whose journal it cannot read: a resume retries the delegation.
type storageFailure struct{ err error }

func (e *storageFailure) Error() string { return e.err.Error() }
func (e *storageFailure) Unwrap() error { return e.err }

// authorityErr is err as Call returns it: a storage failure unrecorded, anything else as it is.
func authorityErr(err error) error {
	if _, ok := errors.AsType[*storageFailure](err); ok {
		return unrecorded(err)
	}
	return err
}

// ungrantedLeafName names the leaf a delegation journals in its sub-run when it runs without a
// grant, so a rollback can tell a delegation that ran without one from a sub-run it cannot read.
const ungrantedLeafName = "audit:delegation:ungranted"

// journaledAuthority reads what the sub-run subRunID journaled about the authority its delegation
// ran under: its one grant (nil if none), whether it recorded running without one, and whether the
// sub-run has any records at all. Only value records (the kind RecordGrant and the ungranted
// marker write) count as authority. More than one grant leaf is an error. A failure to read the
// sub-run is a *storageFailure.
func (t *attenuatingSubAgent) journaledAuthority(ctx context.Context, subRunID string) (grant *SignedGrant, ungranted, any bool, err error) {
	recs, err := t.store.History(ctx, subRunID)
	if err != nil {
		return nil, false, false, &storageFailure{fmt.Errorf("audit: read the authority of delegation %q (sub-run %s): %w", t.name, subRunID, err)}
	}
	for _, r := range recs {
		switch {
		case r.Kind != agent.StepValue:
			continue
		case r.Name == ungrantedLeafName:
			ungranted = true
		case strings.HasPrefix(r.Name, grantLeafPrefix):
			if grant != nil {
				return nil, false, true, fmt.Errorf("audit: delegation %q (sub-run %s) journaled more than one grant; the rollback cannot tell which it ran under: %w", t.name, subRunID, agent.ErrProtocol)
			}
			var sg SignedGrant
			if err := json.Unmarshal(r.Result, &sg); err != nil {
				return nil, false, true, fmt.Errorf("audit: decode the grant of delegation %q (sub-run %s): %w (%w)", t.name, subRunID, err, agent.ErrProtocol)
			}
			grant = &sg
		}
	}
	if grant != nil && ungranted {
		return nil, false, true, fmt.Errorf("audit: delegation %q (sub-run %s) journaled both a grant and running without one: %w", t.name, subRunID, agent.ErrProtocol)
	}
	return grant, ungranted, len(recs) > 0, nil
}

// checkChild verifies a journaled child grant against the grant and signer bound to ctx: its
// signature under the signer's key, and that it is a valid attenuation of the bound parent
// (CheckAttenuation under the delegation's rules).
func (t *attenuatingSubAgent) checkChild(child SignedGrant, parent SignedGrant, signer Signer) error {
	v, err := NewVerifier(signer.Alg(), signer.PublicKey())
	if err != nil {
		return fmt.Errorf("audit: delegation %q: the bound signer gives no verifier: %w", t.name, err)
	}
	if err := child.Verify(v); err != nil {
		return fmt.Errorf("audit: delegation %q: its journaled grant does not verify under the bound signer's key: %w", t.name, err)
	}
	if err := CheckAttenuation(parent.Grant, child.Grant, t.rules); err != nil {
		return fmt.Errorf("audit: delegation %q: its journaled grant is not an attenuation of the bound grant: %w (%w)", t.name, err, ErrNotVerified)
	}
	return nil
}

// BindRollback returns the context a saga rollback compensates the sub-run subRunID under: the
// authority the delegation ran under, read from the sub-run's journal, never the parent's.
//   - A journaled grant is used only once it is verified: its signature under the key of the
//     signer bound to ctx, and that it attenuates the grant bound to ctx. The identity and grant
//     are then rebound as Call bound them (Actor this sub-agent, OnBehalfOf the grant's issuer,
//     AuthorityRef its digest). With no grant and signer bound, it cannot be verified, and the
//     rollback stops (ErrConfig): bind them (WithGrant) to the rollback's context.
//   - A delegation that journaled running without a grant is compensated with no grant bound, even
//     if one is bound to ctx. A sub-run with no records has nothing to compensate, and is bound
//     the same way.
//   - Anything else (records but no journaled authority, two grants) stops the rollback rather
//     than guess.
func (t *attenuatingSubAgent) BindRollback(ctx context.Context, subRunID string) (context.Context, error) {
	child, ungranted, any, err := t.journaledAuthority(ctx, subRunID)
	if err != nil {
		return nil, err
	}
	switch {
	case child == nil && (ungranted || !any):
		return withoutGrant(ctx), nil
	case child == nil:
		return nil, fmt.Errorf("audit: delegation %q (sub-run %s) journaled no authority, so the rollback cannot establish what it ran under: %w", t.name, subRunID, agent.ErrProtocol)
	case child.Grant.Subject != t.name:
		// Call refuses the same grant: it was not issued to this sub-agent.
		return nil, fmt.Errorf("audit: delegation %q (sub-run %s) journaled a grant for subject %q: %w", t.name, subRunID, child.Grant.Subject, agent.ErrProtocol)
	}
	parentSG, signer, ok := GrantFrom(ctx)
	if !ok || signer == nil {
		return nil, fmt.Errorf("audit: delegation %q (sub-run %s) ran under a grant, and the rollback has no grant and signer bound to verify it against (see WithGrant): %w", t.name, subRunID, agent.ErrConfig)
	}
	if err := t.checkChild(*child, parentSG, signer); err != nil {
		return nil, err
	}
	ctx = agent.ContextWithIdentity(ctx, agent.Identity{
		Actor:        t.name,
		OnBehalfOf:   child.Grant.Issuer, // the parent's subject: CheckAttenuation required it
		AuthorityRef: child.Grant.Digest(),
	})
	return WithGrant(ctx, *child, signer), nil
}

func (t *attenuatingSubAgent) Call(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	// The sub-run ID must be unique to this call, or delegations from different parent runs would
	// share one journal (and memoize to each other's results). Only the agent loop's run scope is.
	var subRunID string
	if info, ok := agent.RunInfoFrom(ctx); ok && info.ToolUseID != "" {
		subRunID = agent.SubRunID(info.RunID, info.ToolUseID)
	}
	parentSG, signer, ok := GrantFrom(ctx)
	if !ok {
		// No grant to attenuate from: plain delegation, inheriting the caller's identity. Inside a
		// run it is journaled, so a rollback compensates the sub-run with no grant bound.
		if subRunID != "" {
			existing, _, _, err := t.journaledAuthority(ctx, subRunID)
			if err != nil {
				return nil, authorityErr(err)
			}
			if existing != nil {
				return nil, unrecorded(fmt.Errorf("audit: delegation %q (sub-run %s) began under a grant; resume it with the grant and signer bound (WithGrant): %w", t.name, subRunID, agent.ErrConfig))
			}
			if _, err := t.store.Do(ctx, subRunID, ungrantedLeafName, func(context.Context) (agent.Record, error) {
				return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`{"ungranted":true}`)}, nil
			}); err != nil {
				return nil, unrecorded(fmt.Errorf("audit: record that delegation %q ran without a grant: %w", t.name, err))
			}
		}
		return t.Tool.Call(ctx, args)
	}
	// A delegation re-entered on resume (its sub-run paused, or was cut off) runs under the grant it
	// journaled the first time, so the sub-run holds one grant whatever Narrow returns now.
	var existing *SignedGrant
	if subRunID != "" {
		var ungranted, any bool
		var err error
		if existing, ungranted, any, err = t.journaledAuthority(ctx, subRunID); err != nil {
			return nil, authorityErr(err)
		}
		switch {
		case ungranted:
			return nil, unrecorded(fmt.Errorf("audit: delegation %q (sub-run %s) began without a grant; resume it with none bound: %w", t.name, subRunID, agent.ErrConfig))
		case existing == nil && any:
			// The sub-run ran without journaling its authority (an earlier pre-release's ungranted
			// delegation): a grant minted now would cover steps that ran without one.
			return nil, unrecorded(fmt.Errorf("audit: delegation %q (sub-run %s) has records but no journaled authority; it cannot be given a grant now: %w", t.name, subRunID, agent.ErrConfig))
		}
	}
	now := time.Now().Unix()
	var childSG SignedGrant
	if existing != nil {
		if existing.Grant.Subject != t.name {
			return nil, fmt.Errorf("audit: delegation %q (sub-run %s) journaled a grant for subject %q: %w", t.name, subRunID, existing.Grant.Subject, agent.ErrProtocol)
		}
		if err := t.checkChild(*existing, parentSG, signer); err != nil {
			// Resumed under another grant or signer than the delegation began with: bind those and
			// drive again.
			return nil, unrecorded(fmt.Errorf("%w (%w)", err, agent.ErrConfig))
		}
		if existing.Grant.Expired(now) {
			// Permanent: no grant can renew a journaled one, so the delegation fails, recorded, and
			// a saga rolls back (BindRollback does not check expiry).
			return nil, fmt.Errorf("audit: delegation %q (sub-run %s) cannot continue: its grant expired at %d: %w", t.name, subRunID, existing.Grant.NotAfterUnix, agent.ErrConfig)
		}
		childSG = *existing
	} else {
		if parentSG.Grant.Expired(now) {
			// The bound grant expired: bind a live one and drive again.
			return nil, unrecorded(fmt.Errorf("audit: attenuating delegation to %q: the bound grant expired at %d: %w", t.name, parentSG.Grant.NotAfterUnix, agent.ErrConfig))
		}
		child := t.narrow(parentSG.Grant, t.name)
		child.ParentRef = parentSG.Grant.Digest()
		if child.Issuer == "" {
			child.Issuer = parentSG.Grant.Subject // the parent is the issuer of its child grant
		}
		if child.Subject == "" {
			child.Subject = t.name
		}
		if child.Subject != t.name {
			return nil, fmt.Errorf("audit: attenuating delegation to %q: the child grant's subject is %q; it must be the sub-agent's name: %w", t.name, child.Subject, agent.ErrConfig)
		}
		if child.NotAfterUnix == 0 {
			child.NotAfterUnix = parentSG.Grant.NotAfterUnix // a child never outlives its parent
		}
		if err := CheckAttenuation(parentSG.Grant, child, t.rules); err != nil {
			return nil, fmt.Errorf("audit: attenuating delegation to %q: %w", t.name, err)
		}
		if child.Expired(now) { // the AttenuateFunc's choice, under a live parent: a failure
			return nil, fmt.Errorf("audit: attenuating delegation to %q: its child grant expires at %d, already past: %w", t.name, child.NotAfterUnix, agent.ErrConfig)
		}
		if subRunID == "" {
			return nil, fmt.Errorf("audit: attenuating delegation to %q needs a run scope: call it from an agent run", t.name)
		}
		var err error
		if childSG, err = SignGrant(child, signer); err != nil {
			return nil, fmt.Errorf("audit: attenuating delegation to %q: %w", t.name, err)
		}
		// Anchor the child grant in the sub-run's journal so the delegation is provable in the tree.
		if _, err := RecordGrant(ctx, t.store, subRunID, childSG); err != nil {
			return nil, unrecorded(fmt.Errorf("audit: record child grant for %q: %w", t.name, err))
		}
	}

	// Rebind the sub-run: it acts as this sub-agent, on behalf of the parent, under the child grant.
	// Propagate the child grant so a deeper delegation attenuates from it in turn.
	ctx = agent.ContextWithIdentity(ctx, agent.Identity{
		Actor:        t.name,
		OnBehalfOf:   parentSG.Grant.Subject,
		AuthorityRef: childSG.Grant.Digest(),
	})
	ctx = context.WithValue(ctx, grantCtxKey{}, grantCarrier{sg: childSG, signer: signer, delegated: true})

	return t.Tool.Call(ctx, args)
}
