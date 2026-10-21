# Plan flows (the `plan` package)

The `plan` package builds plan flows: a Go-embedded builder you author as typed handles,
compiled to the same journal-backed runtime as plain Go. It adds a way to *author*, never a way to
*execute*. Every node lowers to a memoized journal step (`Journal.Step`), so a flow inherits at-most-once side effects,
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

<!-- docsnip: setup ctx context.Context; journal *agent.Journal; runID string; order Order; type Order struct{}; type Receipt struct{}; type Assessment struct{ Rush bool }; type Reservation struct{}; func classifyOrder(context.Context, Order) (Assessment, error); func reserveInventory(context.Context, Assessment) (Reservation, error); func finalizeReceipt(context.Context, Reservation) (Receipt, error); func declineReceipt(context.Context, Assessment) (Receipt, error); returns error -->
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

out, err := flow.Run(ctx, journal, runID, order)           // Order in, Receipt out
```

`journal` is an `*agent.Journal` over any store (an in-memory store for tests; `store/sqlite` or
`store/postgres` for production). `runID` is the durable identity: re-running the same `runID` resumes from the journal.

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
    (an `Interrupt`, a pending approval) is `ErrConfig`, as for `Journal.Step`; put the pause in a
    `ReadOnly` step of its own.
    `Join2`/`Join3` merge bodies take the same leading `ctx`. `When` predicates and `Switch` routing
    take no ctx: they must be pure functions of the value, since resume replays the recorded arm.
  - `Tool[I, O](name, agent.Tool)` runs a tool; give `I`/`O` explicitly (they say how to JSON-encode
    the input and decode the result). The node runs the tool as an agent does: under its
    `ToolSpec.Timeout` (`agent.WithTimeout`), where a result that arrives after the deadline is
    returned and an error after it, while the flow's own context is live, has an unknown outcome
    (`agent.ErrToolOutcomeUnknown`: a side-effect node halts on its next drive, a retry-safe node
    runs again); and only once the call guard admits it, so a flow run under an
    `audit.AttenuatingSubAgent` delegation whose grant has expired fails the node without calling
    the tool. `Build` (and `RegisterTool`, reported again at `Load`) refuses a tool `agent.New`
    would refuse for how it wraps another.
  - `Model[I, O](name, prompt)` is a model turn: bind a model with `Builder.WithModel(m)` (or, in a
    declarative config, `Load`'s `WithLoadedModel`). The node renders `prompt` as a `text/template` over the
    typed input `I`, calls the bound model, and decodes the structured response into `O` (so `O` must
    be JSON-shaped). A model that honours a response format (`agent.ModelInfoOf` reports
    `ResponseFormat`: `model/openai`, `model/gemini`) is sent `O`'s JSON schema as the provider's
    structured output, so it answers with `O`'s JSON, each field of its schema type, with no prose
    or code fence. Any other model (`model/anthropic`, or one that does not describe itself) is sent
    no schema, so the prompt should still name the fields and their types. Build errors if a `Model` node has no
    bound model, naming it. A `Model` node is `ReadOnly` by default: it calls no tool and changes
    nothing outside the run, so a call whose outcome is unknown (a crash lost its result, or the call
    failed: the provider errored or the reply did not decode into `O`) is made again on the next drive
    instead of halting. A re-call costs tokens and may answer differently; nothing saw the lost answer.
    A config `safety: side_effect` on the node halts instead.
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
  the *actual-ran* path from the journal). A gated node's label carries its gate:
  `approval: 2 of 3 (ops, finance, risk), excluding the requester, times out after 24h`.
- **`Conform(ctx, store, runID)`** checks a run's journal against the declared topology and returns
  whether it followed the graph (`ok`), the divergences, and an error.

## What you get, and the semantics to know

Every node runs as a [`Journal.Step`](durable-steps.md) under its node key, so it has exactly a Step's
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
  `flow.ResolveHalt(ctx, store, halt.Ref(), agent.Outcome{Result: output})`, where `output` is the
  node's output value; the next `Run` continues past the node without running its body, feeding
  `output` downstream. `flow.ResolveHalt` is `agent.ResolveHalt` after three checks against the
  flow: the halt names a node of this flow, the run is a run of this flow (its recorded start names
  the flow and its recorded digest is the flow's), and `output` decodes as the node's output type (a
  resolution is final, so one the flow could not read would leave the run unable to continue). Like
  `agent.ResolveHalt`, it resolves only a node that halted: one with a live attempt marker
  (`agent.ErrNoLiveAttempt` otherwise).
- **A cancelled flow stops at its next node.** `agent.Cancel(ctx, j, runID, reason)` of a flow's run
  writes `run:cancelled`. A node reads it once it will run (after a side-effect node's claim is won,
  recording the attempt as not started), so no node starts after `Cancel` returns; a node already
  running finishes. The flow then returns `agent.ErrRunCancelled`, and so does every later `Run`.
  A flow that completed while `Cancel` landed reads its end markers back: the first in journal order
  is its end, as `agent.Status` reports it.
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
  A `Model` node is read-only unless a config lowers it.
  A declarative config's `safety` may only lower that (see "Node and join safety"). `Safety` is not
  part of the digest, so a node may be relabelled between a crash and its resume: the marker is the
  attempt's recorded safety, so a node attempted as a side effect halts even if it is retry-safe now,
  and a node attempted as retry-safe (no marker) runs again under a claim if it is a side effect now.
- **A run keeps its flow and its input.** `Run` checks the run ID as `agent.Run` does (an empty ID, or
  one in the form reserved for sub-agents and session turns, is `ErrConfig`), then records the run's
  start (`run:start`, see `agent.RunStart`): kind `agent.RunKindFlow`, the flow's name, and the JSON
  of the input. A later drive with a different input, under another flow's name, or of a run an
  `Agent` started is `ErrConfig` and records nothing (and an `Agent` refuses a flow's run the same
  way). Inputs are compared as canonical JSON (keys sorted, each number as the exact decimal value
  it denotes, so `1`, `1.0` and `1e0` match and `2^53` and `2^53+1` do not), so the recovery path of
  decoding `RecordedStart`'s `Input` without loss (into the flow's input type, or with
  `json.Decoder.UseNumber`) and passing it to `Run` resumes the run even where that round trip
  changes the JSON text. Decoding it into a `float64` can change a number, and then the input. The
  comparison is blind to whitespace, key order, a number's spelling (whatever the size of its
  exponent) and a string's escaping, and nothing else: an input whose JSON has a repeated object key,
  an escaped lone surrogate (`"\ud800"`) or invalid UTF-8, on which two different texts would read
  as one value, is refused with `ErrConfig` before anything is recorded. Like RFC 8785, the comparison
  reads numbers as values: `-0` is `0`, and `1.0` is `1`. A resumed drive whose input differs from
  the recorded one only that way is the same input, and its nodes see the input the run recorded
  (their recorded outputs are replayed); a flow whose input type is `json.RawMessage`, or that
  relies on the sign of zero, should not resume with such a respelling. The comparison takes time
  linear in the input's length, whatever its nesting or the size of its exponents. `Run` then records the
  flow's digest and, on resume, refuses (`ErrConfig`) to continue a run that started under a
  different digest: its journal only means what it meant under that flow.
- **A finished run is final.** When the terminal node finishes, `Run` records `run:complete` with the
  flow's name and its output, so `Recover` and `RecoverLoop` skip the run and `agent.IsComplete`
  reports it. A later drive with the run's input returns the recorded output with one point read,
  whatever the flow's topology is now.
- **Steps inside a node are scoped to it.** A `Journal.Step` (or `Journal.Parallel` task) a node's body
  runs for the same run ID is recorded under the node's key, `node:<name>:step:<step>` (and
  `node:iter:<n>:<name>:step:<step>` in a loop body), so each loop iteration runs its own Steps
  instead of replaying the first iteration's, and a Step name need be unique only within its node (and must not be empty: an empty name is `ErrConfig`, for every `Journal.Step`). A
  halt of such a Step names that key; resolve it with the halt's `Ref()`. Other pauses a body takes
  (`Interrupt`, `Await`) are not scoped: give them names unique per iteration.
- **Durable timers are not available in a node.** `agent.Sleep` and `agent.WaitUntil` need the run
  context an agent gives its tools, which a flow does not bind: called from a `Step` or `Tool` node
  they fail with `ErrConfig` ("agent: Sleep called outside a running agent"). A run that must sleep
  and wake needs an agent run today: see the [ambient agent recipe](../recipes/ambient-agent.md) and
  [durable timers](durable-steps.md#durable-timers-sleep--waituntil-and-the-waker).
- **A node can wait for human approval.** A node gated with `plan.Approval(policy)` (or by the
  agent tool it wraps, or a config `approval` block) pauses before it runs, exactly as a gated agent
  tool call does: see [Approval gates on nodes](#approval-gates-on-nodes).
- **Reserved keys.** `node:`, `switch:` and `flow:` are reserved prefixes, like `run:` and `attempt:`,
  so a `Journal.Step` a node's body runs cannot name one of the flow's records.
- **Conformance.** Because the flow is authored and the actual path is derived from the journal, a run
  can be proven to have followed the declared topology, at node-visitation granularity plus the
  journaled branch choice. `Conform` replays the run's routing from its recorded choices: a record of
  a node on an arm its `Switch` did not take, of a loop iteration the run did not reach, or of a node
  inside or outside a loop under the wrong kind of key is a divergence, and so is a journal that
  records nodes without `run:start` and `flow:digest`. A completed run must record every node,
  choice and loop iteration its routing reaches (`Run` records them all before its completion), and
  its `run:complete` must carry the recorded output of the last terminal node it reached. Values are
  compared under the one rule above, so an output recorded with or without HTML escapes (`\u003c` or
  `<`) is the same output. The blind spot: conformance sees *that* a node ran, not what its Go body did
  inside, so a node whose interior must be checked should be split into smaller nodes.

## Approval gates on nodes

A node can require human approval before it runs, with the same gate an agent's tool call uses
([Human approval](hitl-approval.md)): `agent.SingleApproval()` for one decision, or an m-of-n policy
of named approvers who sign their decisions. Gate a node with `plan.Approval`, and give the flow the
approvers' verifiers:

<!-- docsnip: setup approvers agent.ApproverVerifierFor; type Order struct{}; type Refund struct{}; type Receipt struct{}; func lookupRefund(context.Context, Order) (Refund, error); func issueRefund(context.Context, Refund) (Receipt, error) -->
```go
b := plan.New[Order, Receipt]("refunds").WithApproverVerifiers(approvers)
lookup := b.Step("lookup", lookupRefund, plan.ReadOnly())
refund := b.Step("refund", issueRefund,
	plan.Approval(&agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}}))
