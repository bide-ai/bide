# Governed convergent state in the rung-1 `plan` flow builder

Status: SCOPING DESIGN NOTE. Not built. This is research plus a proposed shape,
subject to the three-question gate in
[expression-surfaces.md](expression-surfaces.md). Nothing here has been
implemented, and this note deliberately edits no core or `plan` code.

## The question

Can the rung-1 `plan` builder (a Go-embedded flow DSL, see
[expression-surfaces.md](expression-surfaces.md)) express a first-class GOVERNED
effect: a node that applies a gsm-verified event to shared, convergent state? And
if so, how, given that `plan` is architecturally forbidden from importing the
`govern` package or gsm at all?

## 1. Current state: how governed state is applied today

Governed state lives entirely in the `govern` package (a Tier-2 edge integration),
NOT in the core module. The relevant symbols:

- `govern.Applier` (an interface) at
  [govern/govern.go:27](../../govern/govern.go) is the port a caller applies a
  governed event through:

  ```go
  type Applier interface {
      Apply(ctx context.Context, event string) (gsm.State, error)
      State() gsm.State
  }
  ```

  Its signature returns `gsm.State`, so the port itself is expressed in gsm
  types. Two adapters satisfy it: `govern.Governor` (in-memory,
  [govern/govern.go:42](../../govern/govern.go)) and
  `govern.PersistentGovernor` (crash-recoverable via an `EventLog`,
  [govern/govern.go:110](../../govern/govern.go)).

- The bridge from an agent loop to governed state is
  `govern.EventTool(gov Applier, name, description, event string, safety agent.Safety) agent.Tool`
  at [govern/govern.go:161](../../govern/govern.go). When the agent's model calls
  the tool, the tool body calls `gov.Apply(ctx, event)`. There is also
  `govern.AttestedEventTool` ([govern/govern.go:183](../../govern/govern.go)),
  which additionally records `policy_digest` and `state_digest` into the tool
  result so an auditor can bind the action to the convergent policy that admitted
  it.

- The convergence proof itself is carried by
  `govern.ConfluenceCertificate` / `govern.CertifyConvergence`
  ([govern/confluence.go:23](../../govern/confluence.go),
  [govern/confluence.go:62](../../govern/confluence.go)), translated from
  `gsm.Registry.Build`'s report (WFC and CC, checked exhaustively over the
  enumerated state space).

The concrete authoring path today (see
[examples/coordination/main.go:82](../../examples/coordination/main.go)):

```go
gov  := govern.New(m, m.NewState())                    // wrap a built gsm.Machine
incA := govern.EventTool(gov, "increment_a", "...", "inc_a", agent.Safety{Idempotent: true})
agent.New(&model, agent.NewMemStore(), incA).Run(ctx, "gov-1", "...")
```

The governed effect reaches the runtime AS A TOOL. `EventTool` produces an
`agent.Tool` via `agent.Func` ([tool.go:63](../../tool.go)); the tool call is
journaled by the core loop like any other tool call.

**Is there a core-level governance port? No.** The core module
(`github.com/dayna/go-agents` root package) has no governance interface. Grepping
the root `*.go` files, the only `interface` types are `agent.Event`
([model.go:78](../../model.go), a provider-stream event) and `agent.AgentEvent`
([stream_agent.go:20](../../stream_agent.go)) - neither is about governed state.
`Applier` is defined in `govern`, imports gsm, and returns `gsm.State`. The core
imports no gsm (locked by
[architecture_test.go:37](../../architecture_test.go)).

So governed state today is applied by importing `govern` (and transitively gsm),
constructing a `Governor`/`PersistentGovernor`, and either calling `Apply`
directly or handing an `EventTool`/`AttestedEventTool` to `agent.New`. There is
no core abstraction that lets a caller apply a governed event without pulling the
`govern` subpackage.

## 2. The constraint: `plan` cannot import `govern` or gsm

[plan/architecture_test.go](../../plan/architecture_test.go),
`TestPlanNoAdapterImports`, lists `dayna/go-agents/govern` in the `forbidden`
adapter set and `blackwell-systems/gsm` in the forbidden-infrastructure set. It
runs `go list -deps github.com/dayna/go-agents/plan` and fails if either string
appears in `plan`'s non-test runtime dependency graph. The comment is explicit:
`plan` may import the core root (that is intentionally NOT forbidden) but "must
never drag a concrete adapter into its runtime import graph. Dependencies point
INWARD." `govern` is called out as the "gsm-backed governor (Tier-2 edge)."

This is a hard boundary, confirmed by reading the test. A first-class governed
node in `plan` therefore cannot reference `govern.Applier`, `govern.EventTool`,
`gsm.State`, or any gsm type, because that would put `govern`/gsm into `plan`'s
runtime import graph and the test would fail.

