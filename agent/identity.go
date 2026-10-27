package agent

import (
	"context"
	"fmt"
	"reflect"
)

// Identity is the accountability binding for a run: which agent instance acted, on whose behalf,
// and under what authority. It is assigned by the DEPLOYMENT, not by the model: the operator sets
// it at run start from its own auth layer (an IdP, a signed capability grant, a service identity),
// and it rides the context so every governed action and every sub-agent call inherits it. The SDK
// anchors and proves the identity CLAIM (these fields, committed in the audit log); it does not
// authenticate the principal, which is the operator's IdP/PKI, and the attribution is only as
// strong as custody of whatever key signs the run's tree heads and grants.
//
// The model has no inherent identity. "The agent's identity" is the identity the deployment binds
// to this run, so a compromised or misconfigured deployment attributes to whatever it claims. That
// boundary is deliberate and matches the model/governance split: prove the record and the granted
// authority, do not overclaim the intent behind them.
type Identity struct {
	// Actor is the acting agent instance: a deployment plus model version, e.g. "exec-agent@1.4.2".
	Actor string `json:"actor,omitempty"`
	// OnBehalfOf is the principal the agent acts for: a desk, an account, a user.
	OnBehalfOf string `json:"on_behalf_of,omitempty"`
	// AuthorityRef identifies the grant that authorizes this action: the digest or id of a signed
	// capability/authority record (see audit anchoring), so the leaf links to what permitted it.
	AuthorityRef string `json:"authority_ref,omitempty"`
}

// Empty reports whether the identity carries no attribution at all.
func (i Identity) Empty() bool {
	return i.Actor == "" && i.OnBehalfOf == "" && i.AuthorityRef == ""
}

type identityKey struct{}

// contextWithIdentity binds an acting identity to ctx, for the run driven with it: a run binds its
// identity (the WithIdentity option, the run's or the agent's) here, and it propagates to tool
// calls and to sub-agents, so every governed action inherits the same attribution without
// threading it by hand. A delegation (package audit) rebinds its sub-run's through toolhook.
func contextWithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// IdentityFrom returns the acting identity bound to ctx, and whether one was set.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// flowRunIdentity is journalhook.RunIdentity: the identity a plan flow run's options give. A flow
// run takes WithIdentity, WithUntrustedInput and WithTrustedInput (see flowRunProvenance): its other
// settings (the turn limit, a tool filter, a saga, ...) are an agent run's.
func flowRunIdentity(opts []any) (actor, onBehalfOf, authorityRef string, set bool, err error) {
	var c runConfig
	for i, o := range opts {
		ro, ok := o.(RunOption)
		if !ok || isNil(ro) {
			return "", "", "", false, fmt.Errorf("plan flow run option %d is %T, not an agent.RunOption: %w", i, o, ErrConfig)
		}
		if err := ro.applyRun(&c); err != nil {
			return "", "", "", false, err
		}
		id, un, tr := c.identity, c.untrustedInput, c.trustedInput
		c.identity, c.untrustedInput, c.trustedInput = nil, nil, nil
		if !reflect.DeepEqual(c, runConfig{}) {
			return "", "", "", false, fmt.Errorf("plan flow run option %d: a flow run takes agent.WithIdentity, WithUntrustedInput and WithTrustedInput only: %w", i, ErrConfig)
		}
		c.identity, c.untrustedInput, c.trustedInput = id, un, tr
	}
	if c.identity == nil {
		return "", "", "", false, nil
	}
	return c.identity.Actor, c.identity.OnBehalfOf, c.identity.AuthorityRef, true, nil
}

// flowRunProvenance is journalhook.RunProvenance: where a plan flow run's options say its input came from.
// The options are checked as flowRunIdentity checks them; WithUntrustedInput together with WithTrustedInput
// is ErrConfig, as for an agent run.
func flowRunProvenance(opts []any) (untrusted, trusted *string, err error) {
	var c runConfig
	for i, o := range opts {
		ro, ok := o.(RunOption)
		if !ok || isNil(ro) {
			return nil, nil, fmt.Errorf("plan flow run option %d is %T, not an agent.RunOption: %w", i, o, ErrConfig)
		}
		if err := ro.applyRun(&c); err != nil {
			return nil, nil, err
		}
	}
	if c.untrustedInput != nil && c.trustedInput != nil {
		return nil, nil, fmt.Errorf("WithUntrustedInput and WithTrustedInput both state the run's input provenance; pass one: %w", ErrConfig)
	}
	return c.untrustedInput, c.trustedInput, nil
}
