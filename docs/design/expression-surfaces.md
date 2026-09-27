# Expression surfaces: one substrate, many front-ends

## Why this note exists

Building at an accelerated pace makes it cheap to add new ways to express an agent: a graph DSL, a
visual builder, a declarative config. That is a good option to have, and this project is not opposed
to it. The danger is not capability, it is coherence: when adding a surface is cheap, it is easy to
add one that quietly forks the product and dissolves what makes it differentiated. This note records
the principle that lets us accommodate new surfaces without ever losing a core principle, so the
decision is on record before any surface work starts.

## The invariant

**There is one substrate, and every expression surface lowers to it.**

The substrate is the journal-backed runtime: the append-only journal, memoized steps (`Do`), the
tool and safety layer, and the roughly forty-line loop. From it come the guarantees, at-most-once
side effects, durable resume, the offline-verifiable audit trail, and provably convergent governed
state. Those guarantees live *below* the authoring surface, not in it.

So the rule for any new way to express an agent is absolute:

> A surface may add a new way to author. It may not add a new way to execute. Every surface compiles
> down to the same journal-backed runtime and inherits its guarantees. A surface that defines its own
> execution or durability semantics is a fork, not a front-end, and is out of bounds.

This is the compiler pattern: many front-ends, one intermediate representation, one backend. The
runtime is the backend; pure Go control flow is simply the first front-end. A graph DSL would be a
second front-end that emits the same primitives (events, tools, steps) and runs on the same journal.
Because everything lowers to one core, the guarantees are preserved by construction, and there is
never a second runtime to keep correct.

## The differentiation rule

Multiple surfaces can make the project *more* differentiated, not less, but only under one condition:
**the substrate is always the headline, and a surface is a convenience that inherits it.** "Write
plain Go or a graph, both with at-most-once and a verifiable trail" is a sharper position than either
pure-Go-only or graph-only, because it neutralizes the "but I want a graph" objection while keeping
the thing no one else has.

The failure mode to avoid: letting a surface become the story. If the graph becomes the pitch, the
project is competing on a graph framework's own turf and has given up its advantage. Come for the
surface you already know; stay for the guarantees.

## The gate for adding a surface

Before building any new expression surface, it must pass both:

1. **Does it lower to the core?** Does it inherit at-most-once and the audit trail by compiling to the
   journal-backed runtime? If it needs its own execution or durability model, it is a fork. Hard no.
2. **Does the substrate stay the headline?** After adding it, is the runtime still the star and the
   surface a convenience? If the surface becomes the pitch, no.

Pass both, build it. Fail either, hold. These two gates protect the architecture. Timing and priority
are the author's call, not a gate.

## Derived truth survives authoring

One core principle constrains even a fully-built authoring surface: an authored graph is a diagram you
trust; a derived graph is reconstructed from the journal, so it is exactly what ran. Whatever surface
an agent is authored in, the runtime keeps deriving the actual-ran graph from the journal
(`RenderMermaid`), so the verify, do not trust stance holds regardless of how the agent was written.
Authoring is a convenience for the writer; the journal remains the source of truth for the auditor.

## What a layer up looks like (rung 1 sketch)

This began as an illustrative sketch to make the invariant concrete; rung 1 (the `plan` builder) is
now built to it, feature-complete (see the [Flows guide](../guides/flows.md)). The section is retained
as the design rationale for the shape.

Surfaces stack in rungs, each compiling to the one below and ultimately to the journal-backed core:

- Rung 0 (today): plain Go plus the durable primitives.
- Rung 1: a Go-embedded flow DSL, a reified composition you author as data.
- Rung 2: a declarative config (for example YAML) loaded into the rung-1 builder.
- Rung 3: a visual builder that emits the rung-2 config.