Two options follow:

**(a) Expose governance as a CORE port that `plan` depends on.** `plan` already
imports the core root freely (that is how it reaches `agent.Durable`,
`agent.Tool`, `agent.Func`, `agent.Safety`). If the core grew a governance
interface expressed in NEUTRAL types (no gsm), `plan` could depend on that
interface and stay clean, while `govern` supplies the gsm-backed adapter. This is
the same ports-and-adapters move the whole hexagon rests on.

**(b) Governed effects stay plain `Step`/`Tool` bodies calling a core API.**
Today `EventTool` already produces an `agent.Tool`. A caller could register that
tool in a flow via `Builder.Tool` ([builder.go:97](../../builder.go)) with no new
core port at all: the `agent.Tool` is a neutral type `plan` already accepts, and
the closure inside it closes over a `govern.Applier` constructed in the CALLER's
package (which may import `govern`), not in `plan`. `plan` never names a governed
type; it just runs a tool.

**Assessment.** Option (b) is feasible TODAY with zero core or `plan` changes: a
caller builds an `EventTool` in their own package and passes it to
`Builder.Tool`. The cost is that the governed effect is opaque to `plan`: it is an
ordinary tool node, so `plan` cannot surface the footprint, cannot render the node
as "governed," and cannot fold gsm's convergence facts into `Build`-time
validation. That is exactly the reification `plan` exists to provide (see
[expression-surfaces.md](expression-surfaces.md), "What reifying the flow buys").

Option (a) is what makes a governed effect FIRST-CLASS. It requires a new core
port, because there is no governance abstraction in the core today and `plan`
cannot reach the one in `govern`. The port must be expressed without gsm types (a
neutral `event string`, a neutral footprint descriptor), or the core would
transitively depend on gsm and break
[architecture_test.go:37](../../architecture_test.go). The gsm-typed `Applier`
in `govern` would then be adapted to that neutral core port.

## 3. Proposed first-class governed node for `plan`

A `Builder.Govern` constructor that registers a governed effect as a durable node,
mirroring `Builder.Tool` in structure and lowering.

### Shape

```go
// PROPOSED, not built. Neutral types only; no gsm, no govern.
func (b *Builder[In, Out]) Govern[I, O any](
    name string,
    gov  agent.Governor,   // NEW core port (see below); adapter lives in govern
    event string,
    fp   agent.Footprint,  // NEW neutral footprint descriptor
) Handle[I, O]
```

`Govern` records a `node` (see [plan/spec.go:21](../../plan/spec.go)) of a new
`kindGovern` whose `run` closure calls `gov.Apply(ctx, event)` and returns the
resulting state (or a caller-projected `O`) . It returns a `Handle[I, O]` exactly
like `Step`/`Tool`/`Model`, so it wires with `Edge`/`Switch` unchanged.

### The new core port

The core would grow a governance PORT expressed in neutral types:

```go
// PROPOSED core addition. No gsm import; stays inside the dependency-light core.
type Governor interface {
    Apply(ctx context.Context, event string) (json.RawMessage, error) // neutral state digest/snapshot
}

type Footprint struct {
    Writes []string // variable names this event writes
    Reads  []string // variable names this event reads
}
```

`govern.Governor` / `govern.PersistentGovernor` already satisfy the shape (their
`Apply` returns `gsm.State`); an adapter in `govern` would wrap them to return a
neutral `json.RawMessage` (for example `state.Digest()`, already used by
`AttestedEventTool` at [govern/govern.go:198](../../govern/govern.go)). The
gsm-typed `govern.Applier` stays where it is; the neutral `agent.Governor` is the
port `plan` depends on. This is the (a) path from section 2.

### Surfacing the footprint

The convergence guarantee is a FOOTPRINT property. In gsm, each event declares the
variables it writes via `Registry.Event(name).Writes(vars...)`
([registry.go:217](../../../../go/pkg/mod/github.com/blackwell-systems/gsm@v0.11.0/registry.go)),
and gsm's build-time check proves confluence by discharging independent event
pairs either by footprint-disjointness or by brute force
([footprint.go](../../../../go/pkg/mod/github.com/blackwell-systems/gsm@v0.11.0/footprint.go);
`ConfluenceCertificate.PairsDisjoint` / `PairsBrute` record the split, see
[govern/confluence.go:39](../../govern/confluence.go)). Two events with disjoint
footprints commute, so their order does not matter.

`plan` cannot see gsm footprints directly, but if the caller passes a neutral
`agent.Footprint` to `Govern`, `plan` can do a `Build`-time STATIC check that
composes with gsm's convergence rather than duplicating it:

