# Rung 2: declarative config over the plan builder

## Why this note exists

Rung 1 (`plan`, built) is a Go-embedded flow builder. Rung 2 is the next surface in the
[expression-surfaces](expression-surfaces.md) stack: a declarative config (YAML or JSON) that
describes a flow's topology and loads into the rung-1 builder to produce the same `*Flow`. Rung 2 is
now built (registry + `Load`, with `Join`/`Loop`/`Safety` expressible in the config); see the
[Flows guide](../guides/flows.md). This note records the design and the pressure-test decisions behind
it. Rung 2 had one genuinely hard decision rung 1 did not. It passes the two architectural gates: it
lowers to the plan builder, and the substrate stays the headline.

## What rung 2 is, and what it is not

Rung 2 expresses **topology as data**: nodes (by name and kind), edges, and switch arms, referencing
Go behavior by name. It does not express behavior. Per the "raising to higher rungs" rule in the
expression-surfaces note: topology raises, behavior does not. A config places and wires nodes; it
cannot write a `Step`'s body or a `Switch`'s predicate. Those live in Go and are referenced by name
through a registry.

Value: a flow becomes a portable, diffable, tool-emittable artifact (the rung-3 visual builder emits
rung-2 config), and because it still compiles to a `*Flow`, it inherits `RenderMermaid`, `Conform`,
and the topology `Digest` unchanged. That last point is the headline: a config-loaded run is
**cryptographically conformable to its config**, so "the run followed the config" is offline-provable
the same way "the run followed the diagram" already is.

## The hard decision: compile-time generics vs data

Rung 1 is generic at compile time: `New[In, Out]`, `Step[I, O]`, `Edge[M]`. You cannot call a generic
method with type arguments taken from a data file. Rung 2 therefore needs two things rung 1 lacks:

1. A **registry** that maps names to typed Go blocks, so a config can reference behavior by name.
2. A **dynamic construction path** that builds the spec from data and the registry, checking types at
   **load** time rather than compile time.

This is feasible with almost no new machinery, because the `plan` internal spec is already
reflection-based: `node` carries `inType`/`outType` as `reflect.Type` and a `run func(context.Context,
any) (any, error)` closure, and `Build`'s validation already works off those `reflect.Type`s. The
dynamic path populates the same `node`/`edge`/`branch`/`builderCore` values the generic constructors
do, then runs the existing `Build` validation. Nothing about execution changes: the loaded flow runs
on the same `Run`, journals the same records, and gets the same `flow:digest`.

## Proposed API

Registration captures each block's types via `reflect.TypeFor`, so the config never has to declare
node types; they flow from the registered block:

```go
reg := plan.NewRegistry()
plan.RegisterStep(reg, "classify", classifyOrder)        // captures Order -> Assessment
plan.RegisterStep(reg, "reserve", reserveInventory)      // Assessment -> Reservation
plan.RegisterStep(reg, "finalize", finalizeReceipt)      // Reservation -> Receipt
plan.RegisterStep(reg, "decline", declineReceipt)        // Assessment -> Receipt
plan.RegisterTool[Assessment, Hit](reg, "lookup", lookupTool)   // agent.Tool is untyped: I/O explicit
plan.RegisterPredicate(reg, "rush", func(a Assessment) bool { return a.Rush })
```

Loading supplies the boundary types at the caller's Go call site (the caller knows them); the loader
fills the middle from data and validates against the registry and against `In`/`Out`:

```go
flow, err := plan.Load[Order, Receipt](configBytes, reg)   // *Flow[Order, Receipt], or a load error
```

`Load` is where compile-time typing hands off to load-time validation: it parses the config, resolves
each block reference against the registry (an unknown name is a load error naming it), assembles the
spec, and runs the existing whole-graph validation plus a type-compatibility check on every edge and
switch using the registered blocks' `reflect.Type`s and the `In`/`Out` type parameters. A mismatch is
a load error that names the offending nodes and their types, which is a better message than the raw
generic-inference error rung 1 would emit at compile time.

## The config schema

A config is pure topology plus block references. Types are not restated in the config; they come from
the registry. Illustrative YAML (JSON is the on-disk default; see below):

```yaml
flow: triage
in: Order              # optional, validated against Load's In for readability
out: Receipt           # optional, validated against Load's Out
nodes:
  - {name: classify, block: classify}
  - {name: reserve,  block: reserve}
  - {name: finalize, block: finalize}
  - {name: decline,  block: decline}
wiring:
  - switch: classify
    when: [{pred: rush, to: reserve}]
    else: decline
  - edge: [reserve, finalize]
```

`in`/`out` are optional documentation the loader cross-checks against `In`/`Out.String()`; the real
type source is the registry.

## Format and dependencies

Default to **JSON via the standard library**: the config struct has json tags, and `Load` accepts
JSON bytes or an `io.Reader`. This keeps the core module free of a YAML dependency. YAML is a thin,
optional adapter (decode YAML into the same struct with a caller-supplied or separate-module YAML
decoder), not a core dependency. The loader is format-agnostic: it operates on the decoded struct, so
JSON and YAML are just two front-ends to the one loader.

## Where it lives

In the `plan` package itself, not a subpackage. The dynamic path must populate the unexported
`builderCore` (and call the unexported `register`/`Build`/`seal`), so a separate `plan/config`
subpackage cannot host `Load` without `plan` growing a new exported lowering API. `Load` therefore
lives in `plan`. It imports only the standard library plus the core, exactly like the rest of `plan`;
the architecture guard (`TestPlanNoAdapterImports`, no new executor) must keep passing. Registration and `Load` add authoring, not execution, so the no-new-executor invariant holds
trivially.

## What is inherited for free

