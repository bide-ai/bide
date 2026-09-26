# Delegation and capability attenuation

When one agent delegates to a sub-agent, its authority should shrink, never grow. The `audit`
package makes that provable: authority is carried in signed **grants** that hash-link into a
delegation chain, and a verifier can confirm a sub-agent's authority descends, unbroken and
never widened, from a root principal, each hop signed. This page covers minting and signing
grants, verifying a chain, attenuating by default at the sub-agent seam, and driving a grant's
scope from an agent's track record.

## Grants: the unit of authority

A `Grant` is a statement by a principal (`Issuer`) authorizing an actor (`Subject`) to act
within a `Scope` until `NotAfter`. It is the non-repudiable authority behind a governed action:
the action's `agent.Identity.AuthorityRef` is a grant's `Digest`.

```go
g := audit.Grant{
    ID:      "root",
    Issuer:  "principal-alice",
    Subject: "agent-1",
    Scope:   map[string]string{"limit": "1000"},
}
sg, err := audit.SignGrant(g, signer) // signer: Ed25519, ML-DSA, or hybrid
```

`SignGrant` signs the grant with the issuer's own key (an `audit.Signer`), which is deliberately
distinct from the log's tree-head key, so the authorization is attributable to the principal, not
to the operator that runs the log. `Digest` is a stable, domain-separated SHA-256 over the grant,
recomputable by any verifier from the disclosed grant, and is what `Identity.AuthorityRef` points
at. `Grant.Expired(now)` reports expiry against a caller-supplied clock (a zero `NotAfter` never
expires), so verification stays deterministic.

`RecordGrant` anchors a signed grant as a dedicated journal leaf keyed by its digest (idempotent
per run), so it is covered by the same signed tree head and inclusion proofs as the actions taken
under it; `ProveGrant` builds a `ProofBundle` for that leaf. Pair it with an action's bundle whose
`AuthorityRef` matches the digest to show the action ran under an in-log, issuer-signed authority.

## ParentRef and the delegation chain

`ParentRef` hash-links a grant to the one it was attenuated from (empty for a root grant). A
holder of a grant may mint a strictly narrower sub-grant for a sub-agent, setting `ParentRef` to
the parent's `Digest()`, without returning to the root issuer. That is capability attenuation, and
the chain back to the root stays provable.

`VerifyDelegationChain` verifies a chain ordered root-first (index 0) to leaf-last:

```go
ok, err := audit.VerifyDelegationChain(chain, issuerVerifier, audit.AttenuatesNumericScope("limit"))
```

It checks three things per hop:

1. each grant's signature under its issuer's verifier (resolved via the `issuerVerifier`
   callback, which maps an issuer name to its public key);
2. the root has no `ParentRef`, and every other grant's `ParentRef` equals the previous grant's
   digest (the chain is unbroken);
3. every non-root grant is an attenuation of its parent, if an `Attenuator` is supplied.

An `Attenuator` decides what "narrower" means for your domain. `AttenuatesNumericScope(key)`
covers the common "lower the limit" case: it requires `child.Scope[key] <= parent.Scope[key]`
parsed as integers, and **fails closed** on missing or unparseable values, so a sub-grant cannot
widen authority by dropping or corrupting a bound. Expiry is left to the caller (`Grant.Expired`)
since it needs a clock. A widening delegation cannot pass verification even if one is minted.

## Attenuation by default: AttenuatingSubAgent

`AttenuatingSubAgent` wires attenuation into the sub-agent seam so narrowing is the default, not
something the caller has to remember. Bind the acting grant and signer once at the root with
`WithGrant`; then each delegation through the tool narrows automatically.

```go
ctx = audit.WithGrant(ctx, rootSG, signer)

tool := audit.AttenuatingSubAgent("researcher", "does research", subAgent, store,
    func(parent audit.Grant, subAgent string) audit.Grant {
        // return a STRICTLY narrower grant: lower the limit.
        return audit.Grant{Scope: map[string]string{"limit": "100"}}
    })
```

On each call, if a signed grant is bound to the context, the tool mints a narrower child grant
(`ParentRef` hash-linked to the parent, signed by the same signer), records it as a durable leaf
in the sub-run so the chain is anchored, rebinds the sub-run's identity to the child (`Actor` =
this sub-agent, `OnBehalfOf` = the parent's `Subject`, `AuthorityRef` = the child's digest), and
propagates the child grant so a deeper delegation narrows again. Your `AttenuateFunc` sets the
narrower `Scope` (and may set `Subject`); the wrapper fills in `ParentRef`, and `Issuer` /
`Subject` if you left them empty, then signs.

With no grant on the context it is a plain sub-agent that inherits the identity, so it is safe to
use either way. The wrapped sub-agent still runs its own full agent loop and reasons
autonomously; only its authority shrinks. The result is that capabilities monotonically decrease
down a delegation tree by construction, and the whole chain stays provable via
`VerifyDelegationChain`.

## Earned authority: scope from track record

`EarnedAuthority` is a control loop that drives a grant's scope from the agent's provable track
record. Authority starts at the ladder's baseline rung, is promoted one rung after a clean streak
(capped at the top rung), and resets to baseline the instant an anomaly is flagged.

```go
ea, err := audit.NewEarnedAuthority(
    []int{100, 500, 1000}, // ladder: rung 0 is baseline; top must not exceed the root ceiling
    5,                     // promote one rung every 5 compliant actions
    rootSG, signer, "agent-1")

promoted, _ := ea.RecordCompliant() // note one clean action; may promote
changed, _  := ea.FlagAnomaly()     // reset to baseline immediately
grant := ea.Grant()                 // the current signed earned grant
```

Each change re-issues a signed grant that is a **child of the root** (`ParentRef` to the root,
scope limit = the current rung), so the earned limit provably never exceeds the root ceiling:
`NewEarnedAuthority` rejects a ladder whose top rung exceeds the root's numeric `limit` scope, and
every earned grant passes `VerifyDelegationChain` against the root under
`AttenuatesNumericScope("limit")`. So even a buggy or compromised controller cannot widen past
what the root principal authorized.

The asymmetry is the safety property: promotion is slow, capped, and evidence-gated; attenuation
(the anomaly reset) is immediate and ungated, because shrinking authority is always safe. It is
deliberately a durable, sequential controller rather than a convergent machine, because earning is
temporal and order-dependent (a promotion does not commute with a compliant action); the
enforcement of the limit it sets stays a convergent governance invariant on the work machine.

## See also

- `audit/grant.go`: `Grant`, `SignGrant`, `RecordGrant` / `ProveGrant`, `AttenuatesNumericScope`,
  `VerifyDelegationChain`.
- `audit/delegate.go`: `WithGrant`, `AttenuatingSubAgent`, `AttenuateFunc`.
- `audit/earned.go`: `EarnedAuthority`.
- `examples/delegation`, `examples/authority`, `examples/earned-authority`: runnable end to end.
- `docs/guides/security-model.md`: how grant signatures fit the overall trust model.
