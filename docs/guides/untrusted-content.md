# Untrusted content: provenance marks

An agent that moves money reads text it does not control: a customer's email, a web page, a ticket, a
third-party tool's result. That text can tell the model to act. bide does not try to detect that. It
[bounds what an agent can do after it reads such text](../design/untrusted-content-and-amount-caps.md): a run
that read untrusted content will need a human for any side effect, and amounts will be capped. This page
covers the first part, which exists today: **provenance marks**. A mark records, in the journal, what a run
read. It gates nothing yet, and it does not prevent prompt injection; see [known limitations](../KNOWN-LIMITATIONS.md#untrusted-content-marks).

The declarations are experimental ([stability](../trust/stability.md)).

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
- `agent.TaintExempt(reason)` declares a tool whose calls will need no escalation gate in a tainted run. It does
  nothing yet and requires a reason.

A source name is a short printable string (at most 128 bytes). A tool that wraps an `Untrusted` tool must declare
it too, or registration is refused, as for a gate the wrapper would hide.

## What is recorded, and what is not

Marks are recorded whether or not the agent has a taint policy (there is none yet), so the journal shows what a
run read before a policy exists. A run that states nothing records nothing: its records are byte for byte as
before, and older binaries ignore the new members.

Not covered: a result `ResolveHalt` injects (it has no access to the registered tools), a plan flow node (a tool
that declares `Untrusted` or `TaintExempt` is refused as a node), and any text a tool or system prompt injects
that the host did not declare.

## What comes next

The [design](../design/untrusted-content-and-amount-caps.md) adds a taint policy (a human gate for side effects
after a mark, never cleared by an approval), amount caps with a cumulative ledger, and the approver's view of both.
This page will describe each as it lands.
