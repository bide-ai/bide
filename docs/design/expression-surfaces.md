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

Before building any new expression surface, it must pass all three:

1. **Does it lower to the core?** Does it inherit at-most-once and the audit trail by compiling to the
   journal-backed runtime? If it needs its own execution or durability model, it is a fork. Hard no.
2. **Does the substrate stay the headline?** After adding it, is the runtime still the star and the
   surface a convenience? If the surface becomes the pitch, no.
3. **Is it pulled, not pushed?** Is a real user or design partner asking for it, or is it speculative?
   Pre-adoption, a second front-end splits focus from proving the core wedge. If speculative, wait.

Pass all three, build it. Fail any, hold.

## Derived truth survives authoring

One core principle constrains even a fully-built authoring surface: an authored graph is a diagram you
trust; a derived graph is reconstructed from the journal, so it is exactly what ran. Whatever surface
an agent is authored in, the runtime keeps deriving the actual-ran graph from the journal
(`RenderMermaid`), so the verify, do not trust stance holds regardless of how the agent was written.
Authoring is a convenience for the writer; the journal remains the source of truth for the auditor.

## What a layer up looks like (rung 1 sketch)

This is an illustrative sketch, not a committed API. It exists to make the invariant concrete, and
it is subject to the gate above; it is not a work item.

Surfaces stack in rungs, each compiling to the one below and ultimately to the journal-backed core:

- Rung 0 (today): plain Go plus the durable primitives.
- Rung 1: a Go-embedded flow DSL, a reified composition you author as data.
- Rung 2: a declarative config (for example YAML) loaded into the rung-1 builder.
- Rung 3: a visual builder that emits the rung-2 config.

Rung 1 is the first and most load-bearing. A flow is declared as nodes and edges and compiled to a
normal agent that runs on the loop:

```go
flow := agentflow.New("triage").
    Model("classify", classifyPrompt).          // a model turn (a journaled step)
    Tool("lookup", lookupTool).                  // a tool node
    Route("classify", func(s State) string {     // a conditional edge
        if urgent(s) {
            return "escalate"
        }
        return "resolve"
    }).
    Node("escalate", escalateFn).                // an arbitrary Go node (the escape hatch)
    Node("resolve", resolveFn).
    Compile()                                    // returns *agent.Agent; lowers to the loop

out, err := flow.Run(ctx, runID, input)          // same runtime, same guarantees
```

How it lowers, which is what keeps it a front-end and not a fork:

- A node is a durable step. Each maps to a `Do`-memoized unit, so at-most-once and resume are
  inherited for free. The DSL adds no executor.
- An edge is control flow the compiler emits. `Route` is a router; a loop is bounded iteration.
  `Compile` turns the declared topology into the same control flow you would have written by hand.
- State is the journal, not a separate shared object with reducers. Nodes read and write through the
  run's journaled state. This is the divergence from graph frameworks that carry their own mutable
  state model, and it is what keeps the substrate's semantics intact.
- There is always an escape hatch to plain Go. A `Node` can be an arbitrary function, so the DSL is a
  convenience over control flow, never a cage. Going up a layer never costs expressiveness.

What reifying the flow buys (the reason to have the layer at all): the flow is a value, so it can be
rendered from the declaration and not only from a run, statically validated at build time
(unreachable nodes, dangling edges, type mismatches), diffed and versioned as a topology, and used as
the hook a visual builder emits into.

The conformance property (the layer expressed through the accountability identity): because the flow
is authored and the actual path is derived from the journal, a run can be checked against its declared
graph, proving it followed the declared topology or flagging exactly where it diverged. A graph-first
framework cannot offer this, because for it the graph is the execution and there is no independent
record to check against. Here there are both, so "the run did what the diagram said" is verifiable.

## Current status

Pure Go control flow is the only surface built today, and it is deliberately the low level so richer
surfaces can sit above it. A graph DSL or visual builder is a stated, credible option (see the
[Graphs](../../README.md#graphs) section), not a current work item. It is a demand-driven addition,
to be built when a user pulls it or the core has adoption, and only through the gate above.