b.Edge(lookup, refund)
```

`Run` returns `*agent.ApprovalPending` before the node runs. Its `ToolUseID` is the node's key
(`node:refund`, or `node:iter:<n>:refund` in a loop body), its `ToolName` the node's name, and its
`Args` the node's input as JSON: what approvers sign. Record decisions as for a tool call, then run
again with the same input:

<!-- docsnip: setup ctx context.Context; j *agent.Journal; flow *plan.Flow[int, int]; runID string; in int; err error; sig []byte; decidedAt time.Time; returns error -->
```go
var pend *agent.ApprovalPending
if errors.As(err, &pend) {
	// each approver signs agent.ApprovalDecisionBytes(pend.Subject(), id, approved, decidedAt)
	if err := agent.SubmitDecision(ctx, j, agent.Decision{RunID: pend.RunID, ToolUseID: pend.ToolUseID,
		ApproverID: "ops", Approved: true, DecidedAt: decidedAt, Alg: audit.AlgEd25519, Signature: sig}); err != nil {
		return err
	}
}
out, err := flow.Run(ctx, j, runID, in) // runs the node once 2 of 3 have approved
```

- **The same records and rule.** The gate records the request (`approval-request:<node key>`, the
  node and its input) the first time it is reached, counts decisions with `agent.TallyApprovals`,
  and records its terminal tally (`approval-tally:<node key>`) before the node runs.
- **Decisions follow the request.** Node keys are predictable, so a decision counts only if it is
  recorded after the gate's request, and its signature must verify over the input the request
  records: an approver decides on the node's actual input. `agent.Approve` and
  `agent.SubmitDecision` refuse (`ErrConfig`) a decision on a node key whose gate has recorded no
  request yet; in a loop, each iteration's key gets its request when that iteration reaches the gate.
  (This applies to a flow's runs; an agent's tool call keeps its rules whatever its ID looks like.)
  A decision written into the journal before the request by other means never counts. For a
  `SingleApproval` gate it holds the decision's first name, so `Approve` records the real decision
  under the gate's second name, which the gate honours: the stale record does not block the gate.
  For an m-of-n gate the approver signs again at a new time (a new `DecidedAt`): that is a new
  decision, recorded after the request, and it counts; resubmitting the identical stale decision
  returns `ErrConfig` saying so. A
  `SingleApproval` gate is decided with `agent.Approve(ctx, j, pend.RunID, pend.ToolUseID, ok)`.
- **A denial stops the run.** A gate whose quorum can no longer be reached (or an `Approve(false)`)
  never runs the node: `Run` returns an error wrapping `plan.ErrApprovalDenied`, and so does every
  later drive. Nothing is compensated (flows have no saga); `agent.Cancel` ends the run.
- **A deadline stops the wait.** A gate with a timeout (`ApprovalPolicy.Timeout`, or `timeout:
  48h` in a config `approval` block) that has no approval by its deadline expires: `Run` returns
  `plan.ErrApprovalExpired` (which wraps `plan.ErrApprovalDenied`), the node never runs, and every
  later drive returns the same. The deadline is measured from the gate's recorded request and
  recorded once (`approval-deadline:<node key>`); in a loop each iteration has its own. See
  [Human approval](hitl-approval.md#deadlines).
- **A pause survives a crash.** Every fact the gate uses is in the journal; decisions recorded while
  no process runs the flow are counted by the next `Run`, and a gate that passed is never asked again.
- **In a loop, the gate is per iteration.** Each iteration's run of a gated node pauses for its own
  decisions on its own input. To approve a whole loop once, gate a node before it.
- **A gate on an arm not taken never pauses.**
- **Separation of duties.** A policy with `ExcludePrincipal` never counts the run's requester (see
  [Human approval](hitl-approval.md#separation-of-duties)). A flow run's requester is the
  `OnBehalfOf` of the identity it starts with (`agent.WithIdentity` passed to `Run`, or the one its context carries inside an agent run), which the run journals in its
  `run:principal` and `run:start` (a flow started inside another run's drive also records that
  run's principal chain, and runs its nodes start record the flow's); a later drive under another principal is `ErrConfig`, and one with none runs under the
  journaled one. A flow run recorded with no principal (every flow run an earlier version started)
  keeps none, whatever a later drive carries, and stops at such a gate with `ErrConfig`.

  <!-- docsnip: setup ctx context.Context; j *agent.Journal; flow *plan.Flow[int, int]; runID string; in int -->
  ```go
  ops := agent.WithIdentity(agent.Identity{OnBehalfOf: "ops"}) // from your auth layer
  out, err := flow.Run(ctx, j, runID, in, ops) // ops's own approval of a gated node never counts
  ```
- **The digest commits to the gate**, so a paused run cannot be resumed under a flow whose gate was
  removed or changed. A node carries one gate: `plan.Approval` on a `Tool` whose agent tool declares
  another gate is a `Build` error. `Build` refuses an m-of-n gate in a flow with no
  `WithApproverVerifiers` (`WithLoadedApproverVerifiers` for `Load`).
- **Evidence.** `audit.ApprovalEvidence(ctx, j, runID, "node:refund", sth)` and
  `audit.VerifyApprovals` (or `bide audit verify-approvals -call node:refund`) prove offline that the
  approvers signed this node's input before it ran. `bide view` shows the gate on the node.

- **Tell a person.** A paused gate, or a node that halts on an unknown outcome, stops the run until
  someone acts, and nothing tells them. `plan.New(...).WithNotifier(notifier, trail)` (for a loaded
  flow, `plan.WithLoadedNotifier`) tells a `agent.Notifier` once per event, at least once across
  crashes, with its delivery state on a `NotifyTrail` and not in the run's journal. It is a runtime
  binding, not part of the digest, and it takes the options of `agent.WithNotifier`. Call
  `flow.FlushNotifications(ctx)` before the process exits to let deliveries in flight finish; a flow
  that `agent.Recover` or `agent.RecoverLoop` drive is covered by the option given to them as well.
  See [extension points](../reference/extension-points.md#notifier-tell-a-person-a-run-needs-them).

`examples/approval/flow` runs the whole story from a config (its gate has `timeout: 48h`): the pause, two
approvals, one refund, and the offline check.

![bide view on a gated refund flow: the refund node approved 2 of 3, signatures verified](./img/flow-gate-approved-dark.png)

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
- **The topology itself, journaled beside it.** Just before `flow:digest`, a run's first drive records
  the declared `Topology` as JSON under `flow:topology` (`plan.TopologyStep`), one Insert, and a
  resume writes neither record again. `plan.TopologyDigest` of that `Topology` is the digest:
  `Flow.Digest` is computed through it, and the `Topology` carries what the digest commits to beyond
  the drawing (each node's block, each arm's predicate name, and every type with its package path in
  the `*_canonical` fields). `plan.JournaledTopology(records)` reads the record and checks its digest
  against `flow:digest`, so a reader (such as `bide view`) can draw the flow a run ran from its
  journal alone and know the drawing is what ran. The record is informational: `Run`, resume,
  routing and recovery do not read it. A run started before this record existed has none.

The flow, end to end:

1. `flow.Run(ctx, store, runID, in)` executes the flow. It journals `flow:digest` before any node.
2. `audit.NewTreeHead(ctx, store, runID, ts)` then `audit.SignTreeHead(th, signer)` (any `audit.Signer`) commit to the run's
   journal with a signed tree head (STH). Anchor the STH and its key in a separate trust domain; that
   is what makes it tamper-evident (see the `audit` package security model).
3. `audit.ProveRecord(ctx, store, runID, i, sth)` builds an RFC 6962 inclusion proof for the
   `flow:digest` record (index `i`), bundled with the STH.
4. An auditor holding only the bundle and the signer's public key (obtained out of band) checks
   `bundle.Verify(v)` with a verifier for that key, which returns nil only if it holds (the STH signature is authentic and the record is included under the signed
   root), checks the STH's timestamp with `audit.CheckTimestamp`, and that the proven digest equals the declared flow's `flow.Digest()`. Together: **the run
   committed to this signed diagram.**

`Conform` closes the loop on the *path*: it recognizes `run:start` (which must name this flow),
`run:principal` (which must name the principal and ancestors `run:start` does), a gate's tallies
and re-tallies (`approval-tally:<key>`, `:v3`, `:sod`, and a certificate-bound gate's `:ident` and `:ident-reask:<n>`), its identity records, exec holds and notes (`approval-identity:`, `approval-exec-hold:`, `approval-pause:`), `flow:digest`, `flow:topology` (whose digest must be this flow's), a node's attempt markers and the not-started records of its claims as internal records
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
reg.RegisterStep("classify", classifyOrder)              // infers Order -> Assessment
reg.RegisterStep("reserve", reserveInventory)            // Assessment -> Reservation
reg.RegisterStep("finalize", finalizeReceipt)            // Reservation -> Receipt
reg.RegisterStep("decline", declineReceipt)              // Assessment -> Receipt
reg.RegisterPredicate("rush", func(a Assessment) bool { return a.Rush })
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

`LoadReader[In, Out]` is the same reading from an `io.Reader` (an `*os.File` or an HTTP body). It reads at most 16 MiB and refuses a larger config with `agent.ErrConfig`.

### The config schema

A config is pure topology plus block references: a required `version` (`1`, the only version this
release reads, exported as `plan.ConfigVersion`), a top-level `flow` name, optional `in`/`out`
documentation, an explicit `entry` (else `nodes[0]`), the `nodes`, and an ordered `wiring` list. Each
wiring element is EITHER an edge `{"edge": [from, to]}` OR a switch
`{"switch": over, "when": [{"pred": p, "to": t}], "else": t}`. An optional `assert` list states
policy about the graph, which `Load` ignores (see [Policy assertions](#policy-assertions)). Every key is snake_case. Illustrative
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
reg.RegisterJoin2("mergeBlock", func(_ context.Context, a int, s string) (string, error) {
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
code, so only Go declares it: `RegisterStep`, `RegisterTool` (from the `Safety` in the tool's spec),
`RegisterModel`, `RegisterJoin2` and `RegisterJoin3` take `ReadOnly()`/`Idempotent()` options (a
`Model` block is read-only unless an option says otherwise; a `Step`, a `Join` and a `Tool` without
a declared `Safety` are side effects). The
levels, highest first, are read-only, idempotent (`Idempotent`), and side
effect. A config `safety` may keep a block's level or name a lower one (mark a read-only block
`idempotent`, or any block `side_effect` so a crash with no recorded outcome halts for confirmation),
and a value above what Go declares (`readonly` or `idempotent` on a side effect, `readonly` on an
idempotent block) is a load error (`ErrConfig`) naming the node. A config `safety` changes only the
retry level: the approval gate the wrapped agent tool declares (its `ToolSpec.Approval`) is not
part of `Safety` and is kept (see [Node approval](#node-approval)). The Go options `ReadOnly()` and `Idempotent()` on a
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
approval gate (an `agent.ApprovalPolicy`, as `agent.WithApproval` sets on a tool) alongside any
`safety` classification:

```yaml
nodes:
  - name: refund
    block: refund
    safety: idempotent
    approval: {need: 2, approvers: [ops, finance, risk]}
