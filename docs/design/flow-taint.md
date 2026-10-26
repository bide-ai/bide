# Design: taint in plan flows, and its view in the flow editor

Status: proposed. The maintainer approved the direction (taint coverage in the visual editor, which needs a
run-time rule for flows first) and decided the open questions on 2026-10-26: what a node has read is the
run-level rule, a tighten-only per-node `untrusted:` config key is in, a node's own gate does not cover taint
exposure, and an opt-in disjoint-approvers check is in (the name is not yet approved). Section 10 lists them.
Nothing here is built. Written against `main` at aab4f530 (v0.25.0). It resolves decision 18 of the
[untrusted content and amount caps note](untrusted-content-and-amount-caps.md) (section 5.9: "specify taint
and caps for flow nodes in a separate note"), for taint; caps for flows stay out of scope. Where it says what
the code does today it names the file and line, so a reviewer can check it; where it is about the design it
says "would" or states a rule.

The note has two parts. Part A is the run-time rule for plan flows: what is declared where, how it is
enforced and journaled. It is the larger and safety-relevant part and each of its pull requests goes through
the adversarial-review gate. Part B is the view on top of it: the static analysis in `plan`, `bide check`,
`bide check -diff` and the flow editor (`bide edit`).

## 0. What the code does today

Plan flows have no taint rule at all today, so there is nothing yet for a static view to show. A view of
"which steps may read untrusted input, and which approval gates that would trigger" (as `bide view` shows for
agent runs) assumes a flow-level semantics that does not exist in the engine, the config, or the registry.
Showing "escalates" for a flow node would be a claim about behaviour the runtime does not have.

Facts, with locations:

- Agent taint is per model turn, derived from the journal, run-level, no per-value tracking:
  `agent/taint_fold.go:41` (`foldTaint`: marks are the run input, a retrieval step, or a tool result's
  `Safety.Untrusted`, all recorded before the requesting model record), `agent/taint_gate.go:150-215`
  (`escalate`: a tainted turn's side-effecting call needs `TaintPolicy.Gate`, or is refused with
  `Refuse`; `TaintExempt` skips it), `agent/taint_gate.go:247` (`taintApplies`: run under taint, policy set,
  tool `tracked(spec.Safety)`, not a compensation).
- Declarations: `agent.Untrusted(source)` (`agent/taint_marks.go:48`), `Trusted(reason)` (:77),
  `TaintExempt(reason)` (:62), `WithUntrustedInput` / `WithTrustedInput` (:112, run option),
  `RetrievalUntrusted` (:139), `TaintPolicy{Gate, Refuse, RequireProvenance}` (`agent/taint_policy.go:23`),
  escalation subject `<call>#taint` (`internal/escalation/escalation.go:11`). Source names are
  `checkProvenanceName`-checked strings.
- Plan flows refuse the declarations: `plan/builder.go:525` (`checkTool`) returns ErrConfig for a tool with
  `Untrusted`, `Safety.Untrusted` or `TaintExempt`; this is how `RegisterTool` and `Builder.Tool` keep a
  silent loss out. A flow Step or Model block has no way to say "I read untrusted data". The flow YAML
  (`plan/config.go:30`, `configNode` :58) has no taint key. A flow Model node is a single `agent.Generate`
  with no tools (`plan/flow.go:651-676`), so there is no tool call inside a flow model step.
  A flow node that is an agent-running tool does get the agent's own taint for its own calls.
- Docs say the same: design note section 5.9 and decision 18 (`docs/design/untrusted-content-and-amount-
  caps.md:196-205`, :484: "specify taint and caps for flow nodes in a separate note ... a rule for what a
  node has read (node order or data edges) and a place for the escalation gate in the flow digest"),
  `docs/guides/plan-flows.md:863`, `docs/KNOWN-LIMITATIONS.md:857`, ROADMAP line 22.
- `Registry.Describe` / `RegistryDescription` (`plan/describe.go:17-60`): a block carries `Safety` and `Gate`
  only; nothing about provenance, exemption or policy. So typed mode (`flowedit.WithRegistry`,
  `WithBlocks`, `flowedit/typed.go:20-110`) and `bide check -blocks` know no taint either.
- `bide view` on a run: `internal/runview/model.go:633` attaches `gatereason.Find` reasons to the
  escalation instance `<call>#taint`, but `withEscalationInstances` (:592-600) skips `node:` keys with the
  comment "a flow has no escalation instance". So `bide view` shows taint for agent runs only, after the fact,
  from the journal. `plan.InspectConfig` and `bide check` report nothing about taint.
- Agent side already has a static declarations report and diff: `bide doctor` / `bide doctor -diff`
  (`cmd/bide/internal/doctor/declarations.go`, `internal/declarations/diff.go`): a removed `Untrusted`, a new
  `TaintExempt` are reported as weakened. Flows are outside it.

So the editor feature needs flow taint at run time first (Part A), then the view (Part B), which rides on
`ConfigReport`. The analysis in Part B, section 1, is the may-version of the rule in A2.

## Part A. Taint in plan flows at run time (the safety-relevant part)

The ROADMAP's "plan flows, which a taint policy and caps do not cover" holds in the code (section 0). Part B
is sound only if it shows what this part enforces.

### A1. What is declared where

Go declares what loosens (`Trusted`, `TaintExempt`, the policy); the config may declare only what tightens
(`plan/config.go`: a config "may only lower" safety). Names mirror the agent's, so one vocabulary.

- Block provenance: NodeOptions `plan.Untrusted(source)`, `plan.Trusted(reason)`, `plan.TaintExempt(reason)`
  on Step, Model, Join and Tool blocks (`RegisterStep`, `RegisterModel`, `RegisterJoin2/3`, `RegisterTool`,
  and the Builder twins). A Tool block takes them from its spec, so `checkTool` (`plan/builder.go:525`) stops
  refusing `Untrusted` and `TaintExempt`; a NodeOption that disagrees with the tool's spec is ErrConfig, and
  a NodeOption may not drop the tool's `Untrusted` (the larger-of rule).
- Flow policy: Build/Load option `plan.WithTaintPolicy(agent.TaintPolicy)` (Gate or Refuse, optional
  `RequireProvenance`), same checks as `agent.WithTaintPolicy` (`agent/taint_policy.go:36`). Opt-in: a flow
  with no policy gates nothing (the user's amendment to decision 6), and a run is never retro-gated.
- Input provenance: no new name. `agent.WithUntrustedInput` / `WithTrustedInput` are RunOptions and
  `Flow.Run` already takes `agent.RunOption`s (`plan/flow.go:104`); the flow records the choice in run:start
  settings as an agent run does (`agent/runstart.go:98`). Needs the engine to take them for a flow start.
- Registry description: carries the same declarations (Part B), so typed mode sees them.
- Config (decision 3, section 10): a tighten-only per-node key `untrusted: <source>` (the name rules of `Untrusted`).
  It adds a source to the node's output, so a config author can mark a library Step they did not write; it is
  unioned with the block's Go declaration (never replaces it), and it is `omitempty`, so no existing config
  digest moves. It is not in the topology digest: its enforcement is the declarations digest recorded in
  `run:start` (A2, A3), which a resume compares exactly. The config cannot declare `trusted` or an exemption
  (those loosen). Removing or changing the key is a weakening in `check -diff` (section 6). The policy stays
  Go-only: a host that drives flows from YAML alone cannot opt in, and the docs say so.

### A2. The rule the engine enforces

Journal-derived like agents (`agent/taint_fold.go`), not computed once at build: the requirement is a pure
fold of the journal, the recorded declarations (A3) and the topology.

**What a node has read (decision 2, section 10): the run-level rule.**

N's read set is every declared source whose node has a recorded result earlier in the journal (loops
  included, so iteration k sees iterations below k), plus the flow input if the run declared it untrusted,
  plus inherited marks and the `taint-mark:` records below. No join, switch or loop special case, and no
  coupling to `nodeInput`. It is the agent rule: run-level, monotone, "after any mark every later turn is
  tainted" (`agent/taint_fold.go:41`). Why not data edges: a node's Go code can read what an earlier node
  left elsewhere (a shared store, a cache, a captured variable), and a fan-out sibling B of an untrusted
  reader A would have an empty read set although A ran first in the same sequential process
  (`plan/flow.go:216-230`); a data-edge rule would make a flow weaker than an agent. Cost: more nodes read as
  tainted (every node after a source, and a whole loop body); the data path remains as an explanation in the
  view. Data-edge precision could come later as an explicit opt-in with the undeclared-channel caveat.
The flow input is a source only when the run declares it (A1); taint is monotone; a node that
is both a source and a side effect runs ungated (its read set excludes itself, as an agent tool does in the
turn that reads), so such blocks should be split, and the docs say so.

**Requirement and order.** Read set non-empty, the node can have a side effect
(`tracked(node.safety)`), not `TaintExempt`, run under taint. Per drive, per node, in this order:
1. compute the requirement from the fold and insert-once the gate-reason record (a pure record, also
   written for a refused node);
2. if the policy is Refuse: refuse, before any gate, so no approval is wasted;
3. the node's own gate (`runGate`, `plan/flow.go:317`), then the escalation gate on the instance subject;
4. both PreExecs, node gate then instance, immediately before `runNode` (`runGate` ends with
   `journalhook.PreExec`, `plan/gate.go:58-69`; the agent has `preExecuteEscalation`,
   `agent/taint_gate.go:240-262`; a certificate revoked after approval must stop the node);
5. the attempt claim, inside `journalhook.Step`, unchanged. Gates run before the claim, so at-most-once is
   untouched; a replayed node runs its gate reads again (one read each, harmless once the exact resume comparison holds).

**Subject.** The escalation instance of a flow node is `node:[iter:<n>:]<name>:taint`. A node name holds
no ':' (`checkStepName`, `plan/builder.go:216`), so the form collides with no node, and `:step:taint` (a
node's nested Step named taint) is distinct. `encodeID` (`agent/keys.go:350`) already escapes ':'. No '#' ban
at load, so no change for existing flows with '#' in names. Why not `#taint`: `escalation.Base` returns
false for `node:` subjects (`internal/escalation/escalation.go:19-24`), so intake would serve a
`node:x#taint` decision under the node's own gate (`approvals/approvals.go:738,744`, decoy check skipped),
audit evidence would fail (`audit/approval.go:109-114,400-407`), the view would skip it, and Conform would
flag it undeclared.

The instance is not a node key, and the gate machinery today requires one. `parsePlanKey` returns false for
`node:x:taint`, but `decodeApprovalRequest` (`agent/flowgate.go:44`), `flowDecision` (:61: a non-plan key is
treated as an agent call and skips the decision-after-request rule), `flowGate` (:116: ErrConfig "gate key is
not a plan node key") and `gate_standing.go:96` (result lookup `planNodeKey`) would all refuse or mis-handle
it, while the `HasPrefix(x, "node:")` sites would match it (`internal/runview/model.go:392,599,656,676`;
`approvals/approvals.go:748,1006`) and `model.go:656`'s resulted check `r.Name == id` is wrong for an instance
(the result is the base node's). So PR 3 adds ONE predicate pair, in one place, with one parser:
- `planGateKey(name)`: a node key OR an instance; `planInstance(name) (nodeKey, ok)`.
- The parser lives in `internal/escalation` (agent imports it and it cannot import agent), and `Base` uses
  the parser, never a ":taint" suffix test, so `node:x:step:taint` is not an instance. Tests for
  `node:x:step:taint`, `node:iter:3:taint` (node "taint", iteration 3), `node:iter:taint` (instance of node
  "iter"), `node:iter:3:x:taint`, `node:x:taint:taint`.
- Every use of `planNodeKey`, `planNodeStep` and `HasPrefix(.., "node:")` is classified in the PR as
  node-only, gate-subject or either, with a test per site: node-only stays (`halt.go:238`,
  `journalhook.go:27`, `internal/journalhook`, step name checks); gate-subject takes `planGateKey`
  (`flowgate.go:44,61,116`, `gate_standing.go:96` with the result of the BASE node, intake, view `gates()`,
  `gatereason.Find`, audit evidence); either takes both.
- Consumers (A4) change in the same PR with a test each: a decision for the instance cannot be served under
  the node's gate and the reverse; `flowDecision` applies the decision-after-request rule to the instance;
  evidence verifies; the view shows the reasons. A loop with max N may ask up to N times, one per iteration
  subject `node:iter:<n>:<name>:taint`; the docs say so.

**Refuse.** Order inside a drive: compute the requirement; insert-once write the gate-reason record
with `refused: true` (union rule, `agent/gate_reason.go`; a refused node therefore HAS a gate-reason record);
then return the sentinel before the node's own gate. It writes no node record (a `node:x` record would be
read as the node's output, `plan/flow.go:320-324`), no attempt marker and no approval request. Every drive
recomputes the requirement and re-refuses (stable under the resume comparison and the recorded declarations). Like `ErrApprovalDenied`
(`plan/gate.go`) the run stays unfinished until `agent.Cancel`; recovery passes re-drive and re-refuse it each
time (no new terminal marker; an operator cancels). Never a skip, which would change the flow's output. Tests:
crash after the refusal record; resume under Gate; resume under no policy.

**Resume and a weaker policy.** Today `run:start`'s digest is only a flag (`setUnderTaint(start.
taintDigest())`, `agent/runplan.go:217`), and nothing compares it with the current `taintDigest()`
(`agent/taint_policy.go:105`); `escalate` compares only the gate digest in a gate-reason record, which exists
only after a call was first held (`agent/taint_gate.go:166`); `holdToStart` (`agent/runstart.go:420-470`)
keeps the first writer's start and compares kind, flow, saga, principal and input only. For flows: after
`Begin` (which returns no start) read the journaled start with `agent.RecordedStart` and set the flow's
under-taint status from IT, never from this drive's options or policy. At every drive compare the recorded
declarations digest with the current one exactly; any difference is ErrConfig before the first node, naming
both digests. (A stricter current policy could be allowed later; the first cut is exact.) A first-writer race
is covered: each drive reads the journaled start, so a second driver with another policy cannot run ungated.
Exact equality strands in-flight runs on any policy or approver-list change: the operator cancels the run
(`agent.Cancel`) or restores the declarations it started under; the docs say so. Each drive also checks that
the recorded declarations map hashes to the recorded digest (cheap). A policy-less flow driving a run with a
recorded digest: side-effecting nodes ErrConfig, read-only flows
resume. Tests: start under P, crash before any node, resume under a weaker P', under a dropped declaration,
under none: no node body runs.

**Declarations are recorded.** A flow node record holds only its output (`plan/flow.go:631-635`), so
"node fetch is Untrusted(inbound-email)" would exist only in Go and as a hash. `run:start` ext therefore
holds the full declarations map (per node: untrusted, trusted, exempt; policy; require_provenance) under a
new key `taint_declarations`, and `taint_policy_digest` stays its digest. The read set is recorded sources
union current sources (current only adds, as `escalate`'s larger-of rule, `agent/taint_gate.go:178`);
exemption comes from the recorded declarations; Conform flags a current declaration that differs from the
recorded one. A record can only add a
requirement. The fold then needs the journal and the topology only.

**Conform.** `plan/conformance.go` replays routing and record names and cannot tell a tainted
side-effecting node ran with no taint gate. Add: from the recorded declarations, the recorded choices and the
node results, recompute each node's obligation, and for every side-effecting node result present require a
passing `:taint` tally, an exemption, or an empty read set; else a divergence. `gate()` (:155) learns the
instance, and `gate-reason:` records are classified (known names, only for reached nodes). `flow:digest`
equality no longer implies equal taint declarations, so Conform checks the declarations digest separately.

**Sub-agent and nested-flow blocks.** An agent-running Tool block cannot be told from a plain tool
today (section 2), so it cannot be detected, refused or flagged; and any generic tool that calls an agent starts
a child whose context carries no under-taint status. So, as a requirement of PR 3, not a deferred item:
- In a run under taint, the journalhook step hook binds the run's under-taint status and the node's read set
  (as inherited marks, only when non-empty) to the context of EVERY node body, so any child agent or nested
  flow inherits them as a child of a tainted turn does (`agent/taint_inherit.go:80-100`), and a policy-less
  child fails closed by the existing `taintRefusal` (`agent/taint_policy.go`, ~240). Documented.
- Taint coming UP: a node whose child learned a mark needs a journal record the flow can read without
  knowing the child's run ID. A context-bound sink, which the child's engine calls when a mark is recorded
  in it, writes an insert-once `taint-mark:<nodeKey>:<source>` record (source `encodeID`d, so a second child or mark source of the same node is kept) into the flow's journal before the node's result
  is recorded (a crash in between leaves an extra mark; add-only, safe). `taint-mark:` records are known names for Conform's `gate()` and classification and are fold input for the
  obligation check. The fold reads these records as
  sources `agent:<name>` / `flow:<name>` for the nodes after it. A block may declare `Trusted(reason)` to say
  it brings nothing up (no sink consulted then).
- Not in this cut: folding the child's journal at `SubRunID(run, nodeKey)`.

**Disjoint approvers (decision 8: opt-in; the name awaits approval).** Without it nothing stops one approver
approving both `node:x` and `node:x:taint`, so the intended second look can be one person. A decision for one
instance cannot count for the other (the signature binds the subject key, `staleDecision`,
`agent/flowgate.go:99`, once the subject parsers recognise the key); this is about the person.

- Shape: a field of the policy, `TaintPolicy.SeparateApprovers bool` (alternative name: `DisjointApprovers`),
  not a plan-only option and not a config key. Reason: the same weakness exists for an agent (a tool's own
  gate and its `<call>#taint` instance), a plan-only option would leave agents without the control, and
  `plan.WithTaintPolicy` already takes `agent.TaintPolicy`. Cost: one exported field on an agent type and an
  agent behaviour behind a flag that is off by default. Go-only, as the policy is.
- Meaning, when set: an approver whose decision is counted on a call's own gate is not counted on its
  escalation instance, and the reverse (the tally excludes them as `ExcludePrincipal` excludes the requester;
  the viewer says "excluded: already decided on the call's other gate", as `internal/runview/approvers.go:143`
  does for separation of duties). It works on approver IDs; two IDs of one person are not detected (the
  approver-identity design is the layer for that), and the docs say so.
- Two layers. (1) Registration or Build: for every tool or node that can be escalated (side-effecting, not
  exempt) and has its own gate, the own gate's approver set and the policy Gate's approver set must be
  disjoint, else ErrConfig naming the tool or node and the shared IDs; a node with no gate of its own has
  nothing to separate from. This fails early and makes the gates always satisfiable. (2) Run time: the tally
  of each instance excludes approvers with a counted decision on the sibling instance, read from the journal;
  it is defence in depth for approver sets that differ from registration (WithApproverVerifiers, rotation),
  and order-dependent when sets overlap (the first to decide takes the seat), which layer 1 prevents.
- Journaled and digested: the field is in the taint declarations digest only when set (the precedent is
  `RequireProvenance`, `agent/taint_policy.go:79`), so no existing digest moves, and it is in the policy
  summary recorded in `run:start`, so the exact resume comparison refuses a drive that dropped it. The
  gate-reason record is unchanged.
- With `ExcludePrincipal`: independent and cumulative. On the instance an approver counts only if they are not
  excluded by the Gate's `ExcludePrincipal` (not a requester of the run) and, with this field, not a decider
  of the node's gate. The Build check compares ID sets as written, before any requester exclusion, so it does
  not depend on who started a run.
- Names: one exported field. Flag: none. Config key: none.

**Options a flow run takes.** `Flow.Run` takes `WithIdentity` through `journalhook.RunIdentity` today. It
also takes `WithUntrustedInput` / `WithTrustedInput`, recorded in the start settings; a drive's option that
disagrees with the journaled start is ErrConfig (the agent path compares settings in `holdToStart`; `Begin`
compares kind, flow, saga, principal and input only, so the flow path adds it).

**Policy and declaration surface.** Policy is Go-only: a host that drives flows from YAML alone cannot opt in;
the docs say so.

### A3. What is journaled

- run:start `ext.taint_policy_digest` (key reused, flow-specific format tag), plus `ext.taint_declarations`
  (new ext member, recorded map, see A2), plus input provenance in settings. The presence of the digest puts
  the run under taint; its equality is checked every drive (A2).
- The gate-reason record at the node key, insert-once, as `escalate` writes it (`agent/gate_reason.go`):
  reasons (source, ref "node:<name>" / "input" / "inherited", journal position), policy digest, gate.
- The instance's approval records under `node:...:taint`: request, pause note, decisions, tally, as the flow
  gate's (`agent/flowgate.go`). `ApprovalRequest` for the instance carries the node's input, so a decision
  binds the same input.
- `flow:topology` and `flow:digest` unchanged; no existing digest moves (the config key of decision 3 is omitted when unset).
- `bide doctor` (`agent/declarations.go:17`, `internal/declarations/doc.go:31`): a flow's digest carries a
  flow format tag and never matches an agent document, so the declarations document is read by kind.
- Conform (`plan/conformance.go`) checks obligations, not only known names: for each journaled node record
  whose read set (recomputed from the journal) required an escalation, the run has a gate-reason record and
  an approved instance tally, or the node is exempt, or the node has no record because it was refused. A
  tainted side-effecting node record with none of these is a divergence. Unknown step names are not enough.

### A4. Consumers that must learn the new instance

`bide view` (`runview/model.go:592`, drop the "a flow has no escalation instance" skip for flow runs),
`bide approve -list`, the approvals intake (it must not decide the instance under the node's own gate, `internal/escalation`) and the approve page (they resolve `<call>#taint` for agents; add the `node:...:taint` form),
notifier payload (`agent/notify.go:75`), `bide audit` / conformance (`plan/conformance.go`: the new records
must be known step names, not divergences), `plan.Flow` Conform replay of the escalation instance. Each is a
review checklist line.

### A5. Adversarial-review gate: what the reviewer must break

Fail-closed properties to attack: (1) a node with a non-empty read set never runs without the escalation gate
or refusal, through every path: first drive, resume after crash between gate pass and claim, loop iterations,
switch arms, joins, a recorded gate-reason from an older drive, a store writer who adds or deletes records;
(2) a decision for `node:x` cannot pass `node:x:taint` and the reverse, and a loop iteration's decision
cannot pass another iteration's; (3) no path clears taint (monotone); (4) no name or key collision: `node:x:taint` against a node, a node's step, a loop key; and every consumer treating it as the instance, never as the node;
(5) policy removed, changed gate, changed declarations between drives; (6) the refusal cannot be turned into
a skip that changes output; (7) the Part B analysis never says "not reached" for a node the engine gates; (8) resume with a weaker or different policy or declarations is refused, and the run:start digest is compared every drive, not read as a flag; (9) the recorded declarations cannot be edited in the store to lower a requirement (only-add rule); (10) a refused node has a gate-reason record and no node record, no attempt and no approval request.
Formal model: A5b names the invariant and the six mutants (extend `badFire`, Flows.tla:196). The `protocol:flows` markers (`plan/flow.go`) must wrap the new
calls so the protocol conformance test covers them. That is the cost driver.

### A5b. Model additions

`spec/tla/flows/Flows.tla` models one fixed graph (E,Q,T,H,S,N) with one gate per key and no Join, taint,
policy or sources. Add: a Sources set; the read-set fold as a function of the journal (recorded declarations,
results present, choices); a second gate instance per key with its own req, dec and
tally scoped per iteration (GKey-style); Refuse as a mode; a Join with one tainted and one clean input, and a
stale-result variant; a drive resuming under changed declarations (must be refused); and an invariant that a
side-effecting node's body fires only if its read set is empty, or it is exempt, or its taint tally passed
(extend `badFire`, `Flows.tla:196`). Mutants in `regress/`: NoTaintGate, TaintGateUnscoped, TaintReuse (a
node-gate decision counts for the taint key), ReadSetOnResumeEmpty, JoinDropsTaint, RefuseAsSkip. The
`protocol:flows` markers (`plan/flow.go:141,317,407,584,700`) must wrap the new calls; `spec/tla/claims/
taint-*.cfg` can be reused to count the instance.

### A6. New names, part A (all need the user's approval)

Go API: `plan.Untrusted`, `plan.Trusted`, `plan.TaintExempt` (NodeOptions); `plan.WithTaintPolicy` (flow
option); one refusal sentinel (name sketch `plan.ErrTaintRefused`, wrapping the agent's refusal class).
Behaviour changes: `checkTool` accepts `Untrusted`/`TaintExempt` tools; `Flow.Run` honours
`WithUntrustedInput`/`WithTrustedInput`. Journal: the derived subject `node:[iter:<n>:]<name>:taint`,
a gate-reason record for node keys, `run:start` ext key reused (`taint_policy_digest`, new format tag
`bide.flow-taint-declarations.v1`). Config key: `untrusted: <source>` per node (decision 3). Flags: none. CLI: none. `:taint` adds no exported name. `Flow.Run` takes `WithUntrustedInput`/`WithTrustedInput` (existing names). `bide doctor` declarations
document gains flow blocks (reviewed with Part B).

## Part B. The editor and `bide check` view (on top of Part A)

## 1. The static analysis (the over-approximation of A2)

Define it once in `plan`, used by the engine, `InspectConfig`, `bide check`, the editor.

Sources (a node "introduces" taint): a block declared untrusted (new, Go: Step, Model, Tool), and the flow
input when the run declares an untrusted input.

Propagation follows the run-level rule of A2: a node may have read a source when the source may execute before
it. Statically: the engine's own deterministic order (`topoOrder`, one shared function, never re-implemented)
puts the source earlier, or both lie in one loop body (every iteration after the first sees the whole body;
every node after the loop sees all of it). No refinement for mutually exclusive switch arms: an
over-approximation, stated in the UI line.
Taint is monotone: only a block's `TaintExempt` or the run ending stops it (design note 5.7). The data path is
computed too, only to explain ("via triage", else "earlier in the flow order").

The flow input: whether it is untrusted is a RunOption the config does not carry, so it is a third
state, never "not reached": "reads untrusted if the run's input is untrusted" for every node reachable from
the entry, unless the flow declares a flow-level `Trusted`. A node is never called "not reached" without
"unless the run declares its input untrusted". An agent-running node may bring taint up to downstream nodes
(see "Sub-agent and nested-flow blocks"), so downstream is "may read" with no declared source in the flow.

Per-node verdict, computed from topology, declarations and policy:

| Verdict | Meaning |
|---|---|
| reads untrusted (source list) | a declared source can run before it |
| escalates | tainted, side-effecting (`tracked`), not exempt, policy has a Gate: needs `<node>:taint` |
| refused | same, policy has Refuse |
| exempt (reason) | tainted, `TaintExempt` declared |
| no declared source reaches it | unless a block runs an agent or a nested flow (whose taint this view cannot see) or the run declares its input untrusted (see 2); never "no taint" |
| unknown | declarations or policy not known (structural mode, description without the fields) |

Only a node whose Safety can have an effect (readonly excluded, as `taintApplies`) can escalate or be refused.
Safety comes from `ConfigReport.Safety` (exact with a Registry, lowered by config; structural knows it only
where stated).

## 2. What a static view cannot know, and must not claim

Cannot know:
- Data-dependent switches: all arms are treated as live. "May" only.
- What a Step, Tool or Model does inside: a step can read a database or the network without declaring
  `Untrusted`. Undeclared is not trusted. Only a declaration (`Trusted(reason)`) says trusted, and the
  engine's `RequireProvenance` is the existing answer for unknown (a registration error).
- Whether a Model node's prompt contains tainted text: it does when upstream is tainted; the model's output is
  then tainted (the model is the dataflow, design 5.2).
- A tool-call inside a model step: not applicable to flow Model nodes (no tools); applicable to a block that
  runs an agent, whose own journal and taint are the agent's, not the flow's. The view says "runs an agent:
  its tool calls are gated by that agent's policy, not shown here" for such a block, when the description
  marks it (it cannot be told from a plain tool today; leave unsaid in structural mode).
- The policy's Gate/Refuse and approvers: Go-side. Config may not loosen, only tighten.
- Journal history: a resumed run that inherits marks.

Must never claim:
- "safe", "no taint", "clean", "protected" for any node. The strongest allowed wording is "no path from a
  declared untrusted source" and only when declarations are known (typed, with the fields).
- Anything about escalation when the policy is unknown (structural, or description lacking the policy).
  Show "unknown: provenance not declared" instead, with the reason (structural mode / description from an
  older release / block declares no provenance).
- That an escalation gate "protects" a node: it bounds one call per approval, not injection (design note
  section 4 limits).
Every state has a text label; no state is encoded by colour or an icon alone.

## 3. Where it shows on the page

All fields come from the server's view JSON, computed in Go. editor.js only renders them.

- Canvas: a text tag under each node (like the run-path words, `editor.js:832`), plus a distinct border style
  (double or dashed) so it survives forced-colors (`editor.css:102`) and the existing themes (`flowedit/
  themes_test.go` contrast checks). Tags: "reads untrusted: inbound-email", "escalates", "refused", "taint
  unknown". A tainted-source node gets "source: <name>".
- Outline (the primary accessible view, `editor.js:209`): the node's button label gains the same words; the
  one-Tab-stop pattern is unchanged.
- Inspector: a "Taint" section for the selected node: verdict, the sources and the shortest path from a
  source ("inbound-email > triage > refund"), the policy line, the exempt reason, and the "cannot know" note.
- A summary panel (like the affected and assertions panels): counts by verdict, a list of side-effecting nodes
  that read untrusted data, and the unknown cases, each a button that selects the node.
- Accessible names: the verdict words are in the `aria-label` of the node, as the safety and gate words are;
  the READ_ATTRS audit (`editor.js:38`) and `assert_words_test.go` style tests check them. A live-region
  announce on an edit that changes any verdict.
- Colour-independent: verdict is text first; colour only reinforces. Same theme tokens, no new palette.

## 4. Structural versus typed

- Structural (default): no declarations, no policy. The view shows "taint: unknown (blocks not known)" once,
  not per node, and never a per-node verdict. It can still show what the config itself states (a per-node
  `untrusted:` key), labelled as the config's. Nothing is
  shown as clean.
- Typed with Registry (embedded editor, `WithRegistry`): exact. Declarations and policy are read from the
  Registry; full verdicts.
- Typed with description (`-blocks`): verdicts rest on the description, labelled so, like every typed result
  (`runview.SourceDescription`). A description from before the fields reads as structural for taint. The
  description must say it carries declarations, so absence is not read as "none".

## 5. `bide check` and `check -diff`

Yes, same analysis, same Go: `ConfigReport.Taint` is computed in `plan.InspectConfig` /
`InspectConfigDescribed` beside `Gated`, and `runview.BuildDiff` reads it as it reads `Gated`
(`internal/runview/diff_paths.go:127`). The editor's baseline `Weakening` already delegates to
`runview.BuildDiff` (`flowedit/baseline.go:9`), so editor and CI agree by construction; no JS analysis.
`bide check` prints a taint section (text and `-json`) only when declarations are known (needs `-blocks` or an
embedded Registry), and otherwise one line "taint: not known without -blocks". No new flag.

## 6. Weakening

Recommendation: yes, as a new weakening kind "exposure", reported in the editor banner and `check -diff`
(banner word: UNTRUSTED EXPOSURE WIDENED), when a change makes a side-effecting node newly read untrusted
data AND it is not covered: the policy is known to Gate or Refuse it, or it is exempt. A node's own approval gate
is not cover (different approvers and purpose; an approval clears nothing else, `agent/taint_policy.go:
49-53`); a decision of the maintainer (decision 9). Specifically:
- The exposure of a side-effecting node changes with: a new source, a new side-effecting node, a node moved so
  that a source may now run before it (order, loop membership), a changed safety (to or from readonly) or
  exemption, and a removed `Trusted`; not with an edge as such. Each is weakened when not covered as above,
  and listed as a change otherwise ("now reads inbound-email, escalates").
- a removed gate on a path that carries taint: already "gate"/"path" weakening (`diff.go:458,483`); the taint
  note is added to its line.
- A removed or changed per-node `untrusted:` is weakening; adding one is strengthening.
- Go-side changes (a removed `Untrusted`, a new `TaintExempt`) are not in a config diff; `bide doctor -diff`
  already covers agents and should be extended to flow blocks' declarations (PR 3), the same note as design
  5.7.
- Unknown policy or declarations: a change cannot be shown weakened or not; the banner says "taint could not
  be compared (structural)", not silence, and a save asks the same confirmation `NotCertified` asks
  (`flowedit/baseline.go:50`).

## 7. New exported names, part B (part A names are in A6), flags, JSON fields, config keys (for approval)

Editor and check alone (PRs 4, 5), assuming the runtime exists: none new beyond:
- `plan.ConfigReport.Taint map[string]NodeTaint` and `plan.NodeTaint` (verdict, sources, path). One type.
- `flowedit` view JSON field `taint` per node and a `taint` summary: unexported structs rendered by the
  existing view; JSON keys only.
- `runview.Change.Weakening` gets the value "exposure": a new string value of an existing field.
No new flag and no new command. Part B reads the per-node `untrusted:` key (A6) from the config as the
config's own statement.

Runtime prerequisites are Part A (A6). Part B additionally:
- `plan.BlockDescription`: `untrusted`, `trusted`, `taint_exempt`; `plan.RegistryDescription`: `taint_policy`
  (gate or refuse, require_provenance) and a marker that declarations are carried (bump `DescriptionVersion`
  to 2; version 1 reads as "taint not known"). Describing is the only place Part A's declarations reach the
  editor and `-blocks`.
- `bide doctor` declarations document: flow blocks and the flow policy, and its `-diff` weakened rules.
- No topology or digest change (A3), no config key, no flag.

## 8. Tests and accessibility

- Unit (plan): table tests per rule (edge, join, switch, loop fixpoint, entry source, exempt, readonly, refuse
  vs gate, unknown); a property test that the verdict is monotone under adding a source or moving a node later; fuzz the topology
  generator already in flowedit. A mutation check per rule (ignore order, ignore loop membership, ignore the entry source, ignore a
  `taint-mark:` record).
- Parity (also the fan-out sibling case and the run-option case): one test that runs the engine (PR 2) and asserts each node the analysis calls escalating is in fact
  held at `<node>:taint`, and no node the analysis calls "not reached" is ever held (soundness against the
  runtime), including loops, switches and joins.
- Editor: server view tests, browser tests (existing chromedp harness) for canvas tags, outline labels,
  inspector path, summary buttons; the weakened banner for a new exposing edge; typed-with-description vs
  structural labels; forced-colors and each theme (`themes_browser_test.go`), contrast of the new tags,
  keyboard path, announce text. No claim words: a test greps the page and the view for "safe", "clean",
  "no taint", "protected" and fails.
- `bide check` golden outputs and `-diff` cases; docs tests (snippet CI) for the flows guide, editor guide,
  `untrusted-content.md`, KNOWN-LIMITATIONS, i18n.

## 9. PR split and cost

Part A (each reviewed adversarially, A5):
1. Design note for flow taint (docs only): A1 to A3, resolving decision 18. Needs the user's approval. Small.
2. Declarations and policy plumbing, no enforcement: NodeOptions, `checkTool`, `WithTaintPolicy`, run:start
   digest, input provenance, TLA+ flows model extension. Medium.
3. Enforcement: read-set fold, escalation gate `node:<k>:taint`, refusal, gate-reason record, resume and
   sticky rules, consumers (A4: view, approve, notifier, conformance). Large; the cost driver (days, with
   review rounds).
Part B (small PRs, after 3):
4. `RegistryDescription` v2 and `bide doctor` for flow blocks. Small to medium.
5. `plan` static analysis in `InspectConfig`, `bide check` output, `check -diff` weakening, parity test against
   PR 3. Medium.
6. Editor view: JSON, canvas, outline, inspector, panel, banner; a11y tests are most of it. Medium.

PRs 1 to 3 are the work of the original taint build (several days); 4 to 6 about a day each.

## 10. Decisions

Decided by the maintainer on 2026-10-26, or accepted as recommended:

1. Part A first, then Part B (the editor shows only what the engine enforces).
2. a node's read set is run-level by journal order, as an agent's (A2).
3. a tighten-only per-node `untrusted:` config key (A1); the policy and the loosening declarations stay Go.
4. Weakening (section 6): only an uncovered side-effecting exposure is a weakening; a covered one is a listed
   change.
5. `DescriptionVersion` 2 with an explicit "declarations carried" marker, so absence never reads as "none".
6. A flow Model node with tainted upstream is covered by the run-level rule; its output is a source for what
   follows.
7. An agent-wrapping block is shown as gated by its own agent's policy, in typed mode only, when the
   description can say so.
8. an opt-in check that the node gate's and the taint gate's approvers are disjoint (A2, "Disjoint
   approvers"). The shape is proposed there; the field name (`SeparateApprovers`, or `DisjointApprovers`) is
   not yet approved.
9. a node's own approval gate does not cover taint exposure.

New exported names (A6, section 7): `plan.Untrusted`, `plan.Trusted`, `plan.TaintExempt`,
`plan.WithTaintPolicy`, one sentinel error (sketch `plan.ErrTaintRefused`), the policy field of decision 8,
`plan.ConfigReport.Taint` and `plan.NodeTaint`, the `RegistryDescription` version 2 fields, and the "exposure"
value of `Change.Weakening`; one config key (`untrusted:`); no flag and no command. The names in unbuilt parts
are a sketch.