- Record each governed node's declared `Writes`/`Reads`.
- At `Build`, for any two governed nodes on CONCURRENT-CAPABLE paths (in rung-1,
  the two arms of a `Switch`, or, once fan-out exists, parallel branches), report
  whether their footprints are DISJOINT. Disjoint pairs are order-independent by
  gsm's own CC lemma; overlapping pairs are the ones whose convergence rests on
  compensation (a positive `MaxRepairLen`).
- This is a diagnostic, not a re-proof: gsm still owns the proof. `plan` surfaces
  the footprint so the AUTHORED topology shows which governed effects touch shared
  variables, the same way `Build` today surfaces type mismatches and reachability.

The footprint the caller declares to `plan` MUST match the gsm event's `Writes`.
That correspondence is the caller's to keep (gsm's own `verifyFootprints` enforces
it inside the machine); `plan` treats the declared footprint as authoritative for
its static view. A mismatch is a caller bug, the same class as a wrong
`IdempotencyKey`.

### How it lowers (no new executor)

`Govern` lowers exactly like `Tool` does today. `runNode`
([plan/flow.go:177](../../plan/flow.go)) already drives every node under the
two-phase attempt/result guard: it writes an `attempt:` marker
([flow.go:161](../../plan/flow.go)), runs the body, then journals the result via
`store.Do`. A governed node's body calls `gov.Apply`; the applied event's result
(the neutral state snapshot) is JSON-encoded and journaled as the node's `Record`
([flow.go:215](../../plan/flow.go)). No new `Durable`, no goroutine, no channel:
`TestNoNewExecutor` ([plan/architecture_test.go](../../plan/architecture_test.go))
stays green.

One subtlety worth stating: gsm `Apply` is convergent and order-independent for
the events gsm proved independent, so re-driving it is safe. But `plan`'s
at-most-once guard already treats a governed node like any effectful step: an
attempt marker with no result HALTS (`*HaltAmbiguous`,
[flow.go:149](../../plan/flow.go)) rather than re-applying. That is conservative
and correct: it inherits the substrate guarantee unchanged. A later refinement
could let a governed node whose footprint gsm proved compensation-free
(`ConfluenceCertificate.CompensationFree`, [confluence.go:55](../../govern/confluence.go))
auto-retry instead of halt, the governed analogue of `Safety.Idempotent`
([tool.go:55](../../tool.go)). That is a non-goal for a first cut.

## 4. The multi-agent framing: where convergence is actually proven

This is the load-bearing distinction, and it is easy to overclaim.

- gsm's guarantee is about CONCURRENT agents mutating SHARED state: any
  interleaving of the same event set reaches the same valid normal form
  ([govern/govern.go:1-14](../../govern/govern.go),
  [govern/confluence.go:14-22](../../govern/confluence.go)). The proof is over the
  event set and the state space, not over any one flow's control flow.

- A SINGLE `plan` flow is strictly sequential (`Flow.Run` walks one node at a
  time, [flow.go:57](../../plan/flow.go)). Within one flow, governed nodes apply in
  a fixed journaled order. Convergence buys that single flow almost nothing that
  ordinary journaling does not already give it: the order is deterministic, so
  there is no interleaving to reconcile.

- Convergence EARNS ITS PLACE across MANY concurrent flows/agents sharing ONE
  governor. If flow A and flow B (two runs, possibly two processes, backed by a
  shared `PersistentGovernor` over a durable `EventLog`,
  [govern/govern.go:110](../../govern/govern.go)) each apply governed events, gsm
  guarantees the shared state converges regardless of how A's and B's events
  interleave. The e2e proof of this lives in the core's
  [e2e_convergence_test.go](../../e2e_convergence_test.go) (concurrent agents) and
  `govern`'s tests, not in `plan`.

So the correct claim for a governed `plan` node is: a single flow declares a
governed effect and gets it journaled and rendered as a governed step; the
CONVERGENCE property is a statement about the shared governor that multiple such
flows target, and it is proven by gsm at build time, surfaced by
`ConfluenceCertificate`, and unaffected by how any one flow was authored. `plan`
does not prove convergence and must not claim to. Its contribution is reification:
making the governed effect and its footprint VISIBLE in the authored topology, and
conformable against the journal.

## 5. The three-question invariant gate

From [expression-surfaces.md](expression-surfaces.md), applied to this feature.

1. **Does it lower to the core?** Yes, cleanly. A `Govern` node is a `Tool`-shaped
   node whose body calls a neutral core `Governor.Apply`, journaled by the same
   `runNode` two-phase guard as every other node. No new executor, no new
   durability model. It inherits at-most-once, halt, resume, and the audit trail
   by construction. PASS.

