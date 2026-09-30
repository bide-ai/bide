# Delegation and capability attenuation

When one agent delegates to a sub-agent, its authority should shrink, never grow. The `audit`
package makes that provable: authority is carried in signed **grants** that hash-link into a
delegation chain, and a verifier can confirm a sub-agent's authority descends, unbroken and
never widened, from a root principal, each hop signed. This page covers minting and signing
grants, verifying a chain, attenuating by default at the sub-agent seam, and driving a grant's
scope from an agent's track record.

## Grants: the unit of authority

A `Grant` is a statement by a principal (`Issuer`) authorizing an actor (`Subject`) to act
within a `Scope` until `NotAfterUnix` (Unix seconds). It is the non-repudiable authority behind a governed action:
the action's `agent.Identity.AuthorityRef` is a grant's `Digest`.

<!-- docsnip: setup signer audit.Signer -->
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
to the operator that runs the log. `Grant.Bytes` is the grant's canonical form, the
`bide.audit.grant.v2` tag followed by its JSON, and both the signature and `Digest` (a SHA-256 over
those bytes, recomputable by any verifier from the disclosed grant, and what `Identity.AuthorityRef`
points at) cover it. `Grant.Expired(nowUnix)` reports expiry against a caller-supplied clock (a zero
`NotAfterUnix` never expires), so verification stays deterministic. The digest and signature cover
the grant's JSON encoding, which is one-to-one only over valid UTF-8, so `SignGrant` refuses, and
`SignedGrant.Verify` rejects, a grant with invalid UTF-8 in any string.

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

<!-- docsnip: setup chain []audit.SignedGrant; issuerVerifier func(issuer string) (audit.Verifier, bool) -->
```go
err := audit.VerifyDelegationChain(chain, issuerVerifier, audit.ScopeRules{"limit": audit.NumericAtMost})
// err == nil: the chain holds; errors.Is(err, audit.ErrNotVerified): a hop does not
```

It checks each grant's signature under its issuer's verifier (resolved via the `issuerVerifier`
callback, which maps an issuer name to its public key), that the root has no `ParentRef`, and that
every other grant is a valid delegation of the one before it. `CheckAttenuation(parent, child,
rules)` is that per-hop rule, and a child must satisfy all four parts:

1. **Linked**: `child.ParentRef` is `parent.Digest()`.
2. **Issued by the holder**: `child.Issuer` is `parent.Subject`. Only the principal a grant was
   given to can delegate it; anyone else holding a signing key cannot mint a child of it.
3. **Expires no later**: if the parent has a `NotAfterUnix`, the child has one too, at or before the
   parent's. A child of an expiring grant can never be non-expiring. A child of a non-expiring
   parent may set any expiry, or none.
4. **Keeps every constraint**: scope entries are constraints, each one restricting what the grant
   allows. The child must carry every key of the parent's scope, with the same value or a value the
   `ScopeRule` for that key accepts as narrower; a key with no rule can only be copied unchanged.
   The child may add keys the parent lacks (adding a constraint narrows). So dropping `tool: refund`
   from a child, which would let it call any tool, fails, as does raising a limit or changing a
   value no rule governs.

`NumericAtMost` is the rule for an integer upper bound: the child's value must parse as a base-10
integer no greater than the parent's, and unparseable values fail closed. A domain whose scope key
grants rather than restricts (say, a comma-separated allowlist of tools) supplies a rule that
requires a subset. The caller still checks two things the chain cannot: that the root's `Issuer`
is a principal it trusts to grant that authority, and that the leaf is unexpired at the time of use
(`Grant.Expired`, which needs a clock). A widening delegation cannot pass verification even if one
is minted.

## Attenuation by default: AttenuatingSubAgent

`AttenuatingSubAgent` wires attenuation into the sub-agent seam so narrowing is the default, not
something the caller has to remember. Bind the acting grant and signer once at the root with
`WithGrant`; then each delegation through the tool narrows automatically.

<!-- docsnip: setup ctx context.Context; store agent.Durable; rootSG audit.SignedGrant; signer audit.Signer; subAgent *agent.Agent -->
```go
ctx = audit.WithGrant(ctx, rootSG, signer)

tool := audit.AttenuatingSubAgent("researcher", "does research", subAgent, store,
    func(parent audit.Grant, subAgent string) audit.Grant {
        // return a narrower grant: keep every constraint, lower the limit.
        scope := maps.Clone(parent.Scope)
        scope["limit"] = "100"
        return audit.Grant{Scope: scope}
    },
    audit.ScopeRules{"limit": audit.NumericAtMost})
```

