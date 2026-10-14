# Recipe: invoice approval

## When to use it

A model turns unstructured input (an invoice, an email, a form) into structured fields, a person
checks them, and only then does a system of record change. The model's output is a proposal, never an
action; the person's approval is what lets the write happen.

## Run it with no API key

```sh
mkdir invoice && cd invoice
go mod init invoice
```

Save the program below as `main.go`, then:

```sh
go mod tidy
go run .
```

The first run extracts the invoice and stops at the approval. The program exits; nothing is lost.

```go
// Command invoice extracts an invoice with a model, waits for one human to approve it, then
// records it. It runs with no API key; set OPENAI_API_KEY to use a real model.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/model/openai"
	"github.com/bide-ai/bide/plan"
	"github.com/bide-ai/bide/store/sqlite"
)

type Invoice struct {
	Vendor string `json:"vendor"`
	Total  int    `json:"total"`
}

type Booked struct {
	Vendor string `json:"vendor"`
	Total  int    `json:"total"`
	Ref    string `json:"ref"`
}

const text = "ACME Corp invoice #881: 3 widgets, total due 450 USD, net 30."

// model is scripted by default, so the recipe needs no API key or network.
func model() agent.Model {
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		return openai.New(key, openai.WithModel("gpt-4o-mini"))
	}
	return agenttest.NewScriptedModel(agenttest.TextTurn(`{"vendor": "ACME Corp", "total": 450}`))
}

// record stands in for your ledger. It is a side effect, so bide runs it at most once.
func record(_ context.Context, inv Invoice) (Booked, error) {
	fmt.Printf("ledger: booked %d for %s\n", inv.Total, inv.Vendor)
	return Booked{Vendor: inv.Vendor, Total: inv.Total, Ref: "INV-881"}, nil
}

func main() {
	ctx := context.Background()
	store, err := sqlite.Open("invoice.db")
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	j, err := agent.NewJournal(store)
	if err != nil {
		log.Fatal(err)
	}

	b := plan.New[string, Booked]("invoice").WithModel(model())
	ex := b.Model[string, Invoice]("extract",
		`Extract the vendor and the total (an integer) from this invoice as a JSON object with "vendor" and "total": {{.}}`)
	rec := b.Step("record", record, plan.Approval(agent.SingleApproval()))
	b.Edge(ex, rec)
	flow, err := b.Build()
	if err != nil {
		log.Fatal(err)
	}

	approve := len(os.Args) > 1 && os.Args[1] == "approve"
	for {
		out, err := flow.Run(ctx, j, "invoice-881", text)
		var pend *agent.ApprovalPending
		switch {
		case errors.As(err, &pend) && !approve:
			fmt.Printf("waiting for approval of %s\nrun: go run . approve\n", pend.Args)
			return
		case errors.As(err, &pend):
			// Here the person is whoever runs "approve". In production an approval UI calls this,
			// later and from another process.
			if err := agent.Approve(ctx, j, pend.RunID, pend.ToolUseID, true); err != nil {
				log.Fatal(err)
			}
		case err != nil:
			log.Fatal(err)
		default:
			fmt.Printf("done: %+v\n", out)
			return
		}
	}
}
```

```sh
go run .            # extracts, then waits for a decision
go run . approve    # records the decision, books the invoice
go run .            # reads the finished run back: nothing runs twice
```

```text
waiting for approval of {"vendor":"ACME Corp","total":450}
run: go run . approve
ledger: booked 450 for ACME Corp
done: {Vendor:ACME Corp Total:450 Ref:INV-881}
```

Run `go run .` a second time before approving and it waits again without calling the model again:
the extraction is already in the journal. `agent.Approve` is what your approval screen calls, from any
process that can open the journal.

## With a real model

Set `OPENAI_API_KEY` and delete `invoice.db` (a finished node is read back from the journal, so the new
model would not be called for an extraction already recorded):

```sh
rm -f invoice.db invoice.db-wal invoice.db-shm
OPENAI_API_KEY=sk-... go run .
```

`model()` is the only place that changes. For another provider use `model/anthropic` or `model/gemini`,
or point `openai.New` at any OpenAI-compatible endpoint with `openai.WithBaseURL` (see
[Models](../guides/models.md)). A model that supports structured output (`model/openai`,
`model/gemini`) is sent the `Invoice` type's JSON schema; the prompt also names the fields, for one
that does not.

## What the viewer shows

```sh
bide view -db invoice.db
```

The run list shows `invoice-881` as waiting for approval after the first run and completed after the
second. The run page draws `extract` (a model node) then `record` (a step), with the approval gate
between them: one decision, unsigned, approved. The timeline lists the model turn, the approval
request with the exact input being approved, the decision, and the booking. Because the `extract`
node is read-only, a crash during the model call simply asks the model again; `record` is a side effect,
so a crash during it halts instead of booking twice.

## Make the approval stronger

`agent.SingleApproval()` records one unsigned decision. For named approvers who sign the exact input,
use an `agent.ApprovalPolicy` with `Need` and `Approvers` and give the flow their verifiers: see the
[refund recipe](refund-approval.md). Those approvers decide from a terminal with `bide approve`; a
single-approver gate takes it too, unsigned, with no `-as` or `-key`.

## See also

[Plan flows](../guides/plan-flows.md), [Human approval](../guides/hitl-approval.md), and
[`examples/plan`](https://github.com/bide-ai/bide/tree/main/examples/plan).
