# Untrusted content: provenance marks and the taint policy

An agent that moves money reads text it does not control: a customer's email, a web page, a ticket, a
third-party tool's result. That text can tell the model to act. bide does not try to detect that. It
[bounds what an agent can do after it reads such text](../design/untrusted-content-and-amount-caps.md): a run
that read untrusted content will need a human for any side effect, and amounts will be capped. This page
covers the first part: **provenance marks**, which record in the journal what a run read, and the **taint
policy**, which makes a run that read marked content need an escalation gate for its side effects. Both
**bound consequences; neither prevents prompt injection** ([limits](#what-this-does-not-do), [known limitations](../KNOWN-LIMITATIONS.md#untrusted-content-and-the-taint-policy)).

The declarations and the policy are experimental ([stability](../trust/stability.md)).

## Declaring what is untrusted

Only the host declares provenance. The model never does, and nothing in a result can.

<!-- docsnip: setup ctx context.Context; model agent.Model; j *agent.Journal; retriever agent.Retriever; returns error -->
```go
read := agent.MustFunc("read_email", "read an email", func(ctx context.Context, in struct{ ID string }) (string, error) {
	return "...", nil
}, agent.Untrusted("inbound-email"))

a, err := agent.New(model, j, agent.WithTools(read),
	agent.WithRetrieval(retriever, 3, agent.RetrievalUntrusted("kb")))
if err != nil {
	return err
}
// The run's own input came from a customer: say so.
_, err = a.Run(ctx, "run-1", agent.UserText("refund order 4417"), agent.WithUntrustedInput("customer-chat"))
```

- `agent.Untrusted(source)` on a tool: every result the tool returns is recorded with the source name
  (`Safety.Untrusted` of the result record: the normal result, an error, a denial and a saga's failure record).
  Split a tool whose results are sometimes trusted into two tools.
- `agent.WithUntrustedInput(source)` on a run: the input is untrusted. `agent.WithTrustedInput(reason)` says the
  input is the host's own. Either is journaled in `run:start`'s settings; a later drive that passes another is
  `ErrConfig`, and the two cannot be combined.
- `agent.RetrievalUntrusted(source)` on a retrieval layer: the mark is recorded with the layer's `@retrieval/<n>` step.
- `agent.TaintExempt(reason)` declares a tool whose calls need no escalation gate in a tainted run. It requires a
  reason, is part of the declarations digest, and is recorded on the `gate-reason` record of every call it let
  through, so the audit shows each call that ran because of it. It does nothing without a taint policy.

- `agent.Trusted(reason)` on a tool (experimental): the tool's results are the host's own (a table the host writes,
  a service it runs), and why. The reason is required and printable, it is part of the declarations digest, and a
  trusted tool writes no mark. It exists so that a policy can tell a tool someone classified from one nobody did
  (see `RequireProvenance` below). A tool cannot be both `Untrusted` and `Trusted`; `ToolSpec.Trusted` is the same
  declaration, and a wrapper that embeds the inner tool or implements `Unwrap` must carry it like `Untrusted`.

- `agent.RaisesSafety(reason)` on a wrapper (experimental): a wrapper that embeds a tool or implements `Unwrap` may not
  declare a `Safety` safer than a tool inside it (side effect, then `Idempotent`, then `ReadOnly`), because the
  agent acts on the wrapper's `Safety`: a `ReadOnly` wrapper over a side-effecting tool is not gated by the taint
  policy, and its call is re-run on resume. `agent.New` refuses it with `ErrConfig`, naming the wrapper, the tool
  and both levels, unless the wrapper declares `RaisesSafety(reason)` (the reason follows the rules of `Trusted`
  and is part of the declarations digest). With it, the wrapper is treated by the level it declared, so use it only
  when the wrapper does make the call safer. `ToolSpec.RaisesSafety` is the same declaration.

A source given through `agent.WithSafety(agent.Safety{Untrusted: "..."})` is the same declaration, never ignored; giving two different sources (this and the option) is `ErrConfig` at registration.

A source name is a short printable string (at most 128 bytes). A tool that wraps an `Untrusted` tool must declare
it too. Registration refuses a wrapper that embeds the inner tool or implements `Unwrap` and does not carry
the declaration; it does not see a wrapper that does neither (the same holds for an approval gate, a timeout or
caps), so the declaration is the wrapper author's duty there.
A wrapper that declares `Trusted` over a tool that declares `Untrusted` would drop the taint, and is refused the same way (the error names both); one that drops an inner `Trusted` is only unclassified, which `RequireProvenance` refuses.

## What is recorded, and what is not

Marks are recorded whether or not the agent has a taint policy, so the journal shows what a
run read before a policy exists. A run that states nothing records nothing: its records are byte for byte as
before, and older binaries ignore the new members.

Not covered: a result `ResolveHalt` injects (it has no access to the registered tools), a plan flow node (a tool
that declares `Untrusted` or `TaintExempt` is refused as a node), and any text a tool or system prompt injects
that the host did not declare.

## The taint policy

<!-- docsnip: setup ctx context.Context; model agent.Model; j *agent.Journal; read agent.Tool; refund agent.Tool; verifiers agent.ApproverVerifierFor; returns error -->
```go
escalation := &agent.ApprovalPolicy{Need: 2, Approvers: []string{"risk-1", "risk-2"}, ExcludePrincipal: true}
a, err := agent.New(model, j, agent.WithTools(read, refund),
	agent.WithApproverVerifiers(verifiers),
	agent.WithTaintPolicy(agent.TaintPolicy{Gate: escalation}))
if err != nil {
	return err
}
_, err = a.Run(ctx, "run-1", agent.UserText("refund order 4417"))
```

`agent.WithTaintPolicy(agent.TaintPolicy{Gate: policy})` makes the agent enforce the policy on the runs it
starts. After a model turn has been shown content marked untrusted, every call of a tool that can have a side
effect, requested by a **later** turn, needs the escalation gate `policy.Gate` (an ordinary `ApprovalPolicy`: for a
bank, an m-of-n of signed decisions with `ExcludePrincipal`). With `Refuse: true` and no `Gate` the call is not
run and not asked; the model reads an error result.

- **Both gates apply.** If the tool has its own gate, the call runs only when that gate has passed and the
  escalation gate has passed. There is no "weaker wins" rule.
- **The escalation gate is a second instance on the call**, under the subject `<call>#taint` (a tool-use ID with
  a `#` is refused, `ErrToolUseIDInvalid`). Its approvers decide it and nobody else: a decision signed for the
  call does not count on the instance, and the reverse, and an approver of the tool's own gate is not an approver
  of the escalation gate unless the policy names them. `bide approve`, `bide approve -list`, `bide view`, the
  approvals intake and the notifier show it as a gate of its own, with the call's tool and arguments.
- **The gate that decides it is the one its pause note records**, never the tool's live gate or the process's
  current policy. The approvals intake serves an escalation subject only when its `gate-reason` record agrees with
  its pause note; a journal that disagrees is not served.
- **The reason is shown, not signed.** The engine records why it held the call (`gate-reason:<call>`): the
  sources of untrusted content its turn read, with where and at which journal position. `bide approve -list`,
  the decision screen, `bide view` and the notifier's `approval.why` show it for the `#taint` instance (a cap
  reason, for a call a cap rule holds, shows the rule and this call's figures as of the ask). It is the
  application's record: no gate reads it and the signed decision does not bind it.
- **An approval clears nothing.** It is a decision on one call. The next side-effecting call is gated again, and
  no operation clears taint on a run.
- **Not gated:** a `ReadOnly` tool, a tool declared `TaintExempt`, a call requested in the same turn as the read
  (the model had not yet seen the content), and a saga's rollback.
- **The policy is a property of the run.** A run that started with no policy is never gated for its marks, even
  after the host adds a policy. A run that started under a policy records its digest in `run:start`; a process
  with no policy that drives it refuses its side-effecting calls (`ErrConfig`), and a process whose policy has
  another digest holds a call that was asked under the old one (`ErrConfig`). Roll the whole fleet before enabling a
  policy.
- **The `gate-reason` record** says why the call was held: the sources and positions of the marks and the
  escalation policy's digest. It is evidence for the approver and the auditor, and never authority: the engine
  recomputes the reasons and enforces the union, so a record can add a requirement and never remove one.
- **`SeparateApprovers` (experimental) makes the second look a second person's.** Without it nothing stops one
  approver approving both a call and its escalation instance, so the intended second look can be one person. With
  `agent.TaintPolicy{..., SeparateApprovers: true}`, the instance's tally leaves out the approvers whose decisions
  the call's own gate counted (the terminal tally the own gate recorded: approvers and deniers alike), the gate
  records them as `excluded_by_sibling`, and when too few approvers are left to reach the Gate's `Need`, the
  instance is denied, not left waiting. It is checked at `New` too: a tool that can be escalated (it can have a side
  effect, and is not `TaintExempt`) with an own gate must have an m-of-n one whose approvers share no ID with the
  policy's Gate (compared as written, ignoring ASCII case, before any requester exclusion), or `New` is `ErrConfig`
  naming the tool and the shared IDs; a `SingleApproval` own gate, or a `SingleApproval` or `Refuse` policy, is
  `ErrConfig` with it set, since a single decision has no approver ID to keep apart. The own gate is never
  recounted because of the instance. It is in the declarations digest only when set, and `bide doctor -diff` reports
  its removal as weakened. The approvals intake refuses a decision by an approver the own gate counted, `bide approve` refuses it and
  lists them as unable to approve, the run viewer shows them excluded, and the notifier does not ask them.
  `bide audit verify-approvals -separate-approvers` with `-call` the instance (and
  `audit.VerifyApprovals` with `ApprovalPolicy.SeparatedFrom`) recounts an instance with the same exclusion, from the
  own gate's tally that the evidence carries. It works on approver IDs, not people: two IDs of one person are not
  told apart, and in a certificate-bound gate the decider is the approver name, not the certificate's subject.
- **`RequireProvenance` (experimental) leaves nothing unclassified by omission.** With
  `agent.TaintPolicy{..., RequireProvenance: true}`, a tool that declares neither `Untrusted` nor `Trusted`
  (`TaintExempt` does not count) is `ErrConfig` at `agent.New`, naming the tool, and so is a tool that declares
  both. A new `Run`, `Send` or `SendOnce` that states neither `WithUntrustedInput` nor `WithTrustedInput` is
  `ErrConfig` at its first drive and writes nothing; a sub-agent's run (its input is the parent's call) and a
  resume (the input was stated when the run started) are not asked. The setting is part of the declarations
  digest only when it is on, so an existing policy's digest does not change. It declares; it does not verify: a
  `Trusted` reason is the host's word, and a tool that returns outside text under `Trusted` taints nothing.
- [Plan flows](plan-flows.md) take the same policy, with a rule of their own (see [Plan flows](#plan-flows)).
  Children and sessions inherit taint, as follows.

## Children, sub-agents and sessions

Taint follows the content, not the run that read it.

- **Down.** A sub-agent started by a call of a tainted turn starts tainted: its `run:start` records the marks the
  calling turn was shown (`ext.taint_inherited`, one entry per source, sorted), so its first turn is gated as the
  parent's would have been. The marks pass exactly when the parent run is under taint; a child of a run that is
  not under taint inherits nothing. A later drive of the child that finds another list is `ErrConfig`.
  The child need not share the parent's store.
- **Up.** If a run the call drove ended with any mark, whether it read one itself or started tainted, the parent's
  `tool_result` for the call carries the mark `subagent:<tool name>` (cut to 128 bytes on a character boundary), and
  the parent's next turn is tainted. This covers a sub-agent tool and a tool that starts a programmatic sub-run
  (`WithSubRuns`, `RunInfo.SubRunFor`) and returns its answer. Without it a tainted child's answer would launder
  what it read. The child's journal is read from the child's own store. If it cannot be read, a call that succeeded
  records nothing and stops with a storage error (a resume reads again), and a call that failed is marked, since
  its error text may carry what the child read.
- **Along a session.** A session keeps the questions and answers of its turns, so the answer of a tainted turn
  reaches the next turn as the assistant's own words. A turn that ended with marks records them in the session
  journal (`taint/turn/<n>`, the union of all marks so far), and every later turn starts with the union of all the
  session's taint records, so a record can add marks and never remove any. A clean turn
  after a tainted one does not clear it: the later turn inherits the marks and ends with them. The turns of a
  session before a policy joined are not retro-marked.
- A tool that runs an agent on a context of its own (a fresh `context.Background()`, a run ID that is not a
  sub-run of the call) is an ordinary tool that returns content from elsewhere: bide cannot see the run, so the
  answer is not marked. Declare `Untrusted` on such a tool.
- A tool that calls a sub-agent is itself a call: unless it is `ReadOnly` or `TaintExempt`, a tainted turn needs
  the escalation gate to delegate. Attenuation (a narrower grant, [delegation](delegation.md)) is separate and does
  not clear taint.
- Not covered: a saga's rollback re-run of a sub-agent, and the answer a `ResolveHalt` injects.

## Plan flows

A plan flow has no model turn to taint, so it has a rule of its own, the run-level one ([design](../design/flow-taint.md)).
It is experimental, like the rest of the policy, and Go only: a config cannot give a flow a policy.

- **Declare.** A node says what its results carry with `plan.Untrusted(source)` (a Step, Model, Join or Tool
  block; the `untrusted` key of a config node adds a source and never removes one; a tool's own `Untrusted` is
  taken), `plan.Trusted(reason)` or `plan.TaintExempt(reason)`. `Builder.WithTaintPolicy` (or
  `plan.WithLoadedTaintPolicy` for a flow loaded from a config) gives the flow an `agent.TaintPolicy`: an escalation
  `Gate`, or `Refuse`, and optionally `RequireProvenance` (every node declares `Untrusted` or `Trusted`, and a new
  run states where its input came from). A declaration without a policy gates nothing, as for an agent.
- **The rule.** What a node has read is the run's journal in order, not an edge: the sources of the nodes whose
  results are recorded before it, the flow's input when `WithUntrustedInput` names it, the marks the run inherited
  (below), and the marks a run its nodes started brought up. A node that can have a side effect (not `ReadOnly`),
  not `TaintExempt`, and run after any of those needs the policy's escalation gate on a second subject,
  `node:<name>:taint` (`node:iter:<n>:<name>:taint` in a loop body, so a loop asks once per iteration), after its
  own gate and before its attempt claim; under `Refuse` it is refused instead (`plan.ErrTaintRefused`, no node
  record, attempt or approval request; the run stays unfinished until `agent.Cancel`). An approval clears
  nothing: the next such node is gated again. A node does not read itself, nor the marks its own child brought up.
- **It is a property of the run.** The run records the policy, the exempt nodes and the declared sources in
  `run:start`; a drive under other declarations (a weaker policy included) is `ErrConfig`, naming both digests. Roll
  the whole fleet before enabling a policy, and cancel a run, or restore its declarations, to change them in flight.
  A flow with no policy that drives a run that started under one runs only nodes that cannot have a side effect.
  `Flow.Conform` recomputes each node's obligation from the journal and reports a node that ran without its gate.
- **Nested flows.** A flow a node's body starts inside a run under taint (or a tool of a tainted agent that runs one)
  inherits the status and the marks the node had read, as an agent child does (`ext.taint_inherited`, one entry per
  source). With a policy of its own it enforces it; with none it may run only nodes that cannot have a side effect,
  and a flow with such a node is refused (`ErrConfig`) before it writes anything. It must use its parent's journal.
- **Up.** A nested flow sends the marks it learned to the node that started it, as `taint-mark:` records in the
  parent's journal, so the nodes after that node are gated: the sources its own nodes declare, and what its own
  nested flows brought up, not the marks it inherited. It sends after each node's record, again when a drive
  replays the node, and for a run that finished, so a crash does not lose a mark. `ResolveHalt` of a node folds
  in the marks of the flows the node started (`taint-child:` records name them), then records the operator's result.
- **Agents and flows across.** An agent a node's body runs hands the marks it read itself up to the node, before the
  node is recorded. A flow run as a tool of an agent marks the tool's result `subflow:<tool>` when it ended with a mark
  (its own, or inherited), so the agent's next side effect is gated. A `ResolveHalt` of a node that drove an agent
  cannot read the agent's marks (its tools' declarations are not in the journal): it marks the node
  `subagent:<node name>`, which over-taints, on that path only. A flow with no policy records the sources its nodes
  declare in `run:start` (`taint_sources`, only when it declares some), so it marks what it reads too; they gate
  nothing in it, and a drive by a flow that declares other sources is `ErrConfig`. A run that began outside taint sends them up too when a tainted node's body drives it.
- **Separated approvers.** A flow policy with `SeparateApprovers` is held to the same rule: `Build` refuses a node
  that can be escalated whose own gate is one decision, or shares an approver ID with the policy's Gate, and the
  node's instance (`node:<name>:taint`) leaves out the approvers its own gate counted.
- **Not built:** a `Trusted` block that brings nothing up (a node that declares its results trusted still takes the
  marks its nested flows and agents bring up).

## Reviewing the declarations

Taint and cap declarations are Go. Nothing at runtime changes them, so a change is a code change, and the
review of it needs to see the declarations, not the diff of the Go around them. The agent dumps its declarations
as a document, and `bide doctor` reports on one and diffs two.

<!-- docsnip: setup a *agent.Agent; w io.Writer; returns error -->
```go
doc, err := a.Declarations()
if err != nil {
	return err
}
_, err = w.Write(doc)
return err
```

Commit the document next to the code (a test that writes it, and fails when it differs, keeps it current), then:

```text
bide doctor -declarations declarations.json
bide doctor -diff main/declarations.json declarations.json
```

- **The document** (`bide.declarations.v1`) holds the taint policy (the escalation gate, or refuse), and every
  registered tool with whether it can have a side effect, its own gate, its `Untrusted` source, its `TaintExempt`
  reason, its `Trusted` reason, its `RaisesSafety` reason, whether it is `Idempotent` (a side effect that is retry-safe; the document carries `tracks_idempotent` to say so), its cap rules, and the policy's `require_provenance`. Its `digest` is the `taint_policy_digest` the runs of that agent record in `run:start`,
  and the reader recomputes it from the document: a document whose digest is not the digest of what it declares is
  refused, and so is one with a member this version does not know (a document from a newer version).
- **A flow's declarations** are read from the same two commands. The file is the `plan.RegistryDescription` of
  version 2 that a program writes from its `Registry` (see [Describing a Registry](plan-flows.md#describing-a-registry)),
  with the flow's policy set from `plan.DescribeTaintPolicy`; the reader tells the two kinds apart, and refuses to
  compare one with the other. It has no digest. `-declarations` lists a side-effecting block when the description
  gives no policy, each block with a source and no policy, each exempt block and the notes on the gate, as for an agent. `-diff` applies the
  same rules to blocks: a removed `Untrusted` source, a new `TaintExempt`, a gate lowered or
  `require_provenance` turned off is `WEAKENED`. A policy that was not given to a description is not "no policy", so
  when either description gives none the diff reports one weakened line that the policies cannot be compared. A description of version 1 carries no taint declarations: `-declarations` says
  taint is not known, and `-diff` reports one weakened line when either side lacks them, so regenerate the baseline once.
  The description cannot show a run's input provenance, which is a run option.
- **`-declarations`** prints what a reader should look at: a side-effecting tool in an agent with no taint
  policy, an `Untrusted` tool in an agent with no policy (the marks are recorded and nothing is gated), each
  `TaintExempt` tool with its reason, each tool that declares `RaisesSafety` with its reason, each cap-carrying tool, and an escalation gate that is one decision, needs
  fewer than two, or does not exclude the requester. These are notes, not failures: the exit status is 0 for a
  document that reads. When the policy does not require provenance, each tool that declares neither `Untrusted` nor
  `Trusted` is listed as unclassified: it may be trusted or forgotten, and the declaration tells them apart.
- **`-diff old new`** needs both documents from the same generation: a dump from before `tracks_idempotent` (v0.24.0
  and earlier) says nothing about which tools are `Idempotent`, so when one is, the diff reports one weakened line
  asking you to regenerate the baseline with this version (regenerate it once after upgrading), and an older `bide
  doctor` refuses a new document. Otherwise it lists every change as `WEAKENED`, `changed` or `strengthened`, and exits 1 if anything is
  weakened, so a pipeline can compare a branch against `main`. Weakened: a removed `Untrusted` (also when a `Trusted` replaces it); `require_provenance` turned off (turned on is strengthened); a new or reworded
  `TaintExempt`; a new `RaisesSafety` (removing it is strengthened, rewording it is changed); any tool that becomes safer to re-run (side effect to `Idempotent`, or either to `ReadOnly`), since a resume then re-runs its calls (the move back is strengthened); the policy removed, or `refuse` turned into a gate; a lower `need`; a requester no longer
  excluded; an m-of-n gate turned into one decision; a raised cap limit; a removed cap rule; a *renamed*
  cumulative rule (its name is its ledger, so the window starts again from zero); a refuse turned into a gate; a
  window of a day where it was an hour; a rule that applies to fewer tools; a key added to a cumulative rule (one
  shared total becomes one per key); a tool that becomes `ReadOnly` (taint stops gating its calls); an approver
  removed or replaced in a gate, even when `need` is still met (a rotation is a change a person must approve; an
  approver only added is strengthened); and, because they cannot be compared and a pipeline should stop, a cap
  rule that changes kind (per call and cumulative), currency, exponent or key, and cap lines that do not parse on
  either side. Anything the report cannot judge as safe is `WEAKENED`. Changed, for a person to read and not
  stopping the pipeline: a longer or shorter gate timeout, another untrusted source name, a `Trusted` declared, removed or reworded, another amount member, the
  same approvers in another order (the order is part of the digest).
  Cap lines are compared by their meaning, through the cap grammar, not as text.
- **The digest and the diff must agree.** Different digests with no difference in the documents mean the digest
  covers something the document does not carry; equal digests with a difference in what the digest covers mean
  the reverse. Either is an error (exit 1), not a report.
- **Strict reading.** The document is read so that what the text shows is what it holds: a duplicate member, a
  member spelled in another case, an unknown member, data after the document and a document over 16 MiB are
  refused, and so is a gate no valid agent has (an approver listed twice, which would count twice toward `need`, a
  `need` outside the approvers, no approvers).
- **What it cannot see.** The report is only as good as the dump. It does not show what happens at run time: a
  sub-agent tool, or a tool that starts a programmatic sub-run, marks its answer when a run it drove read untrusted
  content (`subagent:<tool>`), and a child inherits its parent's marks, so a delegating tool taints its caller
  whatever the dump says. A tool's side-effect flag and its own gate are in the document but not in its digest. A tool whose wrapper hides its spec, a tool
  that runs an agent on a context of its own, and anything the host does not register with the agent are not
  in it. A difference in a tool's side-effect flag or its own gate is listed as "not in the digest".

## What this does not do

This bounds what an injected request can do. It does not detect or prevent one.

1. A tool that returns outside text and is not declared untrusted taints nothing. The marks are as good as the
   declarations.
2. A human who approves a convincing injected request is a successful attack.
3. A `ReadOnly` tool is not gated. A "read-only" fetch whose URL carries stolen data is an exfiltration channel;
   declare a tool that can reach an attacker-chosen destination as a side effect.
4. The final answer of a tainted run goes to whoever reads it. Taint gates tools, not text.
5. A tainted run that writes to a memory or index the next run trusts launders the content.
6. Models that read the same hostile text can all be steered; taint does not look inside a model's words.
7. Text the host injects outside the journal (a system prompt, a model middleware) is invisible to the marks.
8. An input is trusted unless the entry point passes `WithUntrustedInput`.
9. Amount caps, a separate control, bound magnitude and are not part of this policy.
10. The policy covers agent runs only; see [plan flows](plan-flows.md).

The approval volume of a deployment whose agents read outside text and write will rise. That is the intended
effect; `TaintExempt` is the relief valve, and it is a code change visible in the declarations digest.