On each call, if a signed grant is bound to the context, the tool mints a narrower child grant
(`ParentRef` hash-linked to the parent, signed by the same signer), records it as a durable leaf
in the sub-run so the chain is anchored, rebinds the sub-run's identity to the child (`Actor` =
this sub-agent, `OnBehalfOf` = the parent's `Subject`, `AuthorityRef` = the child's digest), and
propagates the child grant so a deeper delegation narrows again. Your `AttenuateFunc` sets the
narrower `Scope` (and may set `Subject` and an earlier `NotAfterUnix`); the wrapper fills in
`ParentRef`, and `Issuer` (the parent's `Subject`), `Subject` (the sub-agent's name), and `NotAfterUnix`
(the parent's) if you left them empty. It then checks the child with `CheckAttenuation` under the
rules you pass, and refuses the delegation, signing nothing, if the child is not a valid
delegation: a widening `AttenuateFunc` is caught when it runs, not later by a verifier.

The child grant is recorded in the sub-run the agent loop gives this call (`agent.SubRunID(parentRunID,
toolUseID)`, unique per call), so each delegation's grant sits in its own journal. Called outside an agent run,
where there is no such scope, the tool refuses rather than fall back to a sub-run ID that every
parent run would share. With no grant on the context it is a plain sub-agent that inherits the
identity, so it is safe to use either way. The wrapped sub-agent still runs its own full agent loop and reasons
autonomously; only its authority shrinks. The result is that capabilities monotonically decrease
down a delegation tree by construction, and the whole chain stays provable via
`VerifyDelegationChain`.

## Earned authority: scope from track record

`EarnedAuthority` is a control loop that drives a grant's scope from the agent's provable track
record. Authority starts at the ladder's baseline rung, is promoted one rung after a clean streak
(capped at the top rung), and resets to baseline the instant an anomaly is flagged.

<!-- docsnip: setup ctx context.Context; rootSG audit.SignedGrant; signer audit.Signer; ledger agent.Durable -->
```go
ea, err := audit.NewEarnedAuthority(ctx,
    []int{100, 500, 1000}, // ladder: rung 0 is baseline; top must not exceed the root ceiling
    5,                     // promote one rung every 5 compliant actions
    rootSG, signer, "agent-1",
    ledger, "ledger/agent-1") // a store and run that hold only this controller's grants

promoted, _ := ea.RecordCompliant(ctx) // note one clean action; may promote
changed, _  := ea.FlagAnomaly(ctx)     // reset to baseline immediately
grant := ea.Grant()                    // the current signed earned grant
```

Each change re-issues a signed grant that is a **child of the root** (`ParentRef` to the root,
every root scope constraint kept, `limit` = the current rung, and the root's `NotAfterUnix`), so the
earned limit provably never exceeds the root ceiling and no earned grant outlives the root:
`NewEarnedAuthority` rejects a ladder whose top rung exceeds the root's numeric `limit` scope, and
every earned grant passes `VerifyDelegationChain` against the root under `audit.EarnedRules`
(`limit` may only go down). So even a buggy or compromised controller cannot widen past what the
root principal authorized.

**Revocation.** A demotion has to take the higher grant out of use at once, not when it expires,
and an offline verifier has to be able to tell. Every grant the controller issues is appended to
the ledger run as an anchored grant leaf, and the **current** grant is the ledger's last leaf; every
earlier grant is superseded, whether the change was a promotion or a demotion. (Each issue's ID
names its ledger position, so re-reaching a rung issues a new grant rather than reviving an old
one.) A verifier checks a grant against the latest signed head of the ledger run:

<!-- docsnip: setup ctx context.Context; ledger agent.Durable; latestLedgerSTH audit.SignedTreeHead; lastSeen *audit.SignedTreeHead; grant audit.SignedGrant; logKey audit.Verifier -->
```go
// lastSeen is the newest ledger head this verifier has verified before; on first contact pass
// size 0 and a nil lastSeen.
proof, _ := audit.ProveCurrentGrant(ctx, ledger, "ledger/agent-1", latestLedgerSTH, lastSeen.Size)
err := audit.VerifyCurrentGrant(grant, "ledger/agent-1", proof, lastSeen, logKey) // nil: current
```

`VerifyCurrentGrant` requires the proof to be about the named ledger run (a grant that is the last
leaf of some other run signed by the same key, such as an agent run that records the grant it acts
under, proves nothing), to verify under the log key, its record to be the anchored leaf of exactly
this grant, and that leaf to be the last one (index `Size-1`) of the signed ledger head. When the
verifier passes its last-seen ledger head, the proof must also carry a consistency proof showing
the presented head extends it, so a head older than one the verifier already saw fails.

The verifier learns the latest ledger head the way it learns any run's state: it takes the newest
head of the ledger run from the anchor log (anchor the ledger through an `AuditedStore`, or sign
and publish its heads), confirms the head is in the anchor log, and keeps the newest head it has
verified as `lastSeen` for its next check. Without a last-seen head, a proof against an older head
shows only that the grant was current then. Combine it with `VerifyDelegationChain` (the grant
descends from the root) and `Grant.Expired` (it is in date).

The asymmetry is the safety property: promotion is slow, capped, and evidence-gated; attenuation
(the anomaly reset) is immediate and ungated, because shrinking authority is always safe. It is
deliberately a durable, sequential controller rather than a convergent machine, because earning is
temporal and order-dependent (a promotion does not commute with a compliant action); the
enforcement of the limit it sets stays a convergent governance invariant on the work machine.

## See also

- `audit/grant.go`: `Grant`, `SignGrant`, `RecordGrant` / `ProveGrant`, `CheckAttenuation`,
  `ScopeRules`, `NumericAtMost`, `VerifyDelegationChain`.
- `audit/delegate.go`: `WithGrant`, `AttenuatingSubAgent`, `AttenuateFunc`.
- `audit/earned.go`: `EarnedAuthority`, `EarnedRules`, `ProveCurrentGrant`, `VerifyCurrentGrant`.
- `examples/govern/delegation`, `examples/govern/authority`, `examples/govern/earned-authority`: runnable end to end.
- `docs/guides/security-model.md`: how grant signatures fit the overall trust model.
