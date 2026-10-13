# Recipe: update a customer record safely

## When to use it

A write to a system you do not control: change an email address, charge a card, send a message. If the
process dies after the write but before it learns the answer, running the write again could duplicate
it, and doing nothing could lose it. bide's answer is an attempt marker and a halt, so a person (or a
reconciler) decides with the facts in hand.

## How it works

`Journal.Step` records an **attempt marker** before it runs your function, then the result after. On a
resume:

- a recorded result is returned and the function does not run;
- an attempt marker with no result means the outcome is unknown, so the step **halts**
  (`*agent.OutcomeUnknown`) instead of running again. That is the default, a *side effect*;
- a step declared `Idempotent` (a repeat is a no-op downstream, because your write dedupes on a
  business key) or `ReadOnly` simply runs again.

## Run it with no API key

```sh
mkdir customer && cd customer
go mod init customer
```

Save the program below as `main.go`, then:

```sh
go mod tidy
go run .
```

```go
// Command customer updates a customer's email address once, whatever crashes. The write is a
// durable step: bide records an attempt marker before it runs, so a crash while it ran is never
// answered by writing again.
//
//	go run .                       # update the record
//	BIDE_CRASH=1 go run .          # the process dies right after the write, before bide records the result
//	go run .                       # the resume halts: the step's outcome is unknown
//	go run . resolve               # check the record yourself, then tell bide what happened
//	go run . -idempotent           # the same crash with a write that is safe to repeat: it just re-runs
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/store/sqlite"
)

const runID = "customer-7-email"

// Customer is the record. The file customer.json stands in for your database.
type Customer struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	// Applied lists the request IDs already applied, so a repeated request is a no-op.
	Applied []string `json:"applied"`
}

func load() Customer {
	c := Customer{ID: "7", Email: "old@example.com"}
	if b, err := os.ReadFile("customer.json"); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	return c
}

func save(c Customer) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile("customer.json", b, 0o644)
}

// setEmail is the write. With a request ID it is idempotent: applying the same request twice
// changes nothing the second time. A plain write (no ID) is not.
func setEmail(email, requestID string) error {
	c := load()
	for _, id := range c.Applied {
		if id == requestID && requestID != "" {
			return nil // already applied
		}
	}
	c.Email = email
	c.Applied = append(c.Applied, requestID)
	if err := save(c); err != nil {
		return err
	}
	if os.Getenv("BIDE_CRASH") != "" {
		fmt.Println("crash: the process dies after the write, before the result is journaled")
		os.Exit(3)
	}
	return nil
}

func main() {
	idempotent := flag.Bool("idempotent", false, "declare the write safe to repeat")
	flag.Parse()
	ctx := context.Background()
	store, err := sqlite.Open("customer.db")
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	j, err := agent.NewJournal(store)
	if err != nil {
		log.Fatal(err)
	}

	// The default Safety is a side effect: it runs at most once, and an unknown outcome halts.
	safety := agent.Safety{}
	requestID := ""
	if *idempotent {
		safety = agent.Safety{Idempotent: true}
		requestID = runID // the business key the downstream dedupes on
	}
	got, err := j.Step(ctx, runID, "update-email", func(context.Context) (string, error) {
		return "new@example.com", setEmail("new@example.com", requestID)
	}, agent.WithSafety(safety))

	var halt *agent.OutcomeUnknown
	switch {
	case errors.As(err, &halt) && flag.Arg(0) == "resolve":
		// A person has looked at the record: the write did happen, so the step's result is the
		// email. The next run continues past the step without writing again.
		c := load()
		fmt.Printf("customer.json says the email is %s\n", c.Email)
		if err := agent.ResolveHalt(ctx, j, halt.Ref(), agent.Outcome{Result: c.Email}); err != nil {
			log.Fatal(err)
		}
		fmt.Println("resolved: run again to finish")
	case errors.As(err, &halt):
		fmt.Printf("halted: %v\n", halt)
		fmt.Println("check customer.json, then: go run . resolve")
	case err != nil:
		log.Fatal(err)
	default:
		fmt.Printf("email is now %s\n", got)
	}
}
```

The write here is a JSON file standing in for your database. First the normal path, then the crash:

```sh
BIDE_CRASH=1 go run .    # the write happens, then the process dies before the result is journaled
go run .                 # the resume halts
go run . resolve         # you check the record, then tell bide what happened
go run .                 # the run continues past the step without writing again
```

```text
crash: the process dies after the write, before the result is journaled
halted: resume halted: step "update-email" has unknown outcome and is not retry-safe; ...
check customer.json, then: go run . resolve
customer.json says the email is new@example.com
resolved: run again to finish
email is now new@example.com
```

The halt message ends with what to do (check whether the step took effect, record that with
`agent.ResolveHalt`, then run again); the exact words differ between versions. `resolve` is where a
person or a reconciler looks at the real system (the file here, your database or the provider's
dashboard in production) and records what is true: `agent.Outcome{Result: ...}` is the step's result as
if it had returned it. If the write had not happened, you would not record the new email as done: record the failure
instead, or start a new run under a new run ID.

## The idempotent version

When the downstream can dedupe, the halt is unnecessary. Declare the step `Idempotent` and make the
write safe to repeat by tagging it with a key that identifies this request:

```sh
rm -f customer.db customer.db-wal customer.db-shm customer.json
BIDE_CRASH=1 go run . -idempotent    # the write happens, then the process dies
go run . -idempotent                 # the step runs again; the repeat changes nothing
```

`setEmail` records the request ID it applied (`customer.json` lists it in `applied`), so the repeat is a
no-op. In production this is a unique request key, an upsert, or `UPDATE ... WHERE version = N`. Only
Go code can declare a step idempotent, and the claim is yours: bide trusts it. If you cannot make the
write safe to repeat, leave the default and keep the halt.

## With a real model

This recipe has no model. When a model decides *what* to write, put it in a read-only node before this
step (the [invoice recipe](invoice-approval.md) and the [triage recipe](ticket-triage.md) show how), and
keep the write itself in a default or idempotent step like this one.

## What the viewer shows

```sh
bide view -db customer.db
```

After the crash and the failed resume, the run page has a **Halts** panel: "Outcome unknown: step
`update-email`", its attempt marker (`attempt:step:update-email`) live with no result and the time it was
attempted, and the `agent.ResolveHalt(...)` call that clears it, with the run ID, the step and the cause
filled in. This recipe uses a bare `Journal.Step`, so the run has no `run:start` record and the list
shows it as not started; a halt inside an agent run or a plan flow shows up under that run's state as
halted: outcome unknown. After `resolve` the timeline shows the recorded outcome.

## See also

[Durable steps](../guides/durable-steps.md) (the attempt marker, `Safety`, reconciling a halt
automatically with `WithMinHaltAge`), [Debugging and recovery](../guides/debugging.md), and
[`examples/recover`](https://github.com/bide-ai/bide/tree/main/examples/recover).