```

Add `exclude_principal: true` to the block for [separation of
duties](hitl-approval.md#separation-of-duties): the gate then never counts the run's requester
(`approval: {need: 2, approvers: [ops, finance, risk], exclude_principal: true}`). It is part of the
gate, so the flow's digest commits to it, and `bide view -config -diff` reports dropping it as a
weakened gate.

`bide view -config` and `RenderMermaid` show both settings on the gate (`excluding the requester`,
`times out after 24h`), and `bide check -diff` compares them: dropping `exclude_principal`, or
removing or lengthening `timeout`, is a weakened gate (a run can wait longer, and a decision after
the old deadline counts), and adding the exclusion or adding or shortening a timeout is a
strengthened one. It also reports a weakening that is not a gate's own change: a node
that every path reached through a gate before and some path now reaches without one, whatever its
safety (as a new arm around a gated node causes), a new effect no gate guards, and a lowered `safety`
(the label is the new config's own claim); a Switch condition changed beside an ungated effect (the
predicates are not read, so it asks for a review); a block changed on an ungated effect node when
checked without the Registry (the block's own gate is not visible); and an `assert` line removed or
loosened. A node renamed with the same block, kind and edges is `renamed`, not a removal and an addition; a gate that changed with it is reported on it. See [Compare two versions of a
flow](run-viewer.md#compare-two-versions-of-a-flow).

On a terminal, with the weakened gate first:

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="./img/cli-diff-weakened-dark.png">
  <img src="./img/cli-diff-weakened-light.png" alt="bide check -diff -check on a terminal: approval policy weakened called out first in bold red, then the changes with added in green, removed in red and changed in amber">
</picture>

`Load` and `Validate` reject a block with no approvers, a `need` outside `1..len(approvers)`, or a
duplicate approver id (or two that differ only by case), or an id that is not printable ASCII, naming the node.
The gate is enforced as in Go (see [Approval gates on nodes](#approval-gates-on-nodes)): load the flow
with `plan.WithLoadedApproverVerifiers(approvers)`, or `Load` refuses an m-of-n block (`ErrConfig`,
naming the node). A join wiring element takes the same `approval` block. A block may restate the gate
its registered block declares (a tool's own `ToolSpec.Approval`, or `plan.Approval` given at
registration) but not replace it: a config cannot loosen a tool's gate. A block that declares a gate and a config that sets another is one problem, worded with its fix (keep one, by removing `approval` from the config or the gate from the block).

<a id="policy-assertions"></a>
#### Policy assertions: the `assert` list

A config may state policy about its own graph in an `assert` list, one assertion per line. `Load`
ignores the list (it is not part of the flow or its digest); `bide check`, `bide view -config` and
`plan.InspectConfig` decide each line and report a violated one as an error, with a shortest
counterexample path:

```yaml
assert:
  - every path to charge passes approval >= 2 of 3
  - no side effect after charge
  - every path to a terminal passes validate
  - credit reachable