2. **Does the substrate stay the headline?** Yes, and this feature reinforces it
   rather than competing with it. The headline is "provably convergent governed
   state on a verifiable journal"; a governed `plan` node makes that authorable and
   renderable without moving the proof out of gsm or the guarantees out of the
   runtime. The risk to watch: do not let "governed graph node" become a pitch of
   its own. It is a convenience over `EventTool`, which is itself a convenience over
   `Governor.Apply`. PASS, with that caveat.

3. **Is it pulled, not pushed?** UNPROVEN. There is no evidence in the repo of a
   user or design partner asking to author governed effects through `plan` rather
   than through `EventTool` directly. Option (b) (register an `EventTool` via
   `Builder.Tool`) already works with zero new code, so the first-class node is a
   REIFICATION-and-ergonomics upgrade, not a capability unlock. Per the gate, a
   speculative second surface splits focus from proving the core wedge. This gate
   is the one that should hold the work until pull is demonstrated. HOLD.

Net: the feature passes the "lowers to the core" and "substrate stays headline"
gates and is architecturally sound. It fails the "pulled, not pushed" gate today.
The recommendation follows from that.

## 6. Scope table

| Area | Change | Status |
|------|--------|--------|
| CORE | Add a neutral `Governor` port (`Apply(ctx, event) (json.RawMessage, error)`) and a neutral `Footprint` descriptor. No gsm import. | Required for first-class (option a); locked by [architecture_test.go:37](../../architecture_test.go) to stay gsm-free. |
| `govern` | Add an adapter wrapping `govern.Applier` to satisfy `agent.Governor` (return `state.Digest()` as the neutral snapshot). | Required for first-class. Keeps gsm types in `govern`. |
| PLAN | Add `Builder.Govern[I,O]` constructor + `kindGovern` node; lower via existing `runNode`; extend `Build` to surface footprint-disjointness diagnostics across concurrent-capable paths; label governed nodes in `RenderMermaid`. | Required for first-class. No executor, no gsm import. |
| PLAN | Extend the frozen `node` spec ([plan/spec.go:21](../../plan/spec.go)) with a `footprint` field. | Required. Same scaffold-extension pattern the `Tool` doc comment ([builder.go:90-96](../../builder.go)) flags for `Safety`. |
| CONFORMANCE | Teach `Conform` that a `kindGovern` node's journaled record includes the event and state snapshot, so the actual-ran governed path checks against the declared one. | Nice-to-have; follows `plan`'s existing conformance model. |
| NON-GOAL (first cut) | Auto-retry a compensation-free governed node instead of halting (governed `Safety.Idempotent`). | Explicit non-goal. Halt is the conservative correct default. |
| NON-GOAL (first cut) | `plan` re-proving or verifying convergence. gsm owns the proof; `plan` only surfaces the footprint. | Explicit non-goal. |
| NON-GOAL (first cut) | Concurrent governed flows / fan-out in `plan`. Rung-1 is strictly sequential ([flow.go:57](../../plan/flow.go)); the multi-flow convergence story lives in the core e2e tests, not `plan`. | Explicit non-goal. |
| NON-GOAL (interim) | Any of the above, until a user pulls it. Option (b) (`EventTool` via `Builder.Tool`) covers the capability today. | Held by gate 3. |

### Open questions

- Neutral snapshot type: is `json.RawMessage` (a state digest) enough for a
  governed node's journaled result, or does a caller need the projected `O`? The
  `Digest()` used by `AttestedEventTool` ([govern/govern.go:198](../../govern/govern.go))
  suggests a digest is the natural neutral form, but it is opaque to a downstream
  node that wants the actual state.
- Footprint correspondence: `plan` trusts the caller's declared `Footprint` to
  match the gsm event's `Writes`. Should there be a runtime cross-check (the
  adapter could expose the gsm footprint), or is the caller-keeps-the-contract
  model (as with `Safety.IdempotencyKey`) sufficient?
- Does the attested path (`policy_digest`, `actor`, `on_behalf_of`) belong on the
  governed node's journaled record too, so a `plan`-authored governed run is as
  auditable as an `AttestedEventTool` run? Likely yes, but it widens the neutral
  port surface.

## Bottom line

Architecturally the feature is sound and lowers cleanly: it fails only the
"pulled, not pushed" gate. Until a user pulls it, register a `govern.EventTool`
through the existing `Builder.Tool` (option b, zero new code). Build the
first-class `Govern` node (option a: a neutral core `Governor` port + a `govern`
adapter + a `plan` constructor) when a design partner asks to author governed
effects as reified, footprint-aware, conformable flow nodes rather than as opaque
tools.
