# Governed model quorum (design note)

Status: design, not implemented. This note records how a k-of-n model quorum is expressed on the
existing seams (not as a new agent type), so it can be built as a composition helper rather than a
bespoke agent.

## What it is, and how it differs from hedging

A quorum fires the same decision to N models, then requires **governed agreement** (at least k of n
must produce the same decision) before the action commits. Disagreement blocks the action or
escalates it to a human, and the whole vote is in the audit trail.

Hedging (`middleware.Hedge`) takes the *first* successful answer, for tail latency and availability.
A quorum requires *agreement*, for correctness and risk on a high-stakes decision. Different goal,
different tool.

## Not a new agent type: a composition of seams

This is the design principle in action. A quorum is not a `QuorumAgent`; it is a recipe over the
primitives the SDK already has:

1. **Fan-out (concurrency seam)**: get N independent decisions. Run the same prompt across N models
   with `Parallel` (each a durable `Step`) or `errgroup`. Prefer different providers/families: a
   quorum reduces single-model risk only to the extent the models fail independently.
2. **Normalize (typed output)**: the votes must be comparable, so each model returns a discrete
   decision (an enum label, a value, a yes/no, a tool choice) via forced structured output
   (`RunTypedNative` / `ResponseFormat`, or a forced tool choice). Free-form prose cannot be quorumed.
3. **Tally (plain Go)**: count agreement on the normalized decision. No framework machinery; this
   is a few lines that produce "the winning value and how many models chose it".
4. **Govern (governance seam)**: express the k-of-n requirement as a `gsm` invariant so the commit
   is provably gated (below). A failed quorum is a blocked or compensated action, not a silent pick.
5. **Escalate (HITL seam)**: on disagreement, route to a human via `agent.Safety{RequiresApproval:
   true}` (dual control), or fall back to a conservative default, deterministically.
6. **Audit (audit seam)**: record each model's vote and the tally so the `ProofBundle` commits to
   why the action committed or escalated.

Every step is an existing seam. If a future pattern cannot be written this way, that is the signal
it needs a new primitive, not a new agent class.

## Quorum as an invariant

This mirrors authority-as-governed-state (see [GOVERNANCE.md](GOVERNANCE.md)): an external fact (the
tally) is seeded into governed state, and an invariant makes the gate provable. One policy, verified
over every possible vote count.

```go
decision := r.Enum("decision", "approve", "deny", "escalate")
votesFor := r.Int("votes_for", 0, n) // how many models chose the committed decision
committed := r.Bool("committed")

// The gate: a decision may be committed only with quorum. Verified over every (votes_for) value.
r.Rule("quorum_required").
    Require(gsm.Or(gsm.Is(committed, 0), gsm.Ge(gsm.V(votesFor), gsm.Lit(k)))). // committed => votes_for >= k
    RepairWith(gsm.SetLabel(decision, "escalate")).                             // no quorum: escalate
    Add()

// Pre-commit guard: the commit event is a no-op unless quorum is met.
r.On("commit").OnlyIf(gsm.Ge(gsm.V(votesFor), gsm.Lit(k))).Does(gsm.SetBoolTrue(committed)).Add()
```

The deployment runs the fan-out, tallies, seeds `votes_for`, and applies `commit`. If quorum holds,
the decision commits under an `AttestedEventTool` (leaf binds the decision, the policy, and the
vote); if not, the invariant forces `escalate` and the action pauses for approval. The exact combinator
names (`Ge`, `SetLabel`, and a boolean-set transform) are illustrative; the shape is what matters.

## Design decisions

- **k**: unanimous (k = n) is the most conservative; majority (k = ceil(n/2)) balances cost and
  safety. The invariant encodes k; a tie or no-majority resolves to escalate.
- **Diversity over count**: three different providers beats one model called three times. Same-model
  repetition measures that model's own variance (useful against stochastic flakiness, weaker against
  a systematic error).
- **Disagreement policy is explicit**: block, escalate to a named approver, or conservative default,
  chosen as the invariant's repair plus `RequiresApproval`, deterministic and on the record.
- **Vote provenance**: record each model's identity (which provider/version) alongside its vote, so
  the audit shows not just the tally but who voted how (composes with `agent.Identity`).

## The boundary (state it, do not oversell)

- **The gate is provable; the agreement is not.** The k-of-n invariant holds for all inputs and is
  machine-checked. Whether the agreed decision is *correct* is statistical, and correlated model
  errors mean agreement is not statistical independence. A quorum lowers single-model risk; it does
  not certify the answer. This is the same model-versus-governed-boundary discipline as everywhere.
- **High-stakes gate only.** It costs N generations per gated decision and its latency is the k-th
  response. Apply it to the few irreversible or expensive decisions, not the whole loop.
- **Structured decisions only.** Quorum needs a normalizable output; it does not apply to open-ended
  generation.

## Staged build

1. A `govern.Quorum` helper that takes N models (or handlers) and a decision schema, fans out with
   `Parallel`, tallies, and returns the winning value plus the count, so the tally is reusable.
2. Wire the count into a `gsm` invariant as above and commit through `AttestedEventTool`, with
   `RequiresApproval` on the escalate path.
3. Extend the attested leaf (or add vote leaves) so the `ProofBundle` commits to each vote and the
   tally, and a `goagents-audit` verify path can confirm "committed under k-of-n agreement".

Built this way it is a composition helper on the seams, testable and auditable like the rest, and
the one `Agent` stays unchanged.
