# Untrusted content: provenance marks and the taint policy

An agent that moves money reads text it does not control: a customer's email, a web page, a ticket, a
third-party tool's result. That text can tell the model to act. bide does not try to detect that. It
[bounds what an agent can do after it reads such text](../design/untrusted-content-and-amount-caps.md): a run
that read untrusted content will need a human for any side effect, and amounts will be capped. This page
covers the first part: **provenance marks**, which record in the journal what a run read, and the **taint
policy**, which makes a run that read marked content need an escalation gate for its side effects. Both
**bound consequences; neither prevents prompt injection** ([limits](#what-this-does-not-do), [known limitations](../KNOWN-LIMITATIONS.md#untrusted-content-marks)).

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

A source given through `agent.WithSafety(agent.Safety{Untrusted: "..."})` is the same declaration, never ignored; giving two different sources (this and the option) is `ErrConfig` at registration.

A source name is a short printable string (at most 128 bytes). A tool that wraps an `Untrusted` tool must declare
it too. Registration refuses a wrapper that embeds the inner tool or implements `Unwrap` and does not carry
the declaration; it does not see a wrapper that does neither (the same holds for an approval gate, a timeout or
caps), so the declaration is the wrapper author's duty there.

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
- The policy does not cover [plan flows](plan-flows.md) nodes. Children and sessions inherit taint, as follows.

## Children, sub-agents and sessions

Taint follows the content, not the run that read it.

- **Down.** A sub-agent started by a call of a tainted turn starts tainted: its `run:start` records the marks the
  calling turn was shown (`ext.taint_inherited`, one entry per source, sorted), so its first turn is gated as the
  parent's would have been. The marks pass exactly when the parent run is under taint; a child of a run that is
  not under taint inherits nothing. A later drive of the child that finds another list is `ErrConfig`.
  The child need not share the parent's store.
- **Up.** If a sub-agent's run ended with any mark, whether it read one itself or started tainted, the parent's
  `tool_result` for the call carries the mark `subagent:<tool name>` (cut to 128 bytes on a character boundary), and
  the parent's next turn is tainted. Without it a tainted child's answer would launder what it read. The child's
  journal is read from the child's own store.
- **Along a session.** A session keeps the questions and answers of its turns, so the answer of a tainted turn
  reaches the next turn as the assistant's own words. A turn that ended with marks records them in the session
  journal (`taint/turn/<n>`, the union of all marks so far), and every later turn starts with them. A clean turn
  after a tainted one does not clear it: the later turn inherits the marks and ends with them. The turns of a
  session before a policy joined are not retro-marked.
- A tool that calls a sub-agent is itself a call: unless it is `ReadOnly` or `TaintExempt`, a tainted turn needs
  the escalation gate to delegate. Attenuation (a narrower grant, [delegation](delegation.md)) is separate and does
  not clear taint.
- Not covered: a saga's rollback re-run of a sub-agent, and the answer a `ResolveHalt` injects.

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
10. v0.23.0 covers agent runs only; see [plan flows](plan-flows.md).

The approval volume of a deployment whose agents read outside text and write will rise. That is the intended
effect; `TaintExempt` is the relief valve, and it is a code change visible in the declarations digest.
