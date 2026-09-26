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

## Current status

Pure Go control flow is the only surface built today, and it is deliberately the low level so richer
surfaces can sit above it. A graph DSL or visual builder is a stated, credible option (see the
[Graphs](../../README.md#graphs) section), not a current work item. It is a demand-driven addition,
to be built when a user pulls it or the core has adoption, and only through the gate above.
