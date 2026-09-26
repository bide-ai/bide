# Flows (the `plan` builder)

The `plan` package is rung 1 of the [expression surfaces](../design/expression-surfaces.md) stack: a
Go-embedded flow builder you author as typed handles, compiled to the same journal-backed runtime as
plain Go. It adds a way to *author*, never a way to *execute*. Every node lowers to a memoized `Do`
step, so a flow inherits at-most-once side effects, halt-on-ambiguity, and durable resume for free,
and it can be checked against its declared shape (conformance).

Requires Go 1.27 (the builder uses generic methods).

## When to reach for it

Plain Go plus the durable primitives ([durable steps](durable-steps.md)) already gives you the
guarantees and full type safety. Reach for `plan` when you want the flow to be a *value*: a declared
topology you can render, diff, version, hand to a visual builder, and, above all, prove a run
**followed** (did the run do what the diagram said?). If you do not need a declared topology, plain Go
is simpler and wins on every other axis. See the [design note](../design/expression-surfaces.md) for
the reasoning.

## A flow, end to end

```go
import (
    agent "github.com/dayna/go-agents"
    "github.com/dayna/go-agents/plan"
)

f := plan.New[Order, Receipt]("triage")                  // input and output pinned here

classify := f.Step("classify", classifyOrder)            // Order -> Assessment (types inferred from the func)
reserve  := f.Step("reserve", reserveInventory)          // Assessment -> Reservation (a non-idempotent effect)
finalize := f.Step("finalize", finalizeReceipt)          // Reservation -> Receipt
decline  := f.Step("decline", declineReceipt)            // Assessment -> Receipt

f.Switch(classify,                                       // route on classify's output
    plan.When(func(a Assessment) bool { return a.Rush }, reserve),
    plan.Else(decline),
)
f.Edge(reserve, finalize)

flow, err := f.Build()                                   // validates the whole graph; returns *plan.Flow
if err != nil {
    return err
}

out, err := flow.Run(ctx, store, runID, order)           // Order in, Receipt out
```

`store` is any `agent.Durable` (an in-memory store for tests; `store/sqlite` or `store/postgres` for
production). `runID` is the durable identity: re-running the same `runID` resumes from the journal.

## The pieces

- **`New[In, Out](name)`** pins the flow's input and output types at construction, so the boundary is
  checked there rather than by an afterthought.
- **Nodes** are builder methods that return a typed `Handle`:
  - `Step[I, O](name, func(I) (O, error))` wraps arbitrary Go. `I` and `O` are inferred from the func.
  - `Tool[I, O](name, agent.Tool)` runs a tool; give `I`/`O` explicitly (they say how to JSON-encode
    the input and decode the result).
  - `Model[I, O](name, prompt)` is a model turn. In rung 1 it is a **stub that returns an error**
    (model binding is not wired yet); do not use it in a flow you expect to complete.
  - The string `name` is the node's **durable journal key**: it must be unique (Build enforces it) and
    stable across code edits, because resume finds a step by this name. It is not just a label.
- **Wiring** takes handles, so a miswired connection does not compile:
  - `Edge[M](from, to)` connects a producer to a consumer, unifying the connecting type `M`.
  - `Switch[M](over, When(pred, to)..., Else(to))` routes on a node's output to exactly one arm. In
    rung 1 arms do not reconverge: each arm runs to a terminal that produces `Out`.
- **`Build()`** validates whole-graph coherence (entry consumes `In`, every terminal path produces
  `Out`, names unique, no unreachable node, at most one `Else` per switch) and freezes the spec into a
  `*Flow`. Errors name the offending node.
- **`Run(ctx, store, runID, in)`** drives the flow sequentially on the runtime and returns the typed
  output. It adds no executor.
- **`RenderMermaid()`** renders the *declared* topology (contrast `agent.RenderMermaid`, which renders
  the *actual-ran* path from the journal).
- **`Conform(ctx, store, runID)`** checks a run's journal against the declared topology and returns
  whether it followed the graph (`ok`), the divergences, and an error.

## What you get, and the semantics to know

Every node lowers to a memoized `Do` step under a two-phase attempt/result guard, so:

- **At-most-once with halt, automatic.** Before a node's body runs, `Run` journals an `attempt:<name>`
  marker; after it succeeds, it journals the result `<name>`. On resume: a recorded result replays
  (the body does not re-run); an attempt with no result means the outcome is unknown, so **`Run` halts
  with a `*plan.HaltAmbiguous` rather than re-firing the body.** A `Switch` choice is journaled as its
  own step `switch:<over>` and replayed, so a resumed run takes the branch it originally took;
  predicates must therefore be pure functions of the node's output.