Rung 1 is the first and most load-bearing. Studying how other frameworks express a graph pointed to
a specific shape: pass typed values along the flow (as Google's ADK does), reject stringly-typed node
names and a shared reducer-merged state object (LangGraph's footgun class), and never infer edges
from types (the invisible-control-flow complaint against LlamaIndex Workflows). The form that holds up
is a builder of typed step handles: each constructor returns a handle carrying the step's input and
output types, and wiring takes those handles, so branches, fan-in, and back-edges all name their
targets by a compiler-checked value rather than by a string. Go 1.27 generic methods (already the
workspace floor, so no version cost) make the constructors ergonomic; they are a convenience, not the
mechanism.

The flow input and output are pinned once, at construction. A step handle names a durable journal key
(explicit and author-owned) and carries its types:

```go
f := plan.New[Ticket, Result]("triage")                    // input and output pinned here

classify := f.Model[Ticket, Class]("classify", classifyPrompt)  // types explicit: a prompt infers nothing
rec      := f.Tool("lookup", lookupTool)                        // Class -> Record inferred from the tool
esc      := f.Step("escalate", escalateFn)                      // Record -> Result inferred from the func
res      := f.Step("resolve", resolveFn)

f.Edge(classify, rec)                                           // checks classify.Out == rec.In
f.Switch(rec,                                                   // routes on rec.Out (Record)
    plan.When(func(r Record) bool { return r.Urgent }, esc),
    plan.Else(res),
)

flow, err := f.Build()                                          // whole-graph checks; *Flow[Ticket, Result]
out, err  := flow.Run(ctx, runID, ticket)                       // Ticket in, Result out; same guarantees
```

The constructor and wiring signatures. A `Handle[I, O]` satisfies `Producer[O]` and `Consumer[I]`, so
each wiring call unifies the connecting type; constructors are generic methods on the concrete builder
(legal on Go 1.27):

```go
func New[In, Out any](name string) *Builder[In, Out]               // In and Out are mandatory
func (*Builder[In, Out]) Step[I, O any](name string, fn func(I) (O, error)) Handle[I, O]
func (*Builder[In, Out]) Tool[I, O any](name string, t Tool[I, O]) Handle[I, O]
func (*Builder[In, Out]) Model[I, O any](name, prompt string) Handle[I, O]   // I, O explicit
func (*Builder[In, Out]) Edge[M any](from Producer[M], to Consumer[M])
func (*Builder[In, Out]) Switch[M any](over Producer[M], arms ...Arm[M])
func (*Builder[In, Out]) Join2[A, B, O any](name string, a Producer[A], b Producer[B], fn func(A, B) (O, error)) Producer[O]
func (*Builder[In, Out]) Loop[T any](from Producer[T], back Consumer[T], max int)
func (*Builder[In, Out]) Build() (*Flow[In, Out], error)
```

Because `New` fixes both `In` and `Out`, the boundary is checked at construction, not by an optional
assertion. Each `Edge`, `Switch`, `Join2`, and `Loop` type-checks its endpoints at the call site: a
mismatch does not build, and the error names the handles, not an erased list entry. Whole-graph
coherence (the entry step consumes `In`, every terminal path produces `Out`, names are unique, no step
is unreachable) is validated at `Build`, where the errors the type system cannot express surface with
the step names attached. `Model` restates its `I, O` because a prompt string gives nothing to infer
from; `Step` and `Tool` infer both from the func or the tool.

Handles are what make the hard topologies expressible and safe. A branch is a `Switch` over a producer
handle whose arms are handles; fan-in is `Join2`/`Join3` taking typed producer handles and a
`func(A, B) (O, error)` that fixes every type; a back-edge is `Loop` naming an earlier handle whose
input type the current output must match. None of these is expressible in a handle-less chain, where a
single running value cannot name two producers or a differently-typed earlier target without falling
back to stringly-typed names, the footgun this surface exists to reject. The one case Go still cannot
type is ragged fan-in of a statically-unknown number of differently-typed inputs (no variadic type
parameters); that uses a fixed-arity join or the plain-Go escape hatch.

Why handles and not a fluent tip-threading chain. A fluent chain reads well but carries exactly one
live value type, so it expresses a linear spine and a reconvergent branch and nothing else: fan-in
needs two named producers and a loop needs a differently-typed earlier target, and one tip can name
neither, so both regress to string targets. Handles cost one line per named step and buy the whole
topology, type-checked. The generic-method constructors keep the per-step ergonomics (types inferred
for `Step` and `Tool`, no separate construction phase) that made the fluent form attractive, without
its topology limits.

How it lowers, which is what keeps it a front-end and not a fork:

- A step is a durable unit. Each maps to a `Do`-memoized call, so at-most-once, halt-on-ambiguity, and
  resume are inherited for free. The DSL adds no executor.
- Routing is journaled, not just evaluated. A `Switch` records its branch choice as its own
  `Do`-memoized step, and its predicate must be a pure function of the step output it reads, so a
  resumed run replays the recorded branch instead of re-deciding. Without this a predicate that read a
  clock or a map could route differently on replay, the exact non-deterministic-control-flow bug this
  project positions against. A back-edge lowers to bounded iteration whose bound is a compile-time
  constant, not a counter held off the journal.
- Fan-out and join lower to sequential journaled steps plus a memoized rendezvous, never to a
  concurrent scheduler. A join barrier would be a second execution model beside the loop, which the
  invariant forbids; if real parallelism is ever needed it is expressed as concurrent `Do` steps the
  existing runtime already supports, with the join as a memoized step.
- State is the journal, not a separate shared object with reducers. A handle is a compile-time name for
  a step; the value it stands for is, at rest, that step's journaled `Do`-result keyed by its string
  name, so wiring carries types at build time and values through the journal at run time. There is no
  durable channel beside the journal. This is the divergence from graph frameworks that carry their own
  mutable state model, and it is what keeps the substrate's semantics intact.
- The escape hatch carries its safety class. A `Step` wraps an arbitrary `func(In) (Out, error)`, and a
  whole flow can be written in plain Go and still lower to the journal. But `Do` protects only the step
  boundary: a non-idempotent side effect inside a `Step` must declare its safety (ReadOnly, Idempotent)
  and footprint like any tool, or wrap its own `Do`, or a crash mid-step will replay it. The DSL is a
  convenience over control flow, never a cage, and never a bypass of the safety layer.

What reifying the flow buys (the reason to have the layer at all): the flow is a value, so it can be
rendered from the declaration and not only from a run, validated for type mismatches at build time and
for unique step names and reachability at `Build`, diffed and versioned as a topology, and used as the
hook a visual builder emits into.

The conformance property (the layer expressed through the accountability identity): because the flow
is authored and the actual path is derived from the journal, a run can be checked against its declared
graph at step-visitation granularity, and, because the branch choice is itself journaled, against the
declared routing. It proves the run followed the declared topology or flags where it diverged. A
graph-first framework cannot offer this, because for it the graph is the execution and there is no
independent record to check against. The one blind spot is the interior of a `Step`: conformance sees
that a step ran, not what arbitrary Go did inside it, so a step whose interior must be checked should
be decomposed into smaller steps.

Raising to higher rungs (what reification is for): the boundary is clean, topology raises and behavior
does not.

- Rendering down (flow to diagram) is direct and lossless for structure: walk the `*Flow` value and
  emit Mermaid, DOT, or a UI graph, each node labelled by its journal key and its `I, O` types. Because
  handles can represent fan-in and loops, the result is a real DAG, not just a linear chain.
- Authoring up (config or visual builder to flow) composes a registry of pre-registered Go blocks by
  name. Rung 3 (visual) emits rung 2 (config, for example YAML), which loads the rung-1 builder. The
  visual layer can place a `Step` node and wire it, but it cannot write `escalateFn`; escape-hatch
  bodies and `Switch` predicates stay as named Go the config references, never authors. This is the
  same boundary as the conformance blind spot: structure is visible, the interior of a step is not.
- Type-checking changes altitude. In the Go builder, `Edge` unifying the connecting type is a
  compile-time check. Config and visual graphs are data, so the Go compiler cannot see them; their type
  correctness is validated at load time, at `Build`, against the registered blocks' declared types.
  Each rung trades some compile-time checking for load-time validation, and all of them bottom out in
  the same reified value, which lowers to the same runtime, so the guarantees hold regardless of which
  rung authored the flow.

The payoff is four consistent views over one topology: the authored diagram (rendered from the
declaration), the actual-ran diagram (derived from the journal), conformance (whether they match), and
a visual editor (an editor over the same value). Structure moves freely between them; only behavior,
the predicate logic and the step bodies, stays put as Go.

## When rung 1 earns its place (and when plain Go wins)

The same triage flow, written both ways.

A. Plain Go (rung 0):

```go
func Triage(ctx agent.Context, ticket Ticket) (Result, error) {
    class, err := agent.Do(ctx, "classify", func() (Class, error) {
        return classifyModel.Run(ctx, ticket)
    })
    if err != nil {
        return Result{}, err
    }
    rec, err := agent.Do(ctx, "lookup", func() (Record, error) {
        return lookupTool(ctx, class)
    })
    if err != nil {
        return Result{}, err
    }
    if rec.Urgent {                                             // routing is an ordinary if
        return agent.Do(ctx, "escalate", func() (Result, error) { return escalateFn(rec) })
    }
    return agent.Do(ctx, "resolve", func() (Result, error) { return resolveFn(rec) })
}
```

B. The handle builder (rung 1):

```go
f := plan.New[Ticket, Result]("triage")
classify := f.Model[Ticket, Class]("classify", classifyPrompt)
rec      := f.Tool("lookup", lookupTool)
esc      := f.Step("escalate", escalateFn)
res      := f.Step("resolve", resolveFn)

f.Edge(classify, rec)
f.Switch(rec,
    plan.When(func(r Record) bool { return r.Urgent }, esc),
    plan.Else(res),
)
flow, err := f.Build()
```

Both lower to the same journal steps, so at-most-once, halt, and resume are identical; both are
type-checked by the compiler. The difference is that A is code and B is a value.

Compared side by side with the same flow written in plain Go (rung 0), the builder wins on fewer
axes than the typing story suggests. Plain Go already gives the full guarantees (every `agent.Do` is
a memoized step, so at-most-once, halt, and resume hold) and already type-checks the data flow (the
compiler verifies each step's output feeds the next), with better error messages and no new concepts.
The builder's typed handles mostly recover a property straight-line Go never lost, so type safety is
not the reason to reach for it.

The builder earns its place on one thing: the flow becomes a reified value. That is what makes it
possible to render the topology from the declaration, statically validate it, diff and version it,
target it from a visual builder, and, the one capability no graph-first framework can offer, check a
run against its declared graph (plain Go has an actual-ran path derived from the journal, but no
authored graph to compare against). So rung 1's justification is reification and conformance, not
typing. If an author does not want a declared topology to visualize, diff, or conform against, plain
Go dominates on every other axis, which is why the pitch leads with "prove the run followed the
diagram" and treats the typed builder as the ergonomic on-ramp, never the reason.

## Current status

Rung 1 (the `plan` builder) and rung 2 (declarative config over the builder) are both built (see
[rung2-config.md](rung2-config.md) and the [Flows guide](../guides/flows.md)); a rung-3 visual builder
would emit rung-2 config. Each surface sits above the same journal-backed core and is added through the
two-question gate above, at the author's discretion on timing.
