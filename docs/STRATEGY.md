# Strategy and positioning (one page)

Status: working hypothesis, not a commitment. Revised as design partners teach us what sells.

## Category

Not "an agent framework." The **accountability layer for autonomous agents**. The framework is
how we get in the door; the category we intend to own is provable, auditable agent behavior.
Competing as a framework means being compared to LangChain on features and losing; competing as
the accountability layer is an uncontested position.

## Positioning line

**Prove what your agent did, and that it could not have gone wrong, to anyone, without trusting us.**

## What is actually uncontested

We can make an agent's behavior provable to an outside party, not merely observable to its
operator. Actions are journaled at-most-once, admitted under a governed policy that is
machine-checked to converge, and committed to a tamper-evident log that a third party verifies
offline with a checker we did not write. Two independent roots of trust, cryptographic and
mathematical, over one artifact. No agent framework pairs these; the formal-methods world has the
proofs but no agent product; Temporal has durable execution but no governance or proof story.

## Wedge

- **Vertical (hypothesis):** fintech back-office money movement (payments, ledger, reconciliation).
  At-most-once maps to "never double-execute a payment"; audit plus convergence maps to
  reconciliation and regulatory audit. Legal is the fallback (the "prove what the agent did" pitch
  is native there).
- **Buyer:** not the bank. The company building agents that touch money or ledgers, blocked from
  shipping by their customer's compliance or audit bar.
- **Validation gate:** one design partner with a *currently blocked* deployment, not a survey.
  The first willing blocked partner chooses the final vertical.

## Business shape (open-core)

- **Open and free:** the framework and, critically, the verifier. The "independent trust root"
  claim is void if we control the checker, so open-sourcing it is the credibility that makes the
  paid layer sellable, not a giveaway.
- **Paid:** the anchor service (we operate the external transparency log, transparency-log-as-a-
  service for agents), managed audit retention, the compliance dashboard, verified policy packs per
  vertical, enterprise support and certification.
- **Architecture that enforces this:** the SDK owns the production runtime; the research artifact
  (gsm) and the axiom-free proof are the open reference the runtime is differentially tested
  against, never a production dependency. The one durable, versioned asset is the policy
  interchange format and its pin to the proof, a contract all repos conform to.

## Claim scope (the defensibility spine, do not cross)

- Say: convergent policy (machine-checked), tamper-evident append-only log, runtime
  differentially tested against a verified reference, third-party offline verification, two
  independent trust roots.
- Do not say: "verified execution end to end." The refinement gap (the running Go refines the Coq
  model for all inputs) is open; the runtime is tested against the reference, not proven equal to
  it. Say so plainly.
- Do not say: "agents always agree." The proven claim is order-independent convergence of the
  replay to a canonical valid state, an endpoint property, not per-step agreement.
- Lead with the outcome ("prove to your regulator what your agent did"), never the machinery. The
  proof is the backing, not the pitch. No theorem counts in the sales motion.

## Risks that decide landmark versus orphan

1. **Timing.** Demand for provable, auditable agents arrives with regulation and with the first
   public agent-caused incidents. Build for the buyer already living in that future, not for the
   average of today's market.
2. **Adoption friction.** Formal methods read as academic and shrink the talent pool. Keep hiding
   the proof machinery behind the ergonomic surface (combinators, sugar, no porting to Coq).
3. **The framework is not the revenue.** Identity is the accountability layer; the framework is
   distribution.

## Near-term proof-points

1. One blocked design partner in hand (the only real validation).
2. The demoable slice: governance-aware audit leaf plus `verify-governance`, so a partner's
   auditor verifies a bundle offline in the room. That demo closes it.
3. A hosted anchor others can point at, the first recurring-revenue surface.

## Non-goals

- Out-featuring general frameworks on capability.
- Selling directly to end regulated enterprises before a picks-and-shovels buyer validates the wedge.
- Any public claim the proofs do not currently support.
