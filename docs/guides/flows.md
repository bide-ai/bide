# Flows (the `plan` builder)

The `plan` package is a typed flow builder: a Go-embedded builder you author as typed handles,
compiled to the same journal-backed runtime as plain Go. It adds a way to *author*, never a way to
*execute*. Every node lowers to a memoized `Do` step, so a flow inherits at-most-once side effects,
halt-on-ambiguity, and durable resume for free, and it can be checked against its declared shape
(conformance).

Requires Go 1.27 (the builder uses generic methods).

## When to reach for it

Plain Go plus the durable primitives ([durable steps](durable-steps.md)) already gives you the
guarantees and full type safety. Reach for `plan` when you want the flow to be a *value*: a declared
topology you can render, diff, version, hand to a visual builder, and, above all, prove a run
**followed** (did the run do what the diagram said?). If you do not need a declared topology, plain Go
is simpler and wins on every other axis.

## A flow, end to end

<!-- docsnip: setup ctx context.Context; store agent.Durable; runID string; order Order; type Order struct{}; type Receipt struct{}; type Assessment struct{ Rush bool }; type Reservation struct{}; func classifyOrder(context.Context, Order) (Assessment, error); func reserveInventory(context.Context, Assessment) (Reservation, error); func finalizeReceipt(context.Context, Reservation) (Receipt, error); func declineReceipt(context.Context, Assessment) (Receipt, error); returns error -->
```go
import (
    "github.com/bide-ai/bide/agent"
    "github.com/bide-ai/bide/plan"
)

f := plan.New[Order, Receipt]("triage")                  // input and output pinned here

classify := f.Step("classify", classifyOrder)            // func(context.Context, Order) (Assessment, error)
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
  - `Step[I, O](name, func(context.Context, I) (O, error))` wraps arbitrary Go. `I` and `O` are
    inferred from the func. The body receives the ctx the step runs under, derived from the one passed
    to `Run`: it carries the caller's cancellation, deadline and values, so a long body should stop when
    `ctx` is done (return `ctx.Err()` or `context.Cause(ctx)`). A body that returns an error records no
    result, so on resume a default step halts on its attempt marker (`*agent.OutcomeUnknown`) and a
    `ReadOnly`/`Idempotent` step runs again. A default step must not pause: a body that returns a pause
    (an `Interrupt`, a pending approval) is `ErrConfig`, as for `agent.Step`; put the pause in a
    `ReadOnly` step of its own.
    `Join2`/`Join3` merge bodies take the same leading `ctx`. `When` predicates and `Switch` routing
    take no ctx: they must be pure functions of the value, since resume replays the recorded arm.
  - `Tool[I, O](name, agent.Tool)` runs a tool; give `I`/`O` explicitly (they say how to JSON-encode
    the input and decode the result).
  - `Model[I, O](name, prompt)` is a model turn: bind a model with `Builder.WithModel(m)` (or, in a
    declarative config, `Load`'s `WithLoadedModel`). The node renders `prompt` as a `text/template` over the
    typed input `I`, calls the bound model, and decodes the structured response into `O` (so `O` must
    be JSON-shaped and the prompt should ask for matching JSON). Build errors if a `Model` node has no
    bound model, naming it.
  - The string `name` names the node's **durable journal key**, `node:<name>`: it must be non-empty,
    unique, and contain no `:` (Build enforces all three; Run derives its keys, such as `node:<name>`
    and `switch:<name>`, with `:`), and stable across code edits, because resume finds a node by this
    name. It is not just a label.
- **Wiring** takes handles, so a miswired connection does not compile:
  - `Edge[M](from, to)` connects a producer to a consumer, unifying the connecting type `M`.
  - `Switch[M](over, When(pred, to)..., Else(to))` routes on a node's output to exactly one arm.
    Switch arms do not reconverge (each arm runs to a terminal producing `Out`); use `Join2`/`Join3`
    for fan-in of branches that both run, and a `LoopBack(max, pred, head)` arm (a `when` arm with `loop_max` in a config) for a bounded loop.
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

Every node runs as an [`agent.Step`](durable-steps.md) under its node key, so it has exactly a Step's
guarantees:

- **At-most-once with halt, automatic.** Before a default node's body runs, its Step claims the attempt
  marker `attempt:step:node:<name>`; after the body succeeds, it journals the result `node:<name>`. On
  resume: a recorded result replays (the body does not re-run, and the node costs one point read); an
  attempt with no result means the outcome is unknown, so **`Run` halts with an
  `*agent.OutcomeUnknown` rather than re-firing the body.** A `Switch` choice is journaled as its own
  record `switch:<over>` and replayed, so a resumed run takes the branch it originally took;
  predicates must therefore be pure functions of the node's output.
- **A halt is a Step halt.** Its `Op` is `agent.OpRef{Kind: agent.OpStep, ID: "node:<name>"}` (or
  `"node:iter:<n>:<name>"` inside a loop), and it is a pause (`agent.IsPause`), so `RecoverLoop`
  treats it as waiting, not failed. Once you know the node's true outcome, record it with
  `agent.ResolveHaltRef(ctx, store, halt.Ref(), agent.Outcome{Result: output})`, where `output` is the
  node's output value; the next `Run` continues past the node without running its body, feeding
  `output` downstream.
- **A node that provably never started is re-attempted.** A driver cancelled (or whose store failed)
  after claiming a node's marker and before calling its body records that the attempt did not start,
  and the next `Run` re-attempts the node under a numbered marker (`attempt:retry:<n>:step:node:<name>`)
  instead of halting. A process that dies in that gap records nothing, so its resume halts, which is
  safe.
- **Halt is per node, and conservative by default.** Because the attempt marker is written before the
  body, *any* crash inside a node halts on resume unless the node is classified safe to repeat. The
  default (no classification) never double-fires, but completing after a mid-node crash then requires
  resolving the halt. A node may instead declare a `Safety` (read-only or idempotent, via
  `ReadOnly()`/`Idempotent()` in Go) so it writes no marker and re-runs on resume instead of halting.
  A declarative config's `safety` may only lower that (see "Node and join safety"). `Safety` is not
  part of the digest, so a node may be relabelled between a crash and its resume: the marker is the
  attempt's recorded safety, so a node attempted as a side effect halts even if it is retry-safe now,
  and a node attempted as retry-safe (no marker) runs again under a claim if it is a side effect now.
- **A run keeps its flow and its input.** `Run` first records the run's start (`run:start`, see
  `agent.RunStart`): kind `agent.RunKindFlow`, the flow's name, and the JSON of the input. A later
  drive with an input whose JSON differs, under another flow's name, or of a run an `Agent` started is
  `ErrConfig` and records nothing (and an `Agent` refuses a flow's run the same way). `Run` then
  records the flow's digest and, on resume, refuses (`ErrConfig`) to continue a run that started under
  a different digest: its journal only means what it meant under that flow.
- **Reserved keys.** `node:`, `switch:` and `flow:` are reserved prefixes, like `run:` and `attempt:`,
  so an `agent.Step` a node's body runs cannot name one of the flow's records.
- **Conformance.** Because the flow is authored and the actual path is derived from the journal, a run
  can be proven to have followed the declared topology, at node-visitation granularity plus the
  journaled branch choice. The blind spot: conformance sees *that* a node ran, not what its Go body did
  inside, so a node whose interior must be checked should be split into smaller nodes.

## Cryptographic conformance

`Conform` proves the run followed the declared graph *against the same process's copy of the flow*.
Cryptographic conformance goes one step further: it makes that claim **offline-verifiable** by an
auditor who never trusts your process, your database, or your logs. The property proven is precise:
*this run committed to THIS declared topology*.

Two pieces make it work:

- **A topology digest.** `flow.Digest()` returns a deterministic SHA-256 (hex) of the *frozen* spec:
  the flow name, each node's name, block, kind, input type and output type (types with their full
  package path, so `a/model.Req` and `b/model.Req` differ), every edge, and each `Switch` with its
  ordered arms and their predicate names. It is computed by walking the insertion-ordered spec (never
  a map), so it is stable across builds and processes and changes whenever the flow changes (a
  renamed or retyped node, a node pointed at another block, an added or reordered edge, a changed
  arm or predicate). It commits to topology and to the names of the blocks and predicates it wires,
  not to the Go inside them.
  - A config node's block is the registered block it names (a join's is its merge block), and an
    arm's predicate is the registered predicate it names. In Go, a node's block is its own name unless
    `plan.BlockName("...")` gives another, and an arm's predicate has a name only when
    `When(...).Named("...")` (or `LoopBack(...).Named`) gives one. A Go flow that should share a
    config flow's digest gives the config's names.
  - This is digest v2 (`bide.plan.topology.v2`). `Run` refuses to resume a run whose `flow:digest`
    was recorded under v1, with an `ErrConfig` naming v1, because v1 does not commit to block or
    predicate names and so cannot show the run started under this flow; `Conform` reports such a run
    as a divergence. `flow.DigestV1()` still computes the v1 digest, to check a proof of a record an
    earlier version journaled.
- **A journaled record the audit layer covers.** After the run's start and before any node, `Run`
  records the digest, as a durable step under the reserved name `flow:digest` (memoized on resume). Because it lives in the
  journal, the [`audit`](../../audit) package's Merkle tree and signed tree head commit to it like any
  other record.

The flow, end to end:

1. `flow.Run(ctx, store, runID, in)` executes the flow. It journals `flow:digest` before any node.
2. `audit.NewTreeHead(ctx, store, runID, ts)` then `audit.SignTreeHead(th, priv)` commit to the run's
   journal with a signed tree head (STH). Anchor the STH and its key in a separate trust domain; that
   is what makes it tamper-evident (see the `audit` package security model).
3. `audit.ProveRecord(ctx, store, runID, i, sth)` builds an RFC 6962 inclusion proof for the
   `flow:digest` record (index `i`), bundled with the STH.
4. An auditor holding only the bundle and the signer's public key (obtained out of band) checks
   `bundle.Verify(pub)` (the STH signature is authentic and the record is included under the signed
   root), checks the STH's timestamp with `audit.CheckTimestamp`, and that the proven digest equals the declared flow's `flow.Digest()`. Together: **the run
   committed to this signed diagram.**

`Conform` closes the loop on the *path*: it recognizes `run:start` (which must name this flow),
`flow:digest`, a node's attempt markers and the not-started records of its claims as internal records
(never a divergence) and verifies the journaled digest **equals** the current flow's `Digest()`. A mismatch is
reported as a divergence, "ran against a different topology": the run executed under a different
declared graph than the flow now describes. So `Conform` covers node-visitation and branch choices,
and the digest + inclusion proof cover *which topology* the run committed to, verifiable offline.

The `examples/plan` demo prints this after a clean run
(`Cryptographic conformance: the run committed to the declared topology under the signed tree head`),
and `TestCryptographicConformance` there proves it in-process, including that a tampered `flow:digest`
record no longer verifies under the signed root.

## Declarative config

The builder authors a flow as typed Go. You can also author **the same flow as data**: a declarative
config that describes the *topology* (nodes, edges, switch arms) and references *behavior* by name.
A config places and wires nodes; it cannot write a `Step`'s body or a `Switch`'s predicate. Those stay
in Go and are referenced by name through a registry. A config loads into the same builder and produces
the same `*Flow`, so it inherits `RenderMermaid`, `Conform`, and the topology `Digest` unchanged.

### The registry and `Load`

Registration maps config names to typed Go blocks, capturing each block's I/O types via
`reflect.TypeFor`, so the config never restates types; they flow from the registered block.

<!-- docsnip: setup type Order struct{}; type Receipt struct{}; type Assessment struct{ Rush bool }; type Reservation struct{}; func classifyOrder(context.Context, Order) (Assessment, error); func reserveInventory(context.Context, Assessment) (Reservation, error); func finalizeReceipt(context.Context, Reservation) (Receipt, error); func declineReceipt(context.Context, Assessment) (Receipt, error) -->
```go
reg := plan.NewRegistry()
plan.RegisterStep(reg, "classify", classifyOrder)              // infers Order -> Assessment
plan.RegisterStep(reg, "reserve", reserveInventory)            // Assessment -> Reservation
plan.RegisterStep(reg, "finalize", finalizeReceipt)            // Reservation -> Receipt
plan.RegisterStep(reg, "decline", declineReceipt)              // Assessment -> Receipt
plan.RegisterPredicate(reg, "rush", func(a Assessment) bool { return a.Rush })
```

- **`NewRegistry()`** returns a fresh, explicit, per-`Load` registry. There is no global mutable
  default, and a duplicate registration is an error, never a silent overwrite.
- **`RegisterStep[I, O]`** infers `I`/`O` from a `func(context.Context, I) (O, error)`, the same shape
  `Step` takes; `RegisterJoin2`/`RegisterJoin3` take the merge shapes of `Join2`/`Join3`. **`RegisterTool[I, O]`** and
  **`RegisterModel[I, O]`** take them explicitly (an `agent.Tool` and a prompt carry no I/O types). A
  `Model` node needs a bound model, supplied to the loaded flow via `Load(..., WithLoadedModel(m))`.
  **`RegisterPredicate[M]`** captures the switched type `M`; **`RegisterJoin2`/`RegisterJoin3`** register
  a merge block for a `join`.

Loading supplies the boundary types at the Go call site (the caller knows them); the loader fills the
middle from data:

<!-- docsnip: setup type Order struct{}; type Receipt struct{}; configBytes []byte; reg *plan.Registry -->
```go
flow, err := plan.Load[Order, Receipt](configBytes, reg)   // *Flow[Order, Receipt], or a load error
```

`LoadReader[In, Out]` is the same reading from an `io.Reader` (an `*os.File` or an HTTP body).

### The config schema

A config is pure topology plus block references: a required `version` (`1`, the only version this
release reads, exported as `plan.ConfigVersion`), a top-level `flow` name, optional `in`/`out`
documentation, an explicit `entry` (else `nodes[0]`), the `nodes`, and an ordered `wiring` list. Each
wiring element is EITHER an edge `{"edge": [from, to]}` OR a switch
`{"switch": over, "when": [{"pred": p, "to": t}], "else": t}`. Every key is snake_case. Illustrative
YAML:

```yaml
version: 1
flow: order-triage
in: main.Order            # optional, cross-checked against Load's In
out: main.Receipt         # optional, cross-checked against Load's Out
entry: classify
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