```

| Assertion | Holds when |
|---|---|
| `every path to <node> passes approval >= K [of N] [excluding the requester]` | every path from the entry to the node passes a node whose approval gate needs at least K decisions (and, with `of N`, from at most N named approvers; and, with `excluding the requester`, never counts the run's requester: `exclude_principal`). The node's own gate counts. A one-decision gate (`agent.SingleApproval`) counts only for `approval >= 1`. |
| `every path to <node> passes <node>` | every path from the entry to the first node passes the second. |
| `every path to a terminal passes <node>` | every path from the entry to a terminal node (one with no outgoing edge or arm) passes the node. |
| `no side effect after <node>` | no node a path from the node reaches is a side effect (a node that is in a loop with itself counts as after itself). |
| `<node> reachable` | a path from the entry reaches the node. |
| `<node> unreachable` | no path from the entry reaches the node. |

![A violated assertion on bide view -config: the shortest counterexample path, drawn in red, reaches charge without passing a 2-of-3 approval gate](img/run-viewer-config-assert-dark.png)

A node is named bare (no spaces) or in double quotes, and the words are separated by spaces. A quoted name is exactly what is between the quotes: there are no escapes, so a backslash is a backslash, and a name that contains a double quote cannot be written. Before this was so, a quoted name was unescaped as a Go string; `bide check` warns on a quoted name with a backslash sequence that the old reading would have changed (write the character itself). Every
assertion is decided by reachability over the declared graph (every edge, a join's inputs, and every
Switch arm, loop-back arms included), never by listing paths, so a loop is visited once. The graph is
what the config declares: a Switch arm counts as taken whatever its predicate.

Gates and safety come from the same place the check does. With your Registry (`plan/planview`), a
gate or a `Safety` a block declares in Go counts. From the command line, only the gates and `safety`
values the config states count, so a gate declared only in Go reads as missing (the assertion fails,
it never passes wrongly), and `no side effect after X` with a node of unknown safety after X (and no
known side effect) is undecided, which is also an error. An assertion outside the grammar, or one
that names a node the config does not declare, is an error too.

An `every path to X passes ...` or `no side effect after X` assertion holds vacuously when the entry reaches no X (or, for
`every path to a terminal passes X`, no terminal: a flow whose every node has an outgoing edge, a
cycle, has none). The check still reports it as holding, with `vacuous` set on the result and a
message saying so; `bide view -config` shows `holds vacuously`, and `bide check -diff` reports a line
that held and now holds only vacuously as weakened.

#### Editor support: a JSON Schema for flow configs

A JSON Schema (Draft 2020-12) describes the config, so an editor can complete keys, show each key's
description on hover and flag a misspelled key, a wrong `safety` value, an element that is not exactly one
of edge, switch or join, and an `assert` line outside the grammar as you type. It lives at
[`plan/schema/flow.schema.json`](https://github.com/bide-ai/bide/blob/main/plan/schema/flow.schema.json)
and its `$id` is the raw URL
`https://raw.githubusercontent.com/bide-ai/bide/main/plan/schema/flow.schema.json`. Replace `main` with a
release tag to pin the schema to a version.

