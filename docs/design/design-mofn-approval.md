# Design: m-of-n human approval

Status: shipped (not yet released). Composes existing seams (durable approval + the quorum tally
semantics) into a k-of-n human gate. No new persistence model and no new executor. The user-facing
guide is [docs/guides/hitl-approval.md](../guides/hitl-approval.md).

Where the implementation refined this design:

- **Verifier resolution.** `agent` cannot import `audit` (audit imports agent), so the gate verifies
  through a local `ApproverVerifier` interface that the audit verifiers implement, resolved
  by id with `Agent.WithApproverVerifiers` (a chaining setter, matching the Agent's other options).
- **One seat per key (F5).** Distinct approver ids are not distinct people if their verifiers
  resolve to one key: the TLA+ approvals model found one key holder meeting a 2-of-2 quorum alone
  (`spec/tla`, finding F5). `ApproverVerifier` gained `KeyIDs() []string`, identities derived from
  the public key's bytes (a list, so a rotation window and each hybrid component are covered), and
  the gate refuses a policy two of whose approvers share one with `ErrConfig`, on every evaluation
  (a terminal tally already journaled is reused, not recounted).
  `TallyApprovals` also never counts a shared seat, whatever resolver it is given.
- **Tally timing.** `approval-tally:<toolUseID>` is journaled only at a terminal outcome (proceed or
  deny), before the tool runs, never on a pause, because `Durable.Do` is at-most-once by name:
  journaling a pending tally would freeze it. A resume reads the terminal record instead of recounting.
- **Path isolation.** An `ApproveAs` decision never satisfies a 1-of-1 gate, and an `Approve`
  decision never counts toward an m-of-n tally.
- **Approval split from Safety.** The policy no longer lives on `Safety`: it is the tool's
  `ToolSpec.Approval`, set with `agent.WithApproval` (`agent.SingleApproval()` is the 1-of-1 gate
  that `RequiresApproval` was; any other policy is m-of-n), and `Safety` only says how a call may be
  retried. The API block below is the design as first shipped.
- **Declarative config and plan flows.** The `approval` block loads onto the node's approval gate,
  and a plan flow enforces it with the same records and counting rule as an agent's tool call (see
  [Gates on plan flow nodes](#gates-on-plan-flow-nodes) below).
- **v2 decisions and evidence** (below). The first implementation followed this document's "first
  decision per approver wins" literally, and an end-to-end test found that it let a bad record lock
  an approver out. v2 closes that and three related gaps.

## v2: decisions and evidence

### Threat model

The approval claim must hold against an operator who controls the journal and the evidence file
but holds no approver's private key, and against innocent mistakes (an approver signing with the
wrong key, a UI showing stale arguments). The first implementation had four gaps under that model:

1. **Lockout.** Decisions were keyed one per approver, first write wins. A forged, mistaken, or
   directly written record took the approver's only slot; their real decision was silently dropped,
   and since the bad record was not a valid denial, enough of them could hang the gate forever.
2. **Unbound approvals.** Approvers signed (run, call id, approver, decision), not what the call did.
   Evidence could not show that an approval was for "$120 to order 42" rather than whatever the
   operator said the call was.
3. **Omittable evidence.** Evidence disclosed one decision per listed approver. An evidence producer
   could drop an approver's earlier valid denial and show a later approval, undetectably.
4. **Unrecorded policy.** Nothing recorded the policy the gate enforced, so a gate misconfigured as
   1 of 3 produced evidence indistinguishable from a 2 of 3 gate's.

### Design

- **One record per decision, first valid decision counts.** Decision records are named
  `approval:<call>:<approver>:<sha256(approved || signature)>`, so an approver's records never
  collide and an identical resubmission is a no-op. The counting rule takes each approver's first
  record that is eligible and whose signature verifies; invalid records never occupy a place and
  never count toward "unreachable". (Closes 1.)
- **The signature binds the call.** `ApprovalDecisionBytes` v2 (`bide.approval.v2`) adds the tool
  name and SHA-256 of the canonical arguments. Canonicalization (sorted keys, no insignificant
  whitespace, no HTML escaping, number literals verbatim) is required, not cosmetic: the same call's
  arguments arrive as live-stream bytes, then as journaled bytes that encoding/json has compacted and
  HTML-escaped, then re-indented inside an evidence file. (Closes 2.)
- **The tally records what the gate enforced and read.** The terminal tally carries `Need`,
  `Approvers`, `ApprovedBy`, `DeniedBy`, and `Records`, the name of every decision record the gate
  read. It is journaled before the tool runs, so it is fixed before the action and covered by the
  same append-only log. (Enables 3 and 4.)
- **Complete evidence and a verifier that uses it.** `ApprovalEvidence` discloses the request (the
  model turn carrying the call's name and arguments), every record in the tally's `Records`, the
  tally, and the result. `VerifyApprovals` recounts with the same pure function the gate runs
  (`agent.TallyApprovals`), against the proven call, and reports a problem for any omitted record, a
  recount that disagrees with the tally, a policy other than the auditor's, or records out of order.
  (Closes 3 and 4.)
- **Feedback at submission.** `ApproveAs(..., WithDecisionCheck(resolver))` verifies against the
  recorded call before writing and returns `ErrInvalidApproval` or `ErrAlreadyDecided`. It is for
  the approver's benefit only; the gate never depends on it.
- **A non-Go auditor path.** `bide-audit verify-approvals` runs `VerifyApprovals` from the evidence
  file, a key file, and the expected policy on the command line.

### What remains trusted, stated precisely

- **The runtime at decision time.** A compromised runtime could record a false tally, but it cannot
  forge an approver's signature, and the recount against the disclosed signed decisions would
  disagree with a tally that overstates approvals. What it could do is ignore a valid decision it
  never wrote to the journal; that is the limit of any system whose log is written by the party
  being audited, and anchoring the tree head externally bounds when such an omission could occur.
- **Key history.** The tally does not record which key verified each approver. An auditor needs the
  keys that were valid at decision time.
- **Byte-exact evidence.** Proofs commit to the exact recorded bytes; a tool that re-orders JSON keys
  inside a recorded message breaks that record's proof. The approval signature itself is robust to
  reformatting (canonical arguments); the Merkle leaf encoding is not.

## Gates on plan flow nodes

A plan flow node may carry a gate: `plan.Approval(policy)` in Go, a wrapped agent tool's own
`ToolSpec.Approval`, or a config `approval: {need, approvers}` block. There is one implementation of
the gate: the flow reaches the agent package's gate through `internal/journalhook.Gate`, which runs
the same policy checks (`Validate`, one seat per key), the same `Approve` and `SubmitDecision`
records, `TallyApprovals`, and the same terminal tally as a gated tool call.

**The subject.** A gated node is "a call" whose `ToolUseID` is the node's key (`node:<name>`, or
`node:iter:<n>:<name>` in a loop body), whose `ToolName` is the node's name, and whose `Args` are the
node's input as JSON (a join's is the array of its ordered inputs). Approvers sign
`ApprovalDecisionBytes` over exactly that, so a signature approves this node, in this run and
iteration, with this input.