The equivalent JSON (the loader format) is what `examples/plan/declarative.go` embeds and loads.

#### Fan-in: the `join` wiring element

A third wiring form fans several producers back into one node: `{"join": name, "inputs": [...],
"merge": mergeBlock}`. The `merge` names a merge block registered with `RegisterJoin2[A, B, O]` (or
`RegisterJoin3`), whose arity and input types `Load` checks against the join's declared `inputs`. As
with steps and predicates, only the *shape* is data; the merge *body* stays registered Go referenced
by name. A canonical fan-out-then-fan-in diamond as data:

```yaml
nodes:
  - {name: split, block: split}   # int -> int, fans out
  - {name: y,     block: y}        # int -> int
  - {name: z,     block: z}        # int -> string
wiring:
  - edge: [split, y]
  - edge: [split, z]
  - join: merge
    inputs: [y, z]
    merge: mergeBlock              # RegisterJoin2(reg, "mergeBlock", func(context.Context, int, string) (string, error))
    safety: readonly               # optional; see "Node and join safety" below
```

The `merge` block:

<!-- docsnip: setup reg *plan.Registry -->
```go
plan.RegisterJoin2(reg, "mergeBlock", func(_ context.Context, a int, s string) (string, error) {
    return fmt.Sprintf("%s+%d", s, a), nil
})
```