The schema checks the shape of the file. Whether a block, predicate or node name exists, the types, and
whether an assertion holds are still decided by `Load`, `bide check` and `bide view -config`.

**YAML (VS Code with the Red Hat YAML extension, and other editors that run `yaml-language-server`).** Put
this comment on the first line of the config:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/bide-ai/bide/main/plan/schema/flow.schema.json
version: 1
```

To cover every config in a project without touching each file, map the schema to file patterns in
`.vscode/settings.json`:

```json
{
  "yaml.schemas": {
    "https://raw.githubusercontent.com/bide-ai/bide/main/plan/schema/flow.schema.json": [
      "flow.yaml",
      "flows/*.yaml"
    ]
  }
}
```

A JSON config cannot carry a `"$schema"` key (it is not a config key, and `Load` refuses it), so map JSON
files with the `json.schemas` setting (the same URL and `fileMatch` patterns) instead.

**JetBrains IDEs** do not read the comment. Open Settings, Languages & Frameworks, Schemas and DTDs, JSON
Schema Mappings, add the schema by URL (or the local `flow.schema.json`) and map it to your config files.

A YAML plain scalar cannot start with a double quote, so an assertion that quotes a node name with a space
in it is written as a single-quoted string: `'"send receipt" reachable'`.

The schema is kept in step with the code by a test (`plan/schema_test.go`): every key `Load` decodes is in
the schema and no other is, its enums and assertion patterns are the code's own, and every config in
`examples/plan/flows` is valid under it.

### Load-time validation

To see a config's graph and its problems before loading it, run `bide view -config flow.yaml`
(`-check` for CI), or embed the same page with your Registry through package `plan/planview`; see
the [run viewer guide](run-viewer.md#check-a-flow-config), or `bide edit flow.yaml` for the same check
with an outline and an inspector (the [flow editor guide](flow-editor.md)). All use `plan.InspectConfig`, which reports
the checks below as problems attached to the node, edge or arm they name.

Moving topology from Go to data trades compile-time type checking for load-time validation: a
miswired config does not fail at `go build`, it fails at `Load`, with a worded error that names the
offending nodes and types. The JSON is read as written: a name that is not a config field (a
misspelling such as `aproval`, or a case variant such as `Safety`), a name given twice, and data after
the value are parse errors, so a typo cannot silently drop a gate or an entry. A config that omits
`version`, or states a version other than `1`, is refused, since the keys a config may use depend on
its version. A key whose snake_case spelling is a key at that place (the pre-v1 `loopMax`, or
`Safety`) is refused with an error naming the key to write and where it is, for example
`$.wiring[2].when[0]: "loopMax" is not a config key; version 1 keys are snake_case, write "loop_max"`.
A name that is not printable is refused too: every identifier a reviewer reads (the flow, `entry`, `in`
and `out`, node names and blocks, the references in edges, switches, arms and `else`, join names,
inputs and merges, and the `assert` lines) must print. It holds no control character, no
invisible or direction-changing character (a zero-width space, a zero-width joiner or non-joiner, a
left-to-right or right-to-left mark, U+202E), no space other than a plain one, no private-use or
unassigned code point, no character that a renderer may draw as nothing (every character with the Unicode property
Default_Ignorable_Code_Point, which includes the Hangul and Khmer fillers, the grapheme joiner, the
Mongolian selectors and every variation selector, and the Braille blank), no combining mark with no
character before it to combine with, and no space at either end (the words of an `assert` line may be separated
by any whitespace, as the grammar reads them). Such a name can read as another in a diff or a review;
the error names the field and writes the character out, for example `nodes[2].name "ch\u{202E}gra"
holds a character that does not print (U+202E)`. **This also refuses some legitimate text, on purpose:** a Persian
or Urdu word spelled with a zero-width non-joiner, an Indic conjunct spelled with a zero-width joiner,
an emoji sequence joined by one (a family, a rainbow flag), an emoji with a variation selector (a
red heart as `U+2764 U+FE0F`, a check mark, a keycap such as 1 with `U+FE0F U+20E3`; the bare symbol is a valid name), and a left-to-right or right-to-left mark in a name that mixes scripts
and digits. The reason is that two names differing only by an invisible character read the same and
one can impersonate the other. The way out is to transliterate the name, or spell it without the
joiner, selector or mark. The Default_Ignorable set is taken from the Unicode tables of the Go
toolchain that built the program (Unicode 17.0.0 for Go 1.27); a rule that allows a joiner only where
a script needs it, as IDNA does, may come later. Approver ids are held to
printable ASCII by the gate itself. `bide check`, `bide check -diff`, `bide view` and every page and
error message the viewer serves write any such character that reaches their output (a file name, a
journal's topology, a step or predicate name) as `\u{XXXX}`; the JSON endpoints and the other commands
(`bide edit`'s own messages, `bide init`) are not filtered. Every parse error wraps `ErrConfig`. `Load` then runs, by
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
  A join's inputs are registered in wiring order, so an input that is itself a join must be defined
  earlier in `wiring[]` (which also rules out a cycle of joins). `bide check` and `bide view -config` report
  this without a Registry, and so a reference to a join (an edge out of it, a switch over it) before its element,
  an edge, arm or `else` into a join, a node that is switched over and has an edge out, and two switches over
  one node.
- **Loops:** an arm with `loop_max` above 0 is a bounded back-edge to an earlier loop head. The head must
  reach the switch in the forward graph (the edges, a join's inputs and the arms that do not loop back),
  a switch has at most one loop-back arm, and it needs an exit (another arm or an `else`). The loop's body is a
  simple region: no inner switch or nested loop, no edge that leaves it or enters it past its head, and no
  node between the head and the switch that is not on the loop path. `bide check` and `bide view -config`
  report all of these without a Registry.
- **The rest of the wiring:** a cycle that is not a loop, a node not reached from the entry (an edge or an
  arm must lead to it: a node you have declared and not wired in yet is reported this way), a node fed by
  several producers (combine them with a join), and a join's inputs that sit on mutually exclusive arms of
  a switch or that a switch can skip. `bide check` and `bide view -config` report these without a Registry,
  so what only `plan.Load` with your Registry can still refuse is what needs the Go side: a block, predicate
  or merge name that is not registered, the types, a merge block's arity and a node's own gate.
- **Structural checks `Build` does not give:** a node cannot be both switched-over and have an
  outgoing edge, and a `wiring[]` element must set exactly one of `edge`/`switch`/`join`.

`Load` **reports all failures at once** (collect-all drift): it names every unresolved block or
predicate with a near-miss suggestion where one exists, and flags registered blocks the config never
uses. It lists every type problem (every edge, arm, join input and terminal whose types do not fit), not the first, and `InspectConfig` reports up to 100 of them with a last line saying how many more there are; the errors of `Load` and `Validate` for these, and for the checks `Build` makes, wrap `agent.ErrConfig`. For CI, **`Validate(data, reg) error`** runs every check that does not need `In`/`Out`, so
config-vs-registry drift is catchable in a unit test rather than only at process start.

### Findings: the structure behind a problem

`ConfigReport.Problems` is for people: a message, and the node, edge or arm it belongs to. A tool that
needs to tell problems apart (an editor deciding whether an edit added one, a CI step that counts
type misfits) reads `ConfigReport.Findings`, one `plan.Finding` for each error in `Problems`:

```go
r := plan.InspectConfig(data, reg)
for _, f := range r.Findings {
    switch f.Kind {
    case plan.ProblemEdgeTypes:
        // f.Names = [from, to]; f.Got is the type the producer gives, f.Want the one the consumer takes
    case plan.ProblemOther:
        // an error with no kind of its own: f.Message only
    }
}
```

A finding has a `Kind`, the `Names` it is about (in full, in the order the kind documents, with no
position), `Got` and `Want` where the kind has them, and the `Message` of its problem. Two problems are
the same problem when their kind, names, `Got` and `Want` are the same, not when their messages are: a
message shortens a long name and names a wiring element by its position, so it changes when you move
an element or rename a node.

| Kind | Names | Got, Want |
|---|---|---|
| `edge_types` | from, to | type the producer gives, type the consumer takes |
| `arm_types` | switched node, predicate, target | type the predicate tests, type the node gives |
| `join_input_types` | join, input | type the input gives, type the merge takes there |
| `terminal_types` | step | the step's type, the flow's output type |
| `safety_above_registration` | node | safety the config states, safety the block registers |
| `loop_no_exit_arm`, `loop_more_than_one_back_arm` | switch | |
| `loop_head_not_ancestor` | switch, head | |
| `loop_body_not_simple` | switch, node on the body that is not on the loop path | |
| `loop_inner_switch` | node with its own switch, loop switch | |
| `loop_edge_leaves` | from, to, loop switch | |
| `loop_edge_enters` | from, to, loop switch, head | |
| `other` | | |

Findings are in the Go API only: `bide check` prints messages and the pages `bide view -config` and
`bide edit` serve do not carry findings in their JSON; a Go host that marshals a `ConfigReport` gets them.

Findings are found from the config and the registered types alone, so they are there beside any other
error (a misspelt block does not hide a type misfit), and they are complete: where `Problems` lists the
first hundred type problems and then "and N more", `Findings` has them all. Warnings have none. A
config that does not parse has one `other` finding, its parse error.

Stability, while plan is 0.x: a kind's name, the order of its names and the format of `Got` and `Want`
do not change; kinds are added (so ignore a kind you do not know); and `other` is a promise only that
the problem is an error: any `other` may become a specific kind in any release, so treat `other` and an
unknown kind alike. The messages behind `other` are listed in `plan/testdata/other_findings.txt`, which
a test keeps current. `Message` is for people and may change.

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

A YAML config is one document. `bide check`, `bide view -config` and a `bide init` project refuse a file that holds a
second one (a `---` followed by more), as the JSON loader refuses data after the value; a leading `---` and a closing `...`
are one document. Before this was checked, the second document was read by nothing and went unnoticed.

### Describing a Registry

`Registry.Describe` returns a `plan.RegistryDescription`: what the program registered, as plain data. It lists each
block with its kind (`step`, `tool` or `model`), input and output type names, the safety it declares and the approval
gate it declares in Go; each predicate with the type it tests; each merge block with its ordered input types; and any
registration errors `Load` would report. Type names are spelled as the topology digest spells them, with the package
path. It holds no functions, prompts or schemas, it is sorted by name, and the same registrations give the same JSON,
so a program can write it to a file for a tool to read. It is experimental: it may change in any 0.x release. The
flow editor's typed mode is built on it.

`plan.InspectConfigDescribed(data, description)` checks a config against such a description, with the same code as
`InspectConfig`, so a tool that has only the file (the flow editor's `-blocks` form) checks blocks, types, safety,
gates and the digest without the program. `RegistryDescription.Validate` says why a description read from a file is
refused. Two limits: types are told apart by name, so two distinct types spelled the same are one type (`Load`,
comparing Go types, would refuse an edge between them, and the digest already cannot tell them apart), and a type is
written with its package path in messages. A description is only as good as the program that wrote it; the check that
holds is the one made with the real Registry. A registered block the config does not use is an error here, as in
`Load`; a tool that lists blocks a config could use (the flow editor's palette) passes `plan.AllowUnusedBlocks()` to
`InspectConfig` or `InspectConfigDescribed` so the blocks it has not placed yet do not hide the report of those it has. A process keeps one type for each distinct type name it has ever been
given, so it admits at most 20000 names and 4 MiB of names in all (about 25 MB), and one description at most a quarter of that; past that the check reports an error
and a long-lived host must restart to check new type names.

## Limits

- Fan-in is fixed-arity (`Join2`/`Join3`); unbounded or ragged fan-in is not supported.
- `Build` rejects wiring the runtime cannot execute as declared, naming the step: a step fed by more
  than one producer without a Join, more than one Switch over a step, a Switch and an Edge from the
  same step, a Switch inside a loop body other than the loop's own (so no nested loops), and an Edge
  that leaves a loop body or enters it past its head.
- `Model` decodes the response as JSON into `O`, so `O` must be JSON-shaped. Only a model that
  honours a response format is sent `O`'s schema (structured output); for any other model the
  prompt should instruct JSON output, naming each field's type.
- Requires Go 1.27.

## A runnable example

`examples/plan` is a self-contained module: it builds an order-triage flow, prints the declared
diagram, runs it against a sqlite-backed store, conforms the run, and proves cryptographic conformance
(a signed tree head over the run plus an inclusion proof that the journaled topology digest equals
`flow.Digest()`). Its cross-process test crashes mid-run and resumes in a fresh process, proving the
side effect fires at most once and the run either completes or halts. Run the demo with
`cd examples/plan && GOWORK=off go run .`.

## Stopping a flow

`Builder.WithStopSwitch` (and `plan.WithLoadedStopSwitch`) gives a flow the process's kill switch, so a stop that covers the flow by name, a stop on the agent that acts, or a global stop holds a run as a pause (`*agent.Stopped`) at its start and each node before it begins. See the [kill switch guide](kill-switch.md).
