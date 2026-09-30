package audit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bide-ai/bide/agent"
)

// grantCtxKey carries the acting principal's signed grant plus the signer used to mint attenuated
// child grants, so AttenuatingSubAgent can narrow authority automatically down a delegation tree.
type grantCtxKey struct{}

type grantCarrier struct {
	sg     SignedGrant
	signer Signer
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
	if !ok {
		return SignedGrant{}, nil, false
	}
	return c.sg, c.signer, true
}

// AttenuateFunc derives a child grant from the parent grant and the delegating sub-agent's name.
// It sets the narrower Scope (and may set Subject and an earlier NotAfterUnix); AttenuatingSubAgent
// fills in ParentRef and, if empty, Issuer (the parent's Subject), Subject (the sub-agent name), and
// NotAfterUnix (the parent's), then checks the result with CheckAttenuation before signing it, so a
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
func AttenuatingSubAgent(name, description string, sub *agent.Agent, store agent.Durable, narrow AttenuateFunc, rules ScopeRules) agent.Tool {
	return &attenuatingSubAgent{
		Tool:   agent.SubAgent(name, description, sub),
		name:   name,
		store:  store,
		narrow: narrow,
		rules:  rules,
	}
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

func (t *attenuatingSubAgent) Call(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	parentSG, signer, ok := GrantFrom(ctx)
	if !ok {
		return t.Tool.Call(ctx, args) // no grant to attenuate from: plain delegation, inherit identity
	}

	child := t.narrow(parentSG.Grant, t.name)
	child.ParentRef = parentSG.Grant.Digest()
	if child.Issuer == "" {
		child.Issuer = parentSG.Grant.Subject // the parent is the issuer of its child grant
	}
	if child.Subject == "" {
		child.Subject = t.name
	}
	if child.NotAfterUnix == 0 {
		child.NotAfterUnix = parentSG.Grant.NotAfterUnix // a child never outlives its parent
	}
	if err := CheckAttenuation(parentSG.Grant, child, t.rules); err != nil {
		return nil, fmt.Errorf("audit: attenuating delegation to %q: %w", t.name, err)
	}

	// The sub-run ID must be unique to this call, or delegations from different parent runs would
	// share one journal (and memoize to each other's results). Only the agent loop's run scope is.
	subRunID := agent.RunScope(ctx)
	if subRunID == "" {
		return nil, fmt.Errorf("audit: attenuating delegation to %q needs a run scope: call it from an agent run", t.name)
	}
	childSG, err := SignGrant(child, signer)
	if err != nil {
		return nil, fmt.Errorf("audit: attenuating delegation to %q: %w", t.name, err)
	}
	// Anchor the child grant in the sub-run's journal so the delegation is provable in the tree.
	if _, err := RecordGrant(ctx, t.store, subRunID, childSG); err != nil {
		return nil, fmt.Errorf("audit: record child grant for %q: %w", t.name, err)
	}

	// Rebind the sub-run: it acts as this sub-agent, on behalf of the parent, under the child grant.
	// Propagate the child grant so a deeper delegation attenuates from it in turn.
	ctx = agent.WithIdentity(ctx, agent.Identity{
		Actor:        t.name,
		OnBehalfOf:   parentSG.Grant.Subject,
		AuthorityRef: child.Digest(),
	})
	ctx = WithGrant(ctx, childSG, signer)

	return t.Tool.Call(ctx, args)
}