#### Bounded loops: the `loop_max` back-edge arm

A switch `when` arm may carry a `loop_max`: `{"pred": p, "to": head, "loop_max": n}`. This is a bounded
back-edge that routes to an *ancestor* of the switched node (the loop head) while `pred` holds, up to
`n` iterations, so the graph stays finite. The arm's `pred` is an ordinary registered predicate
referenced by name; `loop_max` is the only new field. A bounded countdown loop as data:

```yaml
nodes:
  - {name: seed,   block: seed}     # int -> LoopState (entry)
  - {name: refine, block: refine}   # LoopState -> LoopState (the loop head)
  - {name: check,  block: check}    # LoopState -> LoopState (the loop switch)
  - {name: done,   block: done}     # LoopState -> string (the exit terminal)
wiring:
  - edge: [seed, refine]
  - edge: [refine, check]
  - switch: check
    when: [{pred: again, to: refine, loop_max: 10}]  # loop back to refine while N>0
    else: done                                        # exit
```

The same load-time validation `Build` gives a hand-built loop applies: the back-edge target must be
an ancestor of the switch, the bound must be positive, and the routed type must equal the head's input
type.

#### Node and join safety

A node (or a join) may carry a `safety` classifying how `Run` treats it on the ambiguous-crash window
(its body ran, its result was lost to a crash): `"readonly"`, `"idempotent"`, or
`"side_effect"` (one spelling per level; the pre-v1 alias `"retryable"` is refused, naming `"idempotent"`). A read-only or idempotent node re-runs its body on resume rather
than halting, because its body is safe to repeat; a side effect halts.

