# Rung 2: declarative config over the plan builder

## Why this note exists

Rung 1 (`plan`, built) is a Go-embedded flow builder. Rung 2 is the next surface in the
[expression-surfaces](expression-surfaces.md) stack: a declarative config (YAML or JSON) that
describes a flow's topology and loads into the rung-1 builder to produce the same `*Flow`. This note
resolves the design before any build, because rung 2 has one genuinely hard decision that rung 1 did
not. It is subject to the same gate; the pull here is an explicit request to build toward it.

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
plan.RegisterTool(reg, "lookup", lookupTool)             // an agent.Tool + its I/O types
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

In the `plan` package (or a `plan/config` subpackage in the same module), so the dynamic path can
populate `builderCore` directly. It imports only the standard library plus the core, exactly like the
rest of `plan`; the architecture guard (`TestPlanNoAdapterImports`, no new executor) must keep
passing. Registration and `Load` add authoring, not execution, so the no-new-executor invariant holds
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

## Non-goals for the first cut

- No behavior in config: predicates, step bodies, tools, and models are registered Go, referenced by
  name. `Model` remains the rung-1 stub.
- No fan-in (`Join`) or back-edges (`Loop`) in the config until rung 1 has them.
- No visual builder (rung 3); rung 2 is the data format rung 3 would target.

## Open questions to pressure-test before building

- The `Register*` and `Load` signatures: is `Load[In, Out](bytes, reg)` the right shape, or should the
  registry be typed by boundary? How are predicates typed in the registry so the loader can check an
  arm's predicate against the switched node's output type?
- Type identity across the boundary: matching a config's declared `in`/`out` names and edge types to
  `reflect.Type` reliably (two types named `main.Order` in different packages, etc.).
- Whether the registry should be global or per-load, and how name collisions are handled.
- Error surface: exactly what a load error reports for an unknown block, a type mismatch, and a
  structural violation.
