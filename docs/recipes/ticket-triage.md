# Recipe: support ticket triage

## When to use it

A model decides where work goes, and the work itself has side effects. The decision is a read-only
model call, which is safe to repeat; the actions it routes to (paging someone, sending a reply) are
side effects that must happen once. Keeping the two apart is the point: bide re-asks the model after a
crash, but never re-runs a page.

## Run it with no API key

```sh
mkdir triage && cd triage
go mod init triage
```

Save the program below as `main.go`, then:

```sh
go mod tidy
go run .
```

```go
// Command triage classifies a support ticket with a read-only model call, then routes it to a side
// effect: page the on-call engineer for an outage, otherwise send an automatic reply. It runs with
// no API key; set OPENAI_API_KEY to use a real model.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/model/openai"
	"github.com/bide-ai/bide/plan"
	"github.com/bide-ai/bide/store/sqlite"
)

type Ticket struct {
	ID   string `json:"id"`
	Body string `json:"body"`
}

type Triage struct {
	Ticket   string `json:"ticket"`
	Category string `json:"category"` // "outage" or "other"
	Summary  string `json:"summary"`
}

type Action struct {
	Ticket string `json:"ticket"`
	Did    string `json:"did"`
}

// model is scripted by default, so the recipe needs no API key or network.
func model() agent.Model {
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		return openai.New(key, openai.WithModel("gpt-4o-mini"))
	}
	return agenttest.NewScriptedModel(agenttest.TextTurn(
		`{"ticket": "T-1001", "category": "outage", "summary": "checkout returns 500 for every customer"}`))
}

// page and reply stand in for your pager and your help desk. Each is a side effect, so bide runs
// it at most once per ticket, and a crash while it ran halts the run instead of paging twice.
func page(_ context.Context, t Triage) (Action, error) {
	fmt.Printf("pager: paged on-call about %s: %s\n", t.Ticket, t.Summary)
	return Action{Ticket: t.Ticket, Did: "paged on-call"}, nil
}

func reply(_ context.Context, t Triage) (Action, error) {
	fmt.Printf("helpdesk: sent an automatic reply on %s\n", t.Ticket)
	return Action{Ticket: t.Ticket, Did: "replied"}, nil
}

func main() {
	ctx := context.Background()
	store, err := sqlite.Open("triage.db")
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	j, err := agent.NewJournal(store)
	if err != nil {
		log.Fatal(err)
	}

	b := plan.New[Ticket, Action]("triage").WithModel(model())
	// A Model node is read-only: a crash during it just asks the model again.
	classify := b.Model[Ticket, Triage]("classify",
		`Classify this support ticket. Answer with a JSON object with "ticket" (the string "{{.ID}}"), "category" ("outage" if the service is down for many customers, otherwise "other") and "summary" (one sentence).

Ticket {{.ID}}: {{.Body}}`)
	pageNode := b.Step("page", page)
	replyNode := b.Step("reply", reply)
	b.Switch(classify,
		plan.When(func(t Triage) bool { return t.Category == "outage" }, pageNode),
		plan.Else(replyNode),
	)
	flow, err := b.Build()
	if err != nil {
		log.Fatal(err)
	}

	out, err := flow.Run(ctx, j, "ticket-1001", Ticket{ID: "T-1001", Body: "Checkout shows a 500 error for everyone since 09:00."})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("done: %s\n", out.Did)
}
```

```text
pager: paged on-call about T-1001: checkout returns 500 for every customer
done: paged on-call
```

Run `go run .` again: it prints `done: paged on-call` and nothing is paged twice. The run is read back
from the journal.

The scripted model always classifies the ticket as an outage. To see the other branch, change the
`category` in the scripted answer to `other` and delete `triage.db` (and its `-wal` and `-shm`
files): the reply node runs and the page node does not.

## With a real model

Set `OPENAI_API_KEY`, delete `triage.db`, and run again:

```sh
rm -f triage.db triage.db-wal triage.db-shm
OPENAI_API_KEY=sk-... go run .
```

The model answers with the `Triage` JSON, and the `switch` reads its `category`. Because a model may
answer anything, route on a closed set of values and put the fallback in `plan.Else`, as here: an
unexpected category lands on the safe branch.

## What the viewer shows

```sh
bide view -db triage.db
```

The run page draws the flow with the path taken: `classify` is done, the arm that matched is `page`,
and the branch not taken (`reply`) is marked not reached. The timeline shows the model turn, the switch choice (`switch:classify`), and the page's attempt marker and result.
`bide view -config` on the same flow, if you write it as YAML, colors `classify` as read-only and
`page` and `reply` as side effects.

## What a crash does

A crash during `classify` runs it again on the next start. A crash during `page`, after the pager was
called and before the result was journaled, halts the run (`*agent.OutcomeUnknown`) rather than page
twice. The switch choice is journaled, so a resumed run takes the branch it took before even if the
model would now answer differently.

## See also

[Plan flows](../guides/plan-flows.md) (`Switch`, `Model` nodes), [Reliability](../guides/reliability.md)
for retrying and hedging the model call.