**A config may only lower retry safety.** Whether a step is safe to run twice is a property of its Go
code, so only Go declares it: `RegisterStep`, `RegisterTool` (from the tool's own `Safety`),
`RegisterModel`, `RegisterJoin2` and `RegisterJoin3` take `ReadOnly()`/`Idempotent()` options. The
levels, highest first, are read-only, idempotent (`Idempotent` or an `IdempotencyKey`), and side
effect. A config `safety` may keep a block's level or name a lower one (mark a read-only block
`idempotent`, or any block `side_effect` so a crash with no recorded outcome halts for confirmation),
and a value above what Go declares (`readonly` or `idempotent` on a side effect, `readonly` on an
idempotent block) is a load error (`ErrConfig`) naming the node. `side_effect` also drops an
`IdempotencyKey`, since the key alone makes a node retry-safe. Any other change keeps an approval gate
or an `IdempotencyKey` the wrapped agent tool declares (so a gated tool is still refused, see
[Node approval](#node-approval)). The Go options `ReadOnly()` and `Idempotent()` on a
`Builder` node are Go code and may raise a node's level; they too keep an approval gate.

```yaml
nodes:
  - {name: read, block: read, safety: side_effect}   # halt on resume, even though Go declares it read-only
```

The join wiring element takes the same optional `safety` (shown in the diamond above), which may
lower what its merge block's `RegisterJoin2`/`RegisterJoin3` options declare.

<a id="node-approval"></a>
#### Node approval

A node may also carry an `approval` block declaring an m-of-n human gate, which loads onto the node's
`Safety.Approval` alongside any `safety` classification:

```yaml
nodes:
  - name: refund
    block: refund
    safety: idempotent
    approval: {need: 2, approvers: [ops, finance, risk]}
```

`Load` and `Validate` reject a block with no approvers, a `need` outside `1..len(approvers)`, or a
duplicate approver id, naming the node. The `plan` runtime does not enforce an approval gate yet, so a
well-formed block is refused too (`ErrConfig`, naming the node), as is a `Tool` node wrapping an agent
tool that requires approval: a gate that loaded but never stopped anything would let the node run
unapproved. Until the runtime enforces it, put the gate on an agent tool (see [approval](hitl-approval.md)).

### Load-time validation

Moving topology from Go to data trades compile-time type checking for load-time validation: a
miswired config does not fail at `go build`, it fails at `Load`, with a worded error that names the
offending nodes and types. The JSON is read as written: a name that is not a config field (a
misspelling such as `aproval`, or a case variant such as `Safety`), a name given twice, and data after
the value are parse errors, so a typo cannot silently drop a gate or an entry. A config that omits
`version`, or states a version other than `1`, is refused, since the keys a config may use depend on
its version. A key whose snake_case spelling is a key at that place (the pre-v1 `loopMax`, or
`Safety`) is refused with an error naming the key to write and where it is, for example
`$.wiring[2].when[0]: "loopMax" is not a config key; version 1 keys are snake_case, write "loop_max"`.
Every parse error wraps `ErrConfig`. `Load` then runs, by
`reflect.Type` identity:

- **Predicate typing:** every switch arm's registered predicate `M` equals the switched node's output
  type. This is a strict improvement over the Go builder, which only checks a predicate at its
  compile-time call site.
- **Edge typing:** every edge's `from.outType` equals `to.inType` exactly (nominal identity, not
  assignability, matching the Go builder's `Edge[M]`).
- **Boundary typing:** the entry consumes `In`, every terminal produces `Out`, and any present
  `in`/`out` documentation matches `In`/`Out`.
- **Join typing:** a `join`'s `merge` block must be registered, and its arity and input types must
  match the join's declared `inputs`; an unknown or mis-arity merge is a load error naming the join.
  A join is entered only by its inputs: an `edge` into it from any other node, a repeated input
  edge, or a `switch` arm routing to it is a load error, since the merge would drop that value.
- **Structural checks `Build` does not give:** a node cannot be both switched-over and have an
  outgoing edge, and a `wiring[]` element must set exactly one of `edge`/`switch`/`join`.

`Load` **reports all failures at once** (collect-all drift): it names every unresolved block or
predicate with a near-miss suggestion where one exists, and flags registered blocks the config never
uses. For CI, **`Validate(data, reg) error`** runs every check that does not need `In`/`Out`, so
config-vs-registry drift is catchable in a unit test rather than only at process start.

### Inherited for free, and conformable to its config

A config-loaded `*Flow` is an ordinary flow, so it inherits `Run` (sequential, at-most-once,
halt-on-ambiguity), `RenderMermaid` (the declared topology, now sourced from the config), `Conform`,
and `Digest` + the `flow:digest` record. Because the digest now commits to the config-derived
topology, a signed tree head over the run proves offline that the run followed **this config**, the
same way cryptographic conformance proves it followed the diagram. This is the headline: a
config-loaded flow is **cryptographically conformable to its config**. `examples/plan` demonstrates it
by asserting the config-loaded flow's `Digest()` **equals** the code-built flow's `Digest()`: the
config and the Go describe the same topology (the Go names its predicate with `.Named("rush")` and
the join's merge block with `plan.BlockName("mergeBlock")`, the names the config uses). The same holds for a config-built fan-in or bounded
loop: `examples/plan` loads a `join` diamond and a `loop_max` loop, runs and conforms each, and asserts
each config-loaded flow's `Digest()` equals its code-built counterpart, so a config-built join or loop
is cryptographically conformable to its config just like the linear case.

JSON is the loader (and tool-emit / interchange) format; the core loader stays stdlib-JSON and
dependency-free. YAML is a thin authoring front-end that decodes into the same config struct, not a
core dependency, so the two formats are just front-ends to one loader.

## Limits

- Fan-in is fixed-arity (`Join2`/`Join3`); unbounded or ragged fan-in is not supported.
- `Build` rejects wiring the runtime cannot execute as declared, naming the step: a step fed by more
  than one producer without a Join, more than one Switch over a step, a Switch and an Edge from the
  same step, a Switch inside a loop body other than the loop's own (so no nested loops), and an Edge
  that leaves a loop body or enters it past its head.
- `Model` decodes the response as JSON into `O` (no derived response schema yet), so `O` must be
  JSON-shaped and the prompt should instruct JSON output.
- Requires Go 1.27.

## A runnable example

`examples/plan` is a self-contained module: it builds an order-triage flow, prints the declared
diagram, runs it against a sqlite-backed store, conforms the run, and proves cryptographic conformance
(a signed tree head over the run plus an inclusion proof that the journaled topology digest equals
`flow.Digest()`). Its cross-process test crashes mid-run and resumes in a fresh process, proving the
side effect fires at most once and the run either completes or halts. Run the demo with
`cd examples/plan && GOWORK=off go run .`.