**Records, in journal order, all keyed by run and node key.**

1. `approval-request:<node key>`: the subject (`agent.ApprovalRequest`), recorded once, the first
   time a drive reaches the gate with no outcome recorded. It plays the part the model turn plays for
   a tool call: `FindToolCall` returns it (only for a plan node key), so `WithDecisionCheck`, the
   evidence and the recount all read the subject from the journal. A later drive whose node or input
   differs from the recorded request is `ErrConfig`; the digest check already refuses a resume under
   another flow, so this guards the input encoding.
   **Input binding.** Unlike a model's tool-use ID, a node key can be known before the run reaches
   the gate. The gate therefore counts only decisions recorded after the request (by journal
   order: an m-of-n gate counts the records after it, and a `SingleApproval` gate honors
   `Approve`'s record only if it follows it), and each counted signature verifies over the
   request's input. `Approve` and `SubmitDecision` refuse a plan node key with no request yet
   (`ErrConfig`), in a flow's run only: the run's recorded start decides, not the ID's shape, so an
   agent's model-chosen tool-use ID that looks like `node:x` keeps the agent's rules. A decision
   written into the journal before the request by other means is never counted, and keeps its
   name: decision names are first-writer-wins and ed25519 signatures are deterministic, so the
   approver's identical decision afterwards is not recorded, and `Approve` and `SubmitDecision`
   report that (`ErrConfig`) instead of returning nil. For a `SingleApproval` gate the name is the
   gate's one decision, so the gate cannot pass (it fails closed) and the run is cancelled; for an
   m-of-n gate that approver's seat is lost for the gate, which passes with the others if `need` is
   still reachable and is otherwise cancelled. `flows/limits/gate-early-stuck` states this limit. Found by the adversarial review of #195;
   `flows/regress/gate-request-order` keeps it from coming back.
2. `approval-pending:<node key>`: `ApprovalWait`, written on a pause that finds it missing (so a
   crash between the request and the note does not hide the wait). Informational, as for an agent.
3. The decisions: `approval:<node key>` (`Approve`, for `SingleApproval`), or one
   `approval:<node key>:<approver>:<digest>` record per `SubmitDecision`.
4. `approval-tally:<node key>`: the terminal tally, recorded before the node runs, never on a pause.
5. The node's own Step records (`attempt:step:node:...`, `node:...`).

**Evaluation.** Before the node's Step, Run evaluates the gate: the policy and its seats are checked,
then a recorded outcome decides (one point read: the tally, or `Approve`'s record); with none, the
request is recorded and an m-of-n gate counts the journal. A passing gate lets the Step run; a gate
with no outcome returns `*agent.ApprovalPending` (its `RootRunID` is the flow's run ID); a denying
gate returns an error wrapping `plan.ErrApprovalDenied` and the node never runs. Replaying a node
whose gate passed costs one `Get` (the tally) beside the Step's own read.

**Crash and resume.** Every fact the gate uses is in the journal, so a pause survives a crash: no
process holds it. Decisions recorded while no driver runs are counted by the next drive. A crash
after the passing tally and before the node's result leaves the Step's own claim to decide (the node
halts as any side-effect node does, or re-runs if retry-safe); the gate is not asked again, because
its tally is recorded. `ResolveHalt` needs a live attempt marker, which only a passed gate's Step
writes, so a resolution cannot run a node past a gate that did not pass.

**Loops: per iteration.** A gate inside a bounded loop body is evaluated in every iteration, under
that iteration's key. Each iteration runs the node again with a new input, possibly with another
effect (a second refund), and a signature binds one input, so a per-node approval could only approve
an input nobody has seen yet. Per iteration is the only reading under which "these approvers
approved this action" stays true; a flow that wants one approval for the whole loop gates a node
before the loop. The TLA+ regression `flows/regress/gate-unscoped` shows a per-node key lets the
second iteration run unapproved.

**Switch arms.** A gate is evaluated only on a node the run reaches: a gated node on an arm the
Switch did not take never pauses. A gated switched node pauses before it runs; its arm is chosen
after, from its output.

**Denial and rollback.** Plan flows have no saga mode, so a denial compensates nothing: the nodes
that ran before the gate keep their effects, the run stays unfinished (every later drive returns
`ErrApprovalDenied` from the recorded tally), and `agent.Cancel` ends it. A late approval never
reopens a denied gate. A gated agent tool inside a saga keeps its existing behavior.

**Digest and topology.** `Topology` reports each node's gate (`TopologyNode.Gate`) and the digest
commits to it, written only for gated nodes, so every ungated flow keeps its digest and a paused run
cannot be resumed under a flow whose gate was removed, loosened or changed. A node carries one gate:
`plan.Approval` or a config block on a tool that declares another gate is refused.

**Evidence.** `audit.ApprovalEvidence(store, runID, "<node key>", sth)` emits the request (Kind
`call`), every decision the gate read, the tally, and the node's record (Kind `step`), and
`audit.VerifyApprovals` (and `bide-audit verify-approvals -call <node key>`) recounts it against the
policy the auditor expects, as for a tool call. A denied gate has no node record, so its evidence
does not verify, as a denied tool call's does not.

**Model.** `spec/tla/flows` (model 7) adds the gate before each gated node's Step and approvers who
decide at any time: `GateHeld` (a gated body never runs with fewer than Need approvals for its key,
across crashes, error replies, two drivers and the resolver), `GatedResults` and `TallySound`. The
regressions `gate-missing` (no gate check), `gate-unscoped` (a per-node key in a loop) and
`gate-request-order` (a decision recorded before the request counted) fail `GateHeld`. The counting
rule over signed records, and that a counted signature binds the request's input, are model 1b's
and the Go tests' (`plan/rev_gate_test.go`, `plan/gate_test.go`), not model 7's.

## Original design

The sections below are the design as first proposed, kept as the record of the reasoning. Where
they differ from v2 above (decision keys, the signed bytes, the tally's fields, the evidence
contents), v2 is what shipped.

## Why

Today a tool marked `Safety{RequiresApproval: true}` pauses the run for exactly one human
decision: the loop raises `*PendingApproval`, an operator calls `agent.Approve(runID,
toolUseID, true)`, and a re-run proceeds (see `agent/store.go` `Approve` and `agent/agent.go`
around the `RequiresApproval` gate). That is 1-of-1. Regulated workflows routinely require
*more than one* named human to sign off before a material action: two of three officers on a
refund above a threshold, a maker plus two checkers on a wire, a risk sign-off in addition to
ops. There is no way to express "k distinct approvers of n must approve before this tool runs,"
and no provable record that they did.

This is the human counterpart to `govern.Quorum`, which already tallies k-of-n over *model*
voters. The headline property, and the reason it belongs in this runtime rather than an
external approval service: **the approvals are journaled, at-most-once per approver, and the
"k of n approved before the action executed" fact is offline-verifiable** against the same
signed tree head as the action itself. An external approval queue can gate the call; only this
runtime can *prove* the gate held.

## Placement in the existing model

m-of-n approval sits between two things already shipped, and is the composition of one half of
each:

- **Approval (async collection).** `Approve` records one human decision as a durable step
  (`"approval:"+toolUseID`, `Kind: StepApproval`), idempotent, first-write-wins, and the run
  stays paused as `*PendingApproval` until the decision lands. Decisions arrive *over time from
  outside the run*, exactly like a signal. This is the collection model we keep.
- **Quorum (tally + gate).** `govern.Quorum` groups normalized decisions, computes a
  deterministic plurality, and reports `VotesFor >= k`; each vote and the tally are provable
  via `audit.ProveStep`, and the k-of-n gate is expressible as a gsm invariant over the count
  (see `examples/govern/quorum`). This is the tally/gate math we keep.

**The crux: `govern.Quorum()` itself does not fit, and reusing it would be a bug.** `Quorum`
fans voters out *synchronously* with `agent.Parallel`: each `Voter.Decide(ctx)` is a function
the runtime calls *now* to produce a decision. Human approvers do not work that way: their
decisions arrive asynchronously, out of band, possibly days apart, possibly never. So m-of-n
approval reuses the *tally semantics* (deterministic grouping, `count >= k`, provable gate) but
collects decisions with the *approval model* (external `ApproveAs` writes, run paused between
them), not the quorum fan-out. Same arithmetic, opposite collection direction.

All of it rides the existing substrate: named durable steps (`Durable.Do`, at-most-once by
name) plus `History` replay, and the typed `*PendingApproval` pause that `Run` already
propagates. No new persistence model.

## Design decisions (objections first)

**1. Bounded approver set, not an open one.** The policy names its eligible approvers, so
`n = len(Approvers)`. This is not incidental: it is what makes *denial* decidable and
*eligibility* provable.

- With a bounded set, the gate can deny as soon as reaching `Need` becomes impossible
  (`n - denials < Need`), and the evidence bundle can prove *who was eligible*, not just who
  approved.
- An open set can only ever *approve* on the k-th yes; it can never auto-deny, so a gate that
  never reaches k hangs forever. Resolving that needs a deadline, which is a separate feature
  (see "Deferred"). Bounded first; open sets wait.

**2. Decisions are signed, not just attributed by string.** This is the decision that
determines whether the feature is an accountability primitive or a glorified counter. A bare
`approverID string` makes "2 of 3 approved" only as trustworthy as whoever called `ApproveAs`.
Each decision should carry the approver's signature, reusing the signing machinery already in
the grant/delegation code, so the tally inherits non-repudiation and the evidence bundle proves
*these specific approvers* decided.

- *Authenticating* the approver id (that the caller is really "finance") stays the caller's
  identity provider's job, brought in via ports/MCP, consistent with the SDK's BYO non-goals.
  The core journals and verifies the signature over `(runID, toolUseID, approverID, approved)`;
  it does not run an IdP.

**3. Distinctness is enforced by the step key.** Each decision is journaled under
`"approval:"+toolUseID+":"+approverID`, so it is idempotent *per approver*: one approver cannot
be counted twice, and the first decision an approver records wins (later flips are ignored, same
rule as 1-of-1 `Approve`). Only decisions from eligible approvers with a valid signature count.

## API

The code blocks below show the shipped signatures and types. The prose around them is the original
design; where the implementation refined it, the notes at the top and the v2 section govern.

### Policy on the tool (`agent/tool.go`)

`Safety` gains one optional field. `nil` keeps today's exact 1-of-1 behavior, so the change is
backward-compatible. (Since superseded: the policy is now `ToolSpec.Approval`, see the notes above.)

```go
type Safety struct {
    // ... existing fields ...

    // Approval, when non-nil, upgrades the approval gate from one decision to an m-of-n
    // human gate over a bounded, named set of approvers. nil = the existing 1-of-1
    // RequiresApproval behavior.
    Approval *ApprovalPolicy
}

// ApprovalPolicy declares a k-of-n human gate. Need is k; the eligible approvers are the
// bounded set (n = len(Approvers)). A tool with a non-nil Approval requires approval whether
// or not RequiresApproval is also set.
type ApprovalPolicy struct {
    Need      int      // decisions required to proceed (k), 1 <= Need <= len(Approvers)
    Approvers []string // eligible approver ids; the bounded set (n)
}
```

The gate predicate becomes `RequiresApproval || Approval != nil`.

### Recording a decision (`agent/approval.go`)

<!-- docsnip: skip the v0.4.0 API as designed; ApproveAs is now SubmitDecision with a Decision that names its Alg -->
```go
// ApproveAs records one named approver's signed decision on a tool call gated by an m-of-n
// Approval policy. sig is the approver's signature over
// ApprovalDecisionBytes(pend.Subject(), approverID, approved). Each distinct decision is its own
// journal record; the gate counts each approver's first valid decision and ignores the rest.
// WithDecisionCheck verifies the decision at submission and records nothing if it would not count
// (ErrInvalidApproval, ErrAlreadyDecided). After enough decisions land, re-run with the same runID.
func ApproveAs(ctx context.Context, d Durable, runID, toolUseID, approverID string, approved bool, sig []byte, opts ...ApproveOption) error
```

`Approve` is retained unchanged for the 1-of-1 path (equivalently, the single-approver case of
`ApproveAs`). The journaled record gains `Approver string` and `Signature []byte` alongside the
existing `Approved bool`.

### The pause carries the tally (`agent/store.go`, `agent/approval.go`)

`PendingApproval` (now `ApprovalPending`, one of the kinds of the sealed `agent.Pause`) gains an optional tally so an oversight surface can render progress
("1 of 2 in, waiting on risk"):

<!-- docsnip: api agent -->
```go
type ApprovalPending struct {
    RunRef           // RunID, and RootRunID: the run to re-invoke (differs from RunID inside a sub-agent)
    ToolUseID string
    ToolName  string
    Args      json.RawMessage
    // Quorum is non-nil for an m-of-n gate: the running tally at the pause.
    Quorum *ApprovalTally
}

type ApprovalTally struct {
    Need       int      // the policy's k
    Approvers  []string // the eligible set the gate enforced, in policy order
    Approved   int      // approvers whose counted decision is an approval
    Denied     int      // approvers whose counted decision is a denial
    ApprovedBy []string // those approvers, in journal order
    DeniedBy   []string // those approvers, in journal order
    Pending    []string // eligible approvers with no valid decision yet, in policy order
    // Records names every decision record on this call the gate read, valid or not, in
    // journal order. Evidence must disclose all of them, so an omitted decision is detectable.
    Records []string
}
```

## Gate evaluation

At the point where the loop currently checks `RequiresApproval` (`agent/agent.go`), when
`Safety.Approval != nil` it instead reads the journaled decisions for this `toolUseID`, keeps
only eligible approvers with a valid signature, dedupes by approver (first decision wins), and:

- `approved >= Need` -> proceed (run the tool).
- `n - denied < Need` (reaching k is now impossible) -> deny: skip the tool and feed the model
  the same denied-tool-result it gets from a denied 1-of-1 approval today.
- otherwise -> raise `*PendingApproval` with the `ApprovalTally` filled in; the run stays
  paused, deterministically, until more decisions land.

The count is journaled as its own durable step (`"approval-tally:"+toolUseID`) so the tally is
provable as a single record, not only reconstructable from the individual decisions. The gate
itself (`approved >= Need`) is expressible as a gsm invariant over the count, mirroring the
`examples/govern/quorum` pattern, which makes "k approved before the action" machine-checked over every
possible count rather than asserted.

## Provable artifact

The point of doing this in the runtime. For a quorum-gated action, `audit.Evidence` bundles:

- the inclusion proof of the tool-result record (the action ran), and
- `Need` inclusion proofs of distinct, signed, eligible approver-decision records, all under
  the same signed tree head, all ordered *before* the action.

`bide-audit` verifies offline that k distinct eligible approvers signed approval before the
action executed. The claim it certifies is exact: *these named approvers approved, and the
action did not execute until k of them had*. It does not certify that the humans were right,
only that the gate provably held, the same verify-do-not-trust stance as the rest of the system.

## DSL / config lowering (`plan/config.go`)

The gate is a node attribute, so it drops straight into declarative config (and therefore into
any visual builder that emits it):

```yaml
- name: refund
  block: refund
  approval: { need: 2, approvers: [ops, finance, risk] }
```

`configNode` gains an optional `approval` block that lowers to `Safety.Approval` on the built
node's tool. A visual builder renders an "approval gate: 2 of 3" node whose guarantee no
graph-first runtime can back, because the proof comes from the journal, not the diagram.

## Non-goals (for this increment)

- **Authenticating approver identity.** The core verifies a signature against a registered key;
  binding an id to a real person is the caller's IdP, brought in via ports (BYO).
- **Weighted votes and role predicates** ("at least one approver from risk"). Later; the tally
  stays a plain count for now.
- **Open or dynamic approver sets.** Bounded set only, so denial is decidable (see decision 1).
- **Delegated approval** (an approver hands their slot to another). Composes cleanly with the
  grant/attenuation machinery later, but out of scope here.

## Deferred, and how it composes

- **Deadline to resolve a stuck gate.** A gate that never reaches k should be resolvable by a
  durable deadline (deny on expiry, or escalate). This is exactly the deadline/obligation
  feature: the obligation is "reach k approvals by T," and its breach proof is the absence of a
  passing tally before the STH at T. Build obligations, then wire the timeout in.
- **Weighted / threshold-by-role quorum.** An extension of the tally, not the collection model.
