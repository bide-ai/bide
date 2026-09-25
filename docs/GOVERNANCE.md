# Convergent governance (Tier-2)

How to let **multiple concurrent agents mutate shared state and still agree** — without a
lock, a leader, or a consensus round. This is the `govern` package. It's built on
[gsm](https://github.com/blackwell-systems/gsm) (Governed State Machines), which proves *at
build time* that every interleaving of agent actions converges to the same valid state.

> If you only need one agent's work to survive a crash without firing a side effect twice,
> you want the **durable core + sagas** (see the main README), not this. This tier is for
> *shared* state touched by *concurrent* agents.

## The two tiers, and when to use which

| | **Saga tier** (core) | **Governance tier** (`govern`) |
|---|---|---|
| Shape | one causal order of steps | many agents, any interleaving |
| On failure | reverse-order compensation (rollback) | forward compensation (repair to valid) |
| Guarantee | atomic-ish rollback across a sub-agent tree | order-independent convergence |
| Needs | a sequence to unwind | a shared state to keep consistent |
| Analogy | database transaction / saga | CRDT, but with business invariants |

Rule of thumb: **sequential work that must undo cleanly → saga. Concurrent agents sharing a
consistent view → governance.**

## The mental model

You describe shared state as a **registry**:

- **Variables** — the finite shared state (enums, bools, bounded ints).
- **Invariants** — what "valid" means (`Holds`), and how to **repair** a violation (`Repair`).
- **Events** — the actions agents can take (`Writes` / `Guard` / `Apply`).

`Build()` enumerates the state space and **proves** two properties: compensation always
terminates (WFC) and event order doesn't matter after repair (CC). If it can't prove them, it
**refuses to build** and hands you a counterexample. A built machine is an immutable set of
O(1) lookup tables — no compensation logic runs at runtime.

```go
r := gsm.NewRegistry("order")
status := r.Enum("status", "pending", "paid", "shipped")
paid := r.Bool("paid")
r.Invariant("no_ship_unpaid").Watches(status, paid).
    Holds(func(s gsm.State) bool { return s.Get(status) != "shipped" || s.GetBool(paid) }).
    Repair(func(s gsm.State) gsm.State { return s.Set(status, "pending") }).Add()
r.Event("pay").Writes(status, paid).
    Apply(func(s gsm.State) gsm.State { return s.Set(status, "paid").SetBool(paid, true) }).Add()
// ... ship event ...
m, report, err := r.Build() // proves convergence, or returns a counterexample
```

## Governors: the durable runtime

A **governor** applies agents' events to the shared state. Two flavors:

```go
gov := govern.New(m, m.NewState())                        // in-memory, thread-safe
pg, _ := govern.NewPersistent(ctx, m, log, "order-42", m.NewState()) // event-sourced
```

`PersistentGovernor` appends every event to an `EventLog` and **reconstructs state by
replaying the log** on startup — crash-recoverable. `EventLog` is a port; adapters:

- `govern.NewMemEventLog()` — in-memory (tests / local).
- `govern/sqlitelog` — on-disk SQLite.
- `govern/redislog` — Redis Streams (networked, "no SQL DB required").

## The agent boundary

`FederatedEventTool` (and `EventTool` for a single governor) turns an agent's **tool call**
into a governed event. This is how an LLM agent participates:

```go
tool := govern.EventTool(gov, "pay", "mark the order paid", "pay", agent.Safety{})
// give `tool` to the agent; when the LLM calls it, "pay" is applied to shared state,
// convergently, and durably.
```

Multiple agents sharing one governor converge no matter how their calls interleave.

## Identity and delegated authority

A governed action can commit to **who acted**, not just what happened. The deployment binds an
`agent.Identity{Actor, OnBehalfOf, AuthorityRef}` to the run (from its own auth layer, never from
the model), and `AttestedEventTool` stamps it into the same leaf as the policy and state digests:

```go
id := agent.Identity{Actor: "exec-agent@1.4.2", OnBehalfOf: "desk-EQ-US", AuthorityRef: "grant#a1b2"}
a.Run(agent.WithIdentity(ctx, id), runID, input) // propagates to governed tools and sub-agents
```

An inclusion proof then commits to who acted, on whose behalf, and under what authority. The SDK
proves the identity CLAIM; authenticating the principal is the operator's IdP/PKI, and the
attribution is only as strong as the key custody behind the run's signatures (see AUDIT.md).

**Authority as governed state.** The cleaner move is to make the delegated authority part of the
state the invariants read, so the rule is scoped per principal. Model the limit as a variable and
require `exposure <= limit`:

```go
exposure := r.Int("exposure", 0, 10)
limit    := r.Int("limit", 0, 10)              // seeded at run start from the verified grant
r.Rule("within_delegated_limit").
    Require(gsm.AtMostVar(exposure, limit)).
    RepairWith(gsm.Do(gsm.Set(exposure, gsm.V(limit)))).
    Add()
gov := govern.New(m, m.NewState().SetInt(limit, granted)) // the agent never sets its own limit
```

Because `limit` is a state variable, `Build` verifies the invariant exhaustively over every
`(exposure, limit)` pair: **one machine-checked policy covers every principal's limit at once**, and
each run is governed to the limit its grant seeded. See `examples/authority`.

## Federation: constraints across agents

When shared state spans **multiple registries** with cross-registry rules (one agent's state
constrains another's), connect them with **morphisms** into a `Federation`, and drive it with
a `FederatedGovernor`. The capability ladder:

- **Tree** — each target has one source; the source is *authoritative* over the target's
  shared component (a manufacturer's status fixes a supplier's listing). Coordination-free
  conflict resolution: the source wins, deterministically.
- **Multi-source (DAG)** — a target with several sources declares a **`Resolver`** that
  merges them (priority / AND-OR / most-restrictive):
  ```go
  fed.Morphism(hr, door).Shared(access).Map(...).Add().
      Morphism(security, door).Shared(access).Map(...).Add().
      Resolve(door, func(dst gsm.State, src map[string]gsm.State) gsm.State {
          if src["hr"].GetBool(employed) && src["security"].GetBool(cleared) {
              return dst.Set(access, "granted")
          }
          return dst.Set(access, "denied")
      })
  ```
- **Monotone mesh (cycles)** — peers that constrain **each other** (mutual, cyclic). Enable
  with `Federation.AllowMonotoneCycles()`; gsm requires the repair to be monotone and
  converges by fixed-point iteration. This is the only regime that expresses mutual
  constraints — see `examples/mesh`.
- **Compositional `Embed`** — verify a subsystem once, reuse it as a unit inside a larger
  federation. See `examples/compose`.

`Build()` rejects anything it can't prove convergent: cycles without monotonicity,
multi-source without a resolver, morphisms that don't preserve validity.

## Synthesis: generate the compensation, or prove it's impossible

You don't have to *design* the `Repair` yourself. Declare the invariants (validity) and the
events, and let gsm **generate** a convergent compensation — or tell you none exists:

```go
syn, _ := r.Synthesize()      // Repair omitted on the invariants
if !syn.Convergent {
    // these invariants + events cannot converge under ANY repair — redesign the events
}
gov := govern.New(syn.Machine(), initial) // hand the synthesized machine to a governor
```

This is a natural fit for **LLM-authored policy**: an agent turns a natural-language rule set
into invariants + events (which LLMs are good at), and synthesis mechanically produces a
convergent, crash-recoverable governor *with a proof* (which LLMs are bad at). The
impossibility verdict is a build-time coordination guardrail: it catches unconvergeable
action sets (e.g., two agents setting the same field to different constants) before deploy.

**Caveat — convergent ≠ desirable.** A synthesized repair only makes orderings *agree*; it
may not be the repair you'd *want*. Inspect `syn.Repairs()`; if the only convergent repair is
unacceptable, the fix is to redesign the *events*, not the compensation.

## Runnable demos

```
go run ./examples/mesh       # cyclic mutual-constraint safety mesh (conflict -> converge, crash recovery)
go run ./examples/compose    # a verified subsystem Embed-ed and reused across two systems
go run ./examples/authority  # authority-as-governed-state: per-principal limits, one proof, identity in the leaf
go run ./examples/compliance # KYC pipeline: parallel checks -> governed decision -> offline proofs
```

## Limits

- **Finite state spaces.** The semantic state must be finite (bounded enums/ints). Unbounded
  numeric state is a theory extension, not shipped.
- **Convergent ≠ correct.** Convergence is order-independence, not business correctness.
- **Synthesis scale.** Brute-force over repair assignments, internally bounded; large
  registries need the (not-yet-built) SAT/SMT encoding. Synthesis is single-registry today.
- **`Embed` has no namespacing.** You can reuse a subsystem across *separate* systems, but not
  instantiate it twice within one federation (registry-name collision).

## Under the hood

`govern` never leaks into the durable core (the architecture guard enforces it). All of the
above is `gsm`, the convergence engine and the founder's published research
([Normalization Confluence](https://doi.org/10.5281/zenodo.18677400)). The federation ladder,
monotone cycles, compositionality, and synthesis are all theorems in that work; gsm verifies
their preconditions exhaustively at build time.

The convergence guarantee is backed by a **machine-checked, axiom-free Coq/Rocq proof**,
CI-verified on Coq 8.18 and 8.20 (`Print Assumptions` reports "Closed under the global context"
for every key theorem; no axioms, no admits; the badge is green and anyone can reproduce it with
one command). Mechanized: Newman's Lemma, the single-registry Convergence Theorem (confluence +
unique normal forms), the soundness of gsm's WFC/CC certification (footprint-disjointness =>
commutation, potential-decrease => termination), and the federated monotone-cycles result: both
the least fixed point (Kleene) and asynchronous (chaotic) order-independent convergence to it.
Proof directory:
[normalization-confluence/coq](https://github.com/blackwell-systems/normalization-confluence/tree/main/coq).
The engine's own per-machine verification is also differentially checked against the proof by
**two independent oracles extracted from the Coq development**: a *table oracle* that re-certifies
gsm's emitted step tables converge, and a *rules oracle* that recomputes convergence straight from
the combinator declarations (trusting neither gsm's enumeration nor its normalization). Either one
runs independently of the Go, so a bug in gsm's own verifier cannot let a non-convergent machine
pass. The rules are built from a fixed combinator vocabulary rather than arbitrary Go closures,
which is what makes them inspectable and serializable to those checkers in the first place; and
for machines whose global state space is too large to enumerate, gsm verifies **footprint-local**
(`BuildCompositional`), certifying each independent component over its own small subspace. For
"when does my multi-agent / governed network converge," see the
[REGIMES field guide](https://github.com/blackwell-systems/normalization-confluence/blob/main/REGIMES.md).

Because the policy is inspectable, serializable data, it also becomes an audit artifact:
`govern.AttestedEventTool` records which policy admitted each governed action, and the policy is
anchored as a log leaf an auditor cross-links to the action and re-checks with the external oracle.
See [AUDIT.md](AUDIT.md) for the attestation and `goagents-audit verify-governed-action`.
