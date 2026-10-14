# Recipe: refund with a 2-of-3 approval

## When to use it

A side effect that moves money, and a policy that says who must agree: a refund over a threshold needs
two of three named approvers, each signing the exact refund, and a smaller one does not need anyone.
Use it when you also have to show an auditor, later and offline, that the approvals came first.

## Run it with no API key

`bide init` writes a complete project: a model proposes the refund (scripted, so no key), a gated step
issues it, and a last step notifies. Its gate is 2 of `ops`, `finance` and `risk`.

```sh
bide init refund && cd refund
go mod tidy
go run .
```

```text
paused before node:refund: 0 of 2 approvals (pending ops, finance, risk)
ops approved {"order":"order-42","amount":120,"reason":"arrived damaged"}
paused before node:refund: 1 of 2 approvals (pending finance, risk)
finance approved {"order":"order-42","amount":120,"reason":"arrived damaged"}
done: refunded: arrived damaged (120)
refunds the provider holds: 1
wrote evidence.json, approver-keys.json and keys/log.pub
```

The run paused before the refund, two approvers signed it, it resumed and refunded once. Run
`go run .` again: it reads the finished run back from the journal and refunds nothing.

## Add the threshold

Only a refund over 100 should wait for approval. Replace `flow.yaml` with a flow that routes on the
amount the model proposed. A `switch` sends the large refund through the gated node and the small one
to a node with no gate:

```yaml
version: 1
flow: refund
in: main.Order
out: main.Receipt
entry: assess
nodes:
  - {name: assess, block: assess, safety: readonly}
  # Over the threshold: 2 of the 3 named approvers must sign this exact refund first.
  - name: refund
    block: refund
    safety: side_effect
    approval: {need: 2, approvers: [ops, finance, risk]}
  - {name: notify, block: notify, safety: readonly}
  # At or under the threshold: no gate.
  - {name: refund_small, block: refund_small, safety: side_effect}
wiring:
  - switch: assess
    when: [{pred: over_threshold, to: refund}]
    else: refund_small
  - edge: [refund, notify]
assert:
  - every path to refund passes approval >= 2 of 3
```

The `assert` line is policy about the graph, checked without running anything: `bide check flow.yaml`
exits 1 if some path reaches the refund without a 2-of-3 approval. Then register the two blocks the
file names that `main.go` does not have yet, next to the existing `reg.RegisterStep(...)` calls in
`register`:

<!-- docsnip: setup reg *plan.Registry; p provider; type provider struct{}; func (provider) refund(Refund) error; type Refund struct{ Order string; Amount int; Reason string }; type Receipt struct{ Order string; Amount int; Note string }; returns error -->
```go
return errors.Join(
	reg.RegisterStep("refund_small", func(_ context.Context, r Refund) (Receipt, error) {
		return Receipt{Order: r.Order, Amount: r.Amount, Note: "refunded without approval: " + r.Reason}, p.refund(r)
	}),
	reg.RegisterPredicate("over_threshold", func(r Refund) bool { return r.Amount > 100 }),
)
```

`story` ends by writing approval evidence, which exists only for a run that reached the gate. Skip it
on the small path, in the loop where `story` prints `done:`:

<!-- docsnip: setup ctx context.Context; j *agent.Journal; k keys; dir string; out Receipt; type Receipt struct{ Note string; Amount int }; p provider; type provider struct{}; func (provider) refunds() int; type keys map[string]string; func writeEvidence(context.Context, *agent.Journal, keys, string) error; returns error -->
```go
fmt.Printf("done: %s (%d)\nrefunds the provider holds: %d\n", out.Note, out.Amount, p.refunds())
if out.Amount <= 100 { // no gate on this path, so there is no approval evidence to write
	return nil
}
return writeEvidence(ctx, j, k, dir)
```

Start over (`rm -f run.db run.db-wal run.db-shm refunds.log evidence.json approver-keys.json`) and run
`go run .`: the scripted model proposes 120, so it pauses for approvals as before. Change `"amount": 120`
in `model()` to `40`, start over again, and the refund goes through with no pause.

## With a real model

In `model()` in `main.go`, set `OPENROUTER_API_KEY` or `OPENAI_API_KEY`, uncomment the marked lines
and the `model/openai` import, and start over first (a finished run is read back from the journal, so a
new model would not be called). The model is sent the `Refund` type's JSON schema, so it answers with
that JSON. Any OpenAI-compatible endpoint works through `openai.WithBaseURL`; `model/anthropic` and
`model/gemini` are the native adapters (see [Models](../guides/models.md)).

## What the viewer shows

```sh
bide view -db run.db -evidence evidence.json -pubkey keys/log.pub -approver-keys approver-keys.json
```

The run page draws the flow with the path taken: `assess`, then either `refund` and `notify`, or
`refund_small`. For the gated path it shows the approval gate: which approvers decided, how many of
the 2 needed, and (with `-approver-keys`) whether each signature verifies. Before the second approval
the run shows as waiting for approval and the gate shows who is still pending. `bide view -config
flow.yaml` draws the config as a risk map: "2 side-effect steps, 1 gated, 1 ungated" (the ungated one
is the small refund, by design), the refund's gate (2 of 3: ops, finance, risk), and the `assert` line
holding. The `safety:` lines in the YAML are what let it color the nodes without your `Registry`.

## Prove it offline

```sh
bide audit verify-approvals -evidence evidence.json -pubkey keys/log.pub -call node:refund \
  -need 2 -approvers ops,finance,risk -approver-keys approver-keys.json
```

Exit status 0 means two of the three named approvers signed this exact refund before it ran. The
auditor needs only the evidence file and public keys. `bide audit verify-evidence` checks the evidence
package as a whole.

## Approve from a terminal

`bide approve` is the command an approver runs instead of writing code. `bide keygen -out ops.key -as ops`
makes a key and prints the `approver-keys.json` entry; `bide approve -db run.db -list` shows the calls that
wait; `bide approve -db run.db -run <run> -call <call> -as ops -key ops.key -approver-keys
approver-keys.json` shows the refund as the journal holds it, with the SHA-256 the signature binds, asks
you to confirm, and records the signed decision, leaving a receipt. See [Deciding from the
terminal](../guides/hitl-approval.md#deciding-from-the-terminal).

## What a crash does

A crash while the run waits for approvals loses nothing: the requests and decisions are in the journal,
and the next `go run .` counts them. A crash inside `refund`, after the provider was called and before
the result was journaled, halts the run on the next start (`*agent.OutcomeUnknown`) instead of refunding a
second time; see [Update a customer record safely](customer-record.md) for resolving it. The scaffold
can kill itself on purpose: `BIDE_CRASH=notify go run .`, then `go run .` again.

## See also

[Human approval](../guides/hitl-approval.md), [Plan flows](../guides/plan-flows.md),
[Audit](../guides/audit.md), and the runnable [`examples/approval`](https://github.com/bide-ai/bide/tree/main/examples/approval).
