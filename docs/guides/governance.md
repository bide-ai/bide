# Convergent governance (Tier-2)

How to let **multiple concurrent agents mutate shared state and still agree**, without a
lock, a leader, or a consensus round. This is the `govern` package. It's built on
[gsm](https://github.com/blackwell-systems/gsm) (Governed State Machines), which checks *at
build time* that every interleaving of agent actions (every order of the event pairs declared with
`Independent`, when pairs are declared) converges to the same valid state, against the conditions
of a machine-checked convergence theorem. `Build` returns a machine only after the table oracle, Go
generated from gsm's machine-checked Rocq proof, re-checks it in-process, and for combinator rules
inside its fragment and within a cost cap, the rules oracle re-checks it from the rules as well. A
federation's own conditions, including the event-order checks C1 and C2 (see
[Federation](#federation-constraints-across-agents)), are checked by gsm's Go code. In CI, bide's required gsm machine gate
runs the proof's checkers on every machine the governance examples build. The exact scope,
including which event pairs CC covers (every event pair, or only those declared with
`Independent`), is in [known limitations](../KNOWN-LIMITATIONS.md#governed-state-gsm); see also
[Limits](#limits).

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

- **Variables**: the finite shared state (enums, bools, bounded ints).
- **Invariants**: what "valid" means (`Holds`), and how to **repair** a violation (`Repair`).
- **Events**: the actions agents can take (`Writes` / `Guard` / `Apply`).

`Build()` enumerates the state space and **checks** two properties: compensation always
terminates (WFC) and event order doesn't matter after repair (CC). The convergence theorem says a
machine with both properties converges under every order. If `Build` finds a violation, it
**refuses to build** and hands you a counterexample. A built machine is an immutable set of
O(1) lookup tables; no compensation logic runs at runtime.

<!-- docsnip: setup import "github.com/blackwell-systems/gsm" -->
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
m, report, err := r.Build() // certifies WFC + CC, or returns a counterexample
```

This order machine is also the standard case of the `Build` gap gsm v0.12.0 fixed: if `ship` is
guarded on `paid`, which `pay` writes, `ship` then `pay` and `pay` then `ship` end in different
states. gsm v0.11.0's `Build` could certify it; v0.12.0's rejects it with a counterexample. See
[Limits](#limits).

## What an invariant guarantees: prevent, repair, halt

An invariant is inviolate on the **committed state**: every state a governor ever returns or
commits satisfies every invariant, checked exhaustively at build against the conditions of the
machine-checked convergence theorem (see [Limits](#limits)). What it does not promise is that no *transient* state is ever invalid, because
the model is precisely that an event may violate an invariant and compensation then restores
validity. So pick the posture per rule:

- **Prevent (guard).** A `Guard` / `OnlyIf` makes the event a no-op unless its precondition holds,
  so the invariant is never breached even transiently on that path. Use this when the action must
  never fire into an invalid state at all.
- **Repair (compensation).** The event may fire into an invalid state, and the `Repair` restores a
  valid normal form. The invariant holds after repair, not during. This is the default and the
  source of order-independent convergence.
- **Halt (human decision).** A tool built with `agent.WithApproval(agent.SingleApproval())` (or one that calls
  `Interrupt`) pauses the run for a durable human decision instead of auto-repairing, for cases
  where silent compensation is not acceptable.

Two boundaries follow from this. A machine only builds if every invariant is restorable (WFC + CC),
so any shipped invariant is provably always-restored; `Synthesize` reports a witness when none is.
And a governor makes the governed *record* inviolate, not the outside world: compensation can reset
a state variable but cannot undo a side effect that already left the process, so an invariant that
must hold in reality (not just in the record) uses prevent or halt **before** the side effect
routes, the pre-trade-check posture rather than post-trade remediation.

## Governors: the durable runtime

A **governor** applies agents' events to the shared state. Two flavors:

<!-- docsnip: setup ctx context.Context; import "github.com/blackwell-systems/gsm"; m *gsm.Machine; log govern.EventLog -->
```go
gov := govern.New(m, m.NewState())                        // in-memory, thread-safe
pg, _ := govern.NewPersistent(ctx, m, log, "order-42", m.NewState()) // event-sourced
```

`PersistentGovernor` appends every event to an `EventLog`, and its state **is the log replayed**
through the machine: crash-recoverable, and shared. Any number of processes can run a governor over
the same log. Before answering, `Apply` folds in every event the log holds up to and including its
own, in log order, including other processes' events, so the state it returns (and the
`state_digest` an attested `EventTool` records) is exactly what an auditor gets by replaying the log
through that event's position, reported as `Applied.Position`. Events other processes append later
are folded in by the next `Apply`, or on demand with `Sync(ctx)`; `State()` is the view as of the last
of those. `FederatedGovernor` works the same way for a federation.

An event name the machine does not declare is rejected before anything is written (an
`agent.ErrConfig` error), so a typo in one process never reaches the shared log. If a log already
holds such an entry (from an older policy or another writer), every fold (`NewPersistent`, `Sync`,
`Apply`) stops there with an `agent.ErrProtocol` error rather than crashing.

`ApplyOnce(ctx, id, event)` applies an event at most once per id, across every process sharing the
log: a repeat returns the first call's position and the state replayed through it. `EventTool`
uses it with an id made of the tool call's id and the apply's number within the call
(`agent.NextOnceKey`), so a governed tool call that runs again (its process died after the event
was appended but before the call's result was recorded) records its event once, and a composite
tool that applies several events in one call records each of them. The numbering is stable when
the call applies its events in the same order each time it runs; applies made concurrently must
each run in their own step (`Journal.Step`, or the tasks of `Journal.Parallel`), which numbers its
applies under the step's name.

`EventLog` is a port with a precise contract: `Append(ctx, entity, id, event)` returns the event's
position, positions are dense and never change, and `Events(ctx, entity, from)` reads from any
position, so a reader that has seen positions `[0, n)` always sees them first again. Appends are
idempotent by id: an append whose id the entity's log already holds records nothing and returns the
recorded position, so a transport retry (a Redis client resending a command whose reply was lost)
cannot record an event twice. `govern/eventlogtest.Run` checks that contract, including concurrent
appends and concurrent repeated appends from independent handles; every adapter below runs it, and so
should your own. Adapters:

- `govern.NewMemEventLog()`: in-memory (tests / local).
- `govern/sqlitelog`: on-disk SQLite.
- `govern/postgreslog`: Postgres, for several processes on a shared database. An append is one
  statement that Postgres commits before it replies, so a process stalled mid-append holds no lock
  another process's append waits on, and it behaves the same whatever the database's default
  isolation (a serialization failure changed nothing and is run again, after a backoff, until the
  context ends; give `Append` a deadline if it must be bounded).
- `govern/redislog`: Redis Streams (networked, "no SQL DB required"). An entity's stream and its
  append-id hash share the Redis Cluster hash tag `{<len>:<entity>}`, so the append script, which
  touches both, runs on Redis Cluster; a key prefix must not contain `{` or `}`.

The in-memory `Governor` holds its state in one process. Use a `PersistentGovernor` whenever more than
one process acts on the same state, or when recorded state digests must be checked later against a
durable log.

## The agent boundary

`FederatedEventTool` (and `EventTool` for a single governor) turns an agent's **tool call**
into a governed event. This is how an LLM agent participates:

<!-- docsnip: setup gov *govern.Governor -->
```go
tool := govern.EventTool(gov, govern.EventToolConfig{Name: "pay", Description: "mark the order paid", Event: "pay"})
// give `tool` to the agent; when the LLM calls it, "pay" is applied to shared state,
// convergently, and durably. EventToolConfig also takes Safety, Options (agent.ToolOptions
// such as agent.WithApproval or agent.WithTimeout), and PolicyDigest (the attested form;
// Attested without a PolicyDigest is refused with ErrConfig).
```

Multiple agents sharing one governor converge no matter how their calls interleave.

## Identity and delegated authority

A governed action can commit to **who acted**, not just what happened. The deployment binds an
`agent.Identity{Actor, OnBehalfOf, AuthorityRef}` to the run (from its own auth layer, never from
the model), and an attested `EventTool` (one whose `EventToolConfig.PolicyDigest` is set) stamps it into the same leaf as the policy and state digests:

<!-- docsnip: setup ctx context.Context; a *agent.Agent; runID string; input string; returns error -->
```go
id := agent.Identity{Actor: "exec-agent@1.4.2", OnBehalfOf: "desk-EQ-US", AuthorityRef: "grant#a1b2"}
desk, err := a.With(agent.WithIdentity(id)) // a copy of a acting for this desk
if err != nil {
	return err
}
desk.Run(ctx, runID, agent.UserText(input)) // the identity propagates to governed tools and sub-agents
```

An identity given to one run (`agent.WithIdentity` passed to `Run` as a run option) takes
precedence over the agent's. The same rule decides a sub-agent's identity: a
sub-run's context carries the identity of the run that started it, so a sub-agent (a `SubAgent`
tool, or a programmatic sub-run) runs as its parent's identity whenever the parent has one, and
its own `WithIdentity` applies only under a parent that has none. To act under narrower
authority in a sub-run, attenuate the delegation (`audit.AttenuatingSubAgent`), which binds the
child grant's identity to the sub-run, rather than give the sub-agent an identity of its own.

An inclusion proof then commits to who acted, on whose behalf, and under what authority. The SDK
proves the identity CLAIM; authenticating the principal is the operator's IdP/PKI, and the
attribution is only as strong as the key custody behind the run's signatures (see [Audit](audit.md)).

**Authority as governed state.** The cleaner move is to make the delegated authority part of the
state the invariants read, so the rule is scoped per principal. Model the limit as a variable and
require `exposure <= limit`:

<!-- docsnip: setup import "github.com/blackwell-systems/gsm"; r *gsm.Registry; m *gsm.Machine; granted int -->
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
each run is governed to the limit its grant seeded. See `examples/govern/authority`.

The same pattern (an external fact seeded into governed state, gated by an invariant) is how a
**k-of-n model quorum** would be built: fan out a decision to N models, tally the votes, seed the
count into state, and gate the commit on `votes_for >= k`, so a high-stakes action requires
agreement or escalates. It is a composition of existing seams, not a new agent type, and it is
implemented: `govern.Quorum` (k-of-n model agreement over `Journal.Parallel`) plus the
`bide-audit verify-quorum` verb and `examples/govern/quorum`. See [Quorum](quorum.md).

## Federation: constraints across agents

When shared state spans **multiple registries** with cross-registry rules (one agent's state
constrains another's), connect them with **morphisms** into a `Federation`, and drive it with
a `FederatedGovernor`. The capability ladder:

- **Tree**: each target has one source; the source is *authoritative* over the target's
  shared component (a manufacturer's status fixes a supplier's listing). A conflicting write to
  that component is repaired from the source: the source wins, deterministically.
- **Multi-source (DAG)**: a target with several sources declares a **`Resolver`** that
  merges them (priority / AND-OR / most-restrictive):
  <!-- docsnip: skip the Map arguments are elided; the example shows the Resolve call -->
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
- **Monotone mesh (cycles)**: peers that constrain **each other** (mutual, cyclic). Enable
  with `Federation.AllowMonotoneCycles()`; gsm requires the repair to be monotone and
  converges by fixed-point iteration. This is the only regime that expresses mutual
  constraints (see `examples/govern/mesh`).
- **Compositional `Embed`**: verify a subsystem once, reuse it as a unit inside a larger
  federation. See `examples/govern/compose`.

`Build()` rejects the structures it cannot certify convergent: cycles without monotonicity,
multi-source without a resolver, morphisms that don't preserve validity, and (since gsm v0.13.0)
event orders that can diverge across registries.

**Event order across registries.** The authority argument makes the federated repair terminate in
a unique valid normal form, but it does not make the order of events across registries irrelevant
on its own: a target event whose guard or effect reads a shared component can end differently
before and after a change its source drives, and two target events can commute on their own
registry but not with the repair between them. The claim that cross-registry conflicts resolve
without coordination by the authority argument alone is false (normalization-confluence
`FederationGRS.v`, `fed_thm_fed_convergence_refuted`). Event order across registries is safe
because `Federation.Build` checks both cases, cross-registry CC (C1) and repaired CC (C2), and
rejects a federation that fails either with a `*gsm.CrossOrderError` or `*gsm.SameTargetOrderError`
naming the events, the target and a witness state where the two orders give different results
(`fed_thm_fed_convergence_guarded`). C2 checks the target event pairs the target's own CC covers
(every pair, or only those declared with `Independent`). The same checks certify event order on
monotone cycles (`cyc_check_gc_lfp`) and on `BuildCoordinated`'s residual network. Both are static,
over every valid source state, so the witness may be a state a given run never reaches; to fix a
rejection, record the fact locally and let an invariant derive the outcome, or move the dependency
into the morphism.

## Synthesis: generate the compensation, or prove it's impossible

You don't have to *design* the `Repair` yourself. Declare the invariants (validity) and the
events, and let gsm **generate** a convergent compensation, or tell you none exists:

<!-- docsnip: setup import "github.com/blackwell-systems/gsm"; r *gsm.Registry; initial gsm.State -->
```go
syn, _ := r.Synthesize()      // Repair omitted on the invariants
if !syn.Convergent {
    // these invariants + events cannot converge under ANY repair; redesign the events
}
gov := govern.New(syn.Machine(), initial) // hand the synthesized machine to a governor
```

This is a natural fit for **LLM-authored policy**: an agent turns a natural-language rule set
into invariants + events (which LLMs are good at), and synthesis mechanically produces a
convergent, crash-recoverable governor *with a proof* (which LLMs are bad at). The
impossibility verdict is a build-time coordination guardrail: it catches unconvergeable
action sets (e.g., two agents setting the same field to different constants) before deploy.

**Caveat: convergent ≠ desirable.** A synthesized repair only makes orderings *agree*; it
may not be the repair you'd *want*. Inspect `syn.Repairs()`; if the only convergent repair is
unacceptable, the fix is to redesign the *events*, not the compensation.

### Verify-or-repair and federation coordination

`govern.New` / `govern.NewFederated` wrap a machine that is **already built and certified** by gsm.
So gsm's build APIs are run by the caller on the registry or federation, and the
resulting machine is then handed to `govern`. Two capabilities are worth reaching for:

- **Verify-or-repair in one call**: `Registry.BuildOrSynthesize(opts ...gsm.SynthOption)` builds
  the machine as you wrote it and, only when your rules do not converge, falls back to synthesizing
  a convergent compensation and building that instead. It returns a ready-to-use `*gsm.Machine`;
  the second return is a `*gsm.Synthesis` that is nil when your repair built as written and non-nil
  (with `Repairs()` / `String()`) when a synthesized compensation was substituted. It errors only
  when neither path works: build failed and no convergent compensation exists (the error carries
  the impossibility witness). It ties verification and repair generation together, so a caller can
  say "build this, and if my repair does not converge, give me one that does."
  <!-- docsnip: setup import "github.com/blackwell-systems/gsm"; r *gsm.Registry; initial gsm.State -->
  ```go
  m, syn, err := r.BuildOrSynthesize()
  if err != nil { /* your rules cannot converge, even with a synthesized repair */ }
  if syn != nil { /* a compensation was synthesized; vet syn.Repairs() */ }
  gov := govern.New(m, initial)
  ```
- **Coordinate a cyclic, non-monotone federation**: when a federation has morphism cycles that are
  not monotone, `Build` rejects it. `Federation.CoordinationPlan()` returns a set of morphism edges
  (a feedback edge set) to coordinate so the residual network is acyclic and therefore converges;
  `Federation.BuildCoordinated(plan)` then builds the federation given that those edges are
  externally coordinated (their shared variables become external inputs that the coordination
  mechanism, a single writer, a lock or a consensus round, must serialize; gsm does not check
  that coordination). The plan is a correct coordination of size at most the number of independent
  cycles, not necessarily the minimum (the exact minimum is NP-hard); an empty plan is exactly
  `Build`. Each `CoordinationPoint` names its `Authority`, the registry the cycles it breaks are
  driven from: the normal form is unique given the plan, but cutting a different edge of a cycle
  picks a different root and a different normal form.
  <!-- docsnip: setup import "github.com/blackwell-systems/gsm"; fed *gsm.Federation -->
  ```go
  plan := fed.CoordinationPlan()           // where to coordinate; nil if already acyclic
  fm, _, err := fed.BuildCoordinated(plan) // build given that coordination
  // hand the *gsm.FedMachine to govern.NewFederated(ctx, fm, log, entity, initial)
  ```
  This coordinates only the edges the plan names; the rest of the network runs without
  coordination, under the same `Federation.Build` checks (C1 and C2 included) on the residual
  network. See `examples/govern/coordination`.

## Runnable demos

The demos are one module, `examples/govern`, so they build without adding gsm to the core:

```
cd examples/govern
go run ./mesh       # cyclic mutual-constraint safety mesh (conflict -> converge, crash recovery)
go run ./compose    # a verified subsystem Embed-ed and reused across two systems
go run ./authority  # authority-as-governed-state: per-principal limits, one proof, identity in the leaf
go run ./compliance # KYC pipeline: parallel checks -> governed decision -> offline proofs
go run ./quorum     # governed model quorum: k-of-n agreement gates the commit, else escalate
```

## Limits

- **Verdicts recorded under gsm v0.11.0.** bide required gsm v0.11.0 before; its `Build` skipped
  the CC check for event pairs it judged independent from what they write, without checking what
  their guards and effects read, so it could certify a machine that does not converge. gsm v0.12.0
  checks every pair it checks for CC (every event pair, or only the pairs
  declared with `Independent`) exactly, with no footprint shortcut; a verdict or certificate
  recorded under v0.11.0 is not covered by that fix. See
  [known limitations](../KNOWN-LIMITATIONS.md#governed-state-gsm), which also gives what the
  proof-derived checks cover.
- **Federations built under gsm v0.12.0.** gsm v0.12.0 did not check event order across
  registries (C1, C2), so a federation it built could reach a state that depends on which event
  was appended first; gsm v0.13.0, which bide requires now, checks both. A claim about a federation
  built under v0.12.0 holds only once it builds under v0.13.0.
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
monotone cycles, compositionality, and synthesis are all theorems in that work; gsm checks
their preconditions at build time by enumeration. The federated convergence theorem holds in its
corrected form, with C1 and C2 as conditions; as first published, without them, it is false
(both are mechanized in `FederationGRS.v`).

The convergence theorem is a **machine-checked, axiom-free Coq/Rocq proof**,
CI-verified on Coq 8.18, 8.20, and Rocq 9.3 (`Print Assumptions` reports "Closed under the global context"
for every key theorem; no axioms, no admits; the badge is green and anyone can reproduce it with
one command). Mechanized: Newman's Lemma, the single-registry Convergence Theorem (confluence +
unique normal forms), the soundness of gsm's WFC/CC certification (footprint-disjointness =>
commutation, for events that read only their own footprint; potential-decrease => termination), and the federated monotone-cycles result: both
the least fixed point (Kleene) and asynchronous (chaotic) order-independent convergence to it.
Also mechanized: federated convergence under C1 and C2 (`fed_thm_fed_convergence_guarded`; that
gsm's static checks suffice, `static_c1_c2_gc`; on monotone cycles, `cyc_check_gc_lfp`), and the
counterexample to the version without them (`fed_thm_fed_convergence_refuted`). Also mechanized: convergence under causal delivery, where only *concurrent* events need to commute
after compensation and causally ordered ones never do; standard op-based CRDTs converge as an
instance, and the compensation-free fragment is exactly the op-based CRDTs
([SUBSUMPTION.md](https://github.com/blackwell-systems/normalization-confluence/blob/main/SUBSUMPTION.md)).
In bide a governor replays one durable log in one total order that respects every causal
dependency running through the log (a writer only observes events already in the log), so this covers concurrent writers whose appends to one
registry's log race, provided every pair that can be concurrent is checked (all pairs, or each such
pair declared with `Independent`); bide does not track causality, so declaring those pairs is the
caller's job. With pairs declared, gsm v0.13.0 also checks the undeclared pairs and lists each one
that does not commute; `govern.CertifyConvergence` records them (`CausalOrderRequired`), and those
events must never be concurrent. It records the events a duplicate delivery would change
(`NotIdempotent`) as well: a governed tool applies its event at most once per tool call
(`ApplyOnce`).
Proof directory:
[normalization-confluence/coq](https://github.com/blackwell-systems/normalization-confluence/tree/main/coq).
The theorem is correct; gsm v0.11.0's `Build` used the commutation lemma without checking its
read-footprint precondition, which gsm v0.12.0 fixed (see [Limits](#limits)).
**Two independent oracles are extracted from the Coq development**: a *table oracle* that
re-certifies that gsm's emitted step tables converge, and a *rules oracle* that recomputes
convergence straight from the combinator declarations (trusting neither gsm's enumeration nor its
normalization). Since gsm v0.12.0, `Build` runs the table oracle (and, within its fragment and
cap, the rules oracle), generated from the proof, in-process before it returns a machine, and fails
closed with no machine if an oracle does not certify it. The table oracle gates `SynthesizeWith`,
`BuildOrSynthesize` and each `BuildCompositional` component the same way. For a federation
(`govern.NewFederated`), each component is rebuilt with `Build` and so is oracle-gated, but the
federation-level checks (the morphism and resolver conditions, the event-order checks C1 and C2,
acyclicity, and the fixed-point iteration of monotone cycles) are gsm's Go code and are not
oracle-certified. The
oracles' scope is in [known limitations](../KNOWN-LIMITATIONS.md#governed-state-gsm). bide's required `gsm machine gate`
CI check also runs the two extracted checkers on every machine the governance examples build. An auditor can run the rules oracle on a disclosed
policy with `bide-audit`'s `-checker` flag (see [Audit](audit.md)). The rules are built from a fixed combinator vocabulary rather than arbitrary Go closures,
which is what makes them inspectable and serializable to those checkers in the first place; and
for machines whose global state space is too large to enumerate, gsm verifies **footprint-local**
(`BuildCompositional`), certifying each independent component over its own small subspace. For
"when does my multi-agent / governed network converge," see the
[REGIMES field guide](https://github.com/blackwell-systems/normalization-confluence/blob/main/REGIMES.md).

Because the policy is inspectable, serializable data, it also becomes an audit artifact:
An attested `govern.EventTool` (`EventToolConfig.PolicyDigest` set) records which policy admitted each governed action, and the policy is
anchored as a log leaf an auditor cross-links to the action and re-checks with the external oracle.
See [Audit](audit.md) for the attestation and `bide-audit verify-governed-action`.