A loaded `*Flow` is an ordinary flow, so: `Run` (sequential, at-most-once, halt-on-ambiguity),
`RenderMermaid` (the declared topology, now sourced from the config), `Conform` (the run followed the
declared topology), and `Digest` + the `flow:digest` record (cryptographic conformance). The digest
now commits to the config-derived topology, so a signed tree head over the run proves the run followed
**this config**, offline. Rung 2 gets the whole accountability story with no new code in that path.

## The real cost

Moving from Go to data trades compile-time type checking for load-time validation. A miswired config
does not fail at `go build`; it fails at `Load`, at process start. This is expected and is the same
tradeoff every config/visual surface makes (see the competitor note: nobody type-checks a data config
at compile time). The mitigation is that `Load`'s errors are worded and name the nodes and types,
which is more useful than the compile-time generic-inference error, so the surface that gives up
compile-time safety gives back better diagnostics.

## Staged build plan

1. **Registry + dynamic construction** in `plan`: `Registry`, `RegisterStep`/`RegisterTool`/
   `RegisterModel`/`RegisterPredicate`, and the internal path that builds a `builderCore` from a
   decoded config struct + the registry. Reuse `Build` for validation; add edge/switch
   type-compatibility from `reflect.Type`.
2. **`Load[In, Out]`** and the config struct (JSON, stdlib), with load-time validation and worded
   errors. Optional `in`/`out` cross-check.
3. **Example + docs**: a `examples/plan` config file loaded and run, showing render + conform +
   the digest proof against the config; a Rung 2 section in the Flows guide.
4. Optional later: a YAML adapter module; the rung-3 visual builder that emits this config.

## Built since this note, and remaining non-goals

Built after this note was first written: `Model` is really bound now (not a stub), and fan-in (`Join`)
and bounded loops (`LoopBack`) are expressible in the config, matching rung 1's feature set. Behavior
still stays as registered Go referenced by name (predicates, step bodies, tools, merge functions are
Go, never config expressions).

Remaining non-goals:
- No behavior in config: there is no config expression language; behavior is always registered Go by
  name.
- Fan-in is fixed-arity (`Join2`/`Join3`); no ragged or unbounded fan-in.
- `Model` decodes the response as JSON into `O` (no derived response schema yet).
- No visual builder (rung 3); rung 2 is the data format rung 3 would target.

## Resolved after pressure-test

Two adversarial reviews (type-soundness and DX/competitive) confirmed the mechanism is sound and
settled the open questions. Decisions the build must follow:

- **`Load` lives in package `plan`.** It returns `*Flow[In, Out]` and populates the unexported
  `builderCore` via the existing `register`/`Build`/`seal`, so it cannot live in a subpackage.
  Signatures: `Load[In, Out](data []byte, reg *Registry) (*Flow[In, Out], error)` plus a
  `LoadReader[In, Out]` variant.
- **Registry is per-`Load`, explicit, and fresh.** `NewRegistry()` returns a new instance; a duplicate
  registration is an error, not an overwrite. No global mutable default.
- **`RegisterStep` infers `[I, O]` from the func; `RegisterTool[I, O]` and `RegisterModel[I, O]` take
  them explicitly** (an `agent.Tool` and a prompt carry no I/O types). Predicates:
  `RegisterPredicate[M](reg, name, func(M) bool)` captures `M`.
- **Predicate typing is checked at load**, a strict improvement over rung 1 (which only checks a
  predicate at its compile-time call site): `Load` verifies every arm's registered predicate `M` equals
  the switched node's output type via `reflect.Type` identity. Without this a mismatch would be a
  runtime type-assertion failure in `chooseArm`.
- **Edges match by nominal `reflect.Type` identity, not assignability** (`from.outType == to.inType`
  exactly; interface-typed seams that would work at runtime are rejected, same as rung 1's `Edge[M]`).
  This is reliable because `reflect.TypeFor[T]()` is canonical, matching what `Build` already does.
- **Entry is explicit and order is preserved.** The schema names the entry (an `entry:` field, or
  normatively `nodes[0]`), and `Load` appends nodes in config-array order, never by ranging a map. This
  keeps `entry` deterministic and the topology `Digest` stable across loads of the same file, which the
  cryptographic-conformance property depends on.
- **Two structural checks `Build` does not give:** reject a node that is both switched-over and has an
  outgoing edge (the runtime silently prefers the switch), and reject a `wiring[]` element that sets
  both or neither of `switch`/`edge` (a JSON union that `encoding/json` will not validate).
- **`Load` reports all failures at once**, names every unresolved block or predicate with near-miss
  suggestions, and flags unused registered blocks. A standalone `Validate(config, reg) error` lets drift
  be caught in CI, not at boot. This is the mitigation for config-vs-registry drift (the stringly-typed
  footgun re-created at rung 2); a later `go generate` codegen of a typed `RegisterAll` stub would move
  drift detection to `go build` and would lead the field, since MAF does not solve drift either.
- **A resolved view.** `Load` (or a render mode) can emit a type-annotated copy of the config so it is
  reviewable standalone without restating types in the authored file.
- **YAML is the authoring front-end; JSON is the interchange/tool-emit format.** Ship the YAML adapter
  in the same initiative; the core loader stays stdlib-JSON and dependency-free.
- **Optional `in`/`out` stay documentation only** (a `reflect.Type`-identity cross-check where present,
  never a `.String()` type source).

Competitive: the closest analog is Microsoft Agent Framework Declarative Workflows 1.0, which makes the
same "loads into the code type" move and binds by name through a factory registry, but has no type
safety between steps (namespaced mutable variables plus Power Fx string expressions) and no conformance
or audit property. The differentiator to lead with is that a config-loaded flow inherits `flow:digest`
and `Conform`, so a signed tree head proves offline that the run followed *this config*, which a
graph-is-execution framework structurally cannot offer.