- **Halt is currently unconditional per node.** Because the attempt marker is written before the body,
  *any* crash inside a node, even a side-effect-free one, halts on resume. This is safe by default (it
  never double-fires), but completing after a mid-node crash then requires resolving the halt out of
  band (record the halted node's result, then continue), which is only safe when that node has no side
  effect. A future refinement will let a node declare it is read-only or idempotent so it re-runs on
  resume instead of halting.
- **Conformance.** Because the flow is authored and the actual path is derived from the journal, a run
  can be proven to have followed the declared topology, at node-visitation granularity plus the
  journaled branch choice. The blind spot: conformance sees *that* a node ran, not what its Go body did
  inside, so a node whose interior must be checked should be split into smaller nodes.

## Cryptographic conformance

`Conform` proves a run followed the declared graph *against the flow value in memory*. Cryptographic
conformance makes the same claim **offline-verifiable**: it lets an auditor who was not present at
runtime, and who does not trust your database, confirm that a run committed to **this** declared
topology.

The mechanism is a topology digest journaled inside the run, so the audit layer's Merkle tree and
signed tree head (STH) cover it:

- **`flow.Digest()`** returns a deterministic SHA-256 (hex) fingerprint of the FROZEN topology: the
  flow name and boundary types, every node's name and kind and I/O types, every edge, and every
  `Switch` and its ordered arms, serialized in insertion order (never by map iteration). Two builds of
  the same diagram produce the same digest; any change to the topology (a renamed or retyped node, an
  added or reordered edge, a changed arm) produces a different digest. It commits to topology, not to
  node bodies, mirroring `Conform`.
- **`Run` records the digest first.** The first durable step of every run is the reserved record
  `flow:digest` (a `StepValue` whose result is the hex digest), memoized on resume. Because it lives in
  the journal, an `audit.TreeHead` over the run commits to it like any other record.
- **`Conform` verifies the journaled digest** equals the current flow's `Digest()`. A mismatch is a
  divergence ("ran against a different topology"); the `flow:digest` record itself is recognised as an
  internal record of the run, never an unexpected step.

The full offline-verifiable flow ("the run followed the signed diagram"):

```
Run(store, runID, in)                     // journals flow:digest first, then the nodes
  -> th  := audit.NewTreeHead(store, runID, ts)   // Merkle commitment over the whole journal
  -> sth := audit.SignTreeHead(th, priv)          // anchor this out of band
  -> bundle := audit.ProveRecord(store, runID, idxOf("flow:digest"), sth)  // RFC 6962 inclusion proof
  -> bundle.Verify(pub)                    // signature authentic + flow:digest included under the signed root
  -> decode(bundle.Record) == flow.Digest()       // the proven digest is THIS declared topology
  -> flow.Conform(store, runID)            // the journaled PATH followed the declared graph
```

Given only the signed tree head, the proof bundle, and the signer's public key (obtained out of band),
the auditor checks all of this offline: the signature is authentic, the `flow:digest` record is
included under the signed root, and the proven digest equals the declared flow's `Digest()`. Tampering
with the record breaks the inclusion proof, so a forged topology digest cannot be passed off as
committed. The `plan` package stays audit-free (journaling the digest needs only `store.Do`); the
inclusion-proof step lives at the caller layer, where `audit` is available. See
`proveTopologyConformance` in `examples/plan/main.go` and its test for the worked demonstration.

## Cryptographic conformance

`Conform` proves the run followed the declared graph *against the same process's copy of the flow*.
Cryptographic conformance goes one step further: it makes that claim **offline-verifiable** by an
auditor who never trusts your process, your database, or your logs. The property proven is precise:
*this run committed to THIS declared topology*.

Two pieces make it work:

- **A topology digest.** `flow.Digest()` returns a deterministic SHA-256 (hex) of the *frozen* spec:
  the flow name, each node's name + kind + input type + output type, every edge, and each `Switch`
  with its ordered arms. It is computed by walking the insertion-ordered spec (never a map), so it is
  stable across builds and processes and changes whenever the topology changes (a renamed or retyped
  node, an added or reordered edge, a changed arm). It commits to topology, not to node bodies.
- **A journaled record the audit layer covers.** The first thing `Run` records is the digest, as a
  durable step under the reserved name `flow:digest` (memoized on resume). Because it lives in the
  journal, the [`audit`](../../audit) package's Merkle tree and signed tree head commit to it like any
  other record.

The flow, end to end:

1. `flow.Run(ctx, store, runID, in)` executes the flow. Its first journal record is `flow:digest`.
2. `audit.NewTreeHead(ctx, store, runID, ts)` then `audit.SignTreeHead(th, priv)` commit to the run's
   journal with a signed tree head (STH). Anchor the STH and its key in a separate trust domain; that
   is what makes it tamper-evident (see the `audit` package security model).
3. `audit.ProveRecord(ctx, store, runID, i, sth)` builds an RFC 6962 inclusion proof for the
   `flow:digest` record (index `i`), bundled with the STH.
4. An auditor holding only the bundle and the signer's public key (obtained out of band) checks
   `bundle.Verify(pub)` (the STH signature is authentic and the record is included under the signed
   root) and that the proven digest equals the declared flow's `flow.Digest()`. Together: **the run
   committed to this signed diagram.**

`Conform` closes the loop on the *path*: it recognizes `flow:digest` as an internal record (never a
divergence) and verifies the journaled digest **equals** the current flow's `Digest()`. A mismatch is
reported as a divergence, "ran against a different topology": the run executed under a different
declared graph than the flow now describes. So `Conform` covers node-visitation and branch choices,
and the digest + inclusion proof cover *which topology* the run committed to, verifiable offline.

The `examples/plan` demo prints this after a clean run
(`Cryptographic conformance: the run committed to the declared topology under the signed tree head`),
and `TestCryptographicConformance` there proves it in-process, including that a tampered `flow:digest`
record no longer verifies under the signed root.

## Limits in rung 1

- `Model` is a stub (returns an error); real model binding is not wired.
- No fan-in (`Join`) and no back-edges (`Loop`) yet; a flow is a forward DAG with branches.
- Halt is unconditional per node (no Safety classification yet; see above).
- Requires Go 1.27.

## A runnable example

`examples/plan` is a self-contained module: it builds an order-triage flow, prints the declared
diagram, runs it against a sqlite-backed store, conforms the run, and proves cryptographic conformance
(a signed tree head over the run plus an inclusion proof that the journaled topology digest equals
`flow.Digest()`). Its cross-process test crashes mid-run and resumes in a fresh process, proving the
side effect fires at most once and the run either completes or halts. Run the demo with
`cd examples/plan && GOWORK=off go run .`.

See also the [design note](../design/expression-surfaces.md) for why the surface is shaped this way.
