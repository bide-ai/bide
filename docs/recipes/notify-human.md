# Recipe: get notified when a run needs a human

## When to use it

A run that halts on an unknown outcome (`*agent.OutcomeUnknown`) or waits at an approval gate
(`*agent.ApprovalPending`) stops until a person acts, and nothing in bide tells that person. If nobody is
watching the process, the run just sits. This recipe posts one JSON message to a webhook (a Slack
incoming webhook, say) the first time each such event is seen, and not again on every recovery pass.

## Where bide reports it

Three places see a run stop for a person, and the recipe hooks all three with one function:

- **The error `Run` returns.** `a.Run` (and `Resume`, `RunTyped`, the plan flow's `Run`) returns the typed
  error to its caller.
- **The `Resumer` that `RecoverLoop` calls.** `agent.RecoverLoop` re-drives unfinished runs after a
  restart and on every pass, and a halted or waiting run comes back each time. `WithRecoverErrors` does
  **not** receive it: a run waiting for a person is a pause, not a failure, so the loop counts it as
  recovered. Report from the Resumer instead, which sees the error before the loop classifies it.
  `WithRecoverErrors` still gets genuine failures (a store or model error), which the recipe ignores.
- **A `MemWaker` wakeup.** A run that sleeps and wakes into a halt returns the error to the callback you gave
  `agent.NewMemWaker`. `Start(ctx, every, onError)` reports the failures of the wakeup itself.

Because the loop finds a halted run again on every pass, the notifier must post each event once. It does
that with a key built from the run, the step, and (for a halt) the time of the attempt marker, which is
the same on every report of the same halt.

## Run it with no API key

```sh
mkdir notify && cd notify
go mod init notify
```

Save the program below as `main.go`, then:

```sh
go mod tidy
go run .
```

```go
// Command notify tells a person when a bide run needs them. It posts one JSON message to a webhook
// for each run that halts on an unknown outcome or waits for an approval, from the three places
// bide reports them, and posts each event once.
//
//	go run .                                  # a local receiver stands in for Slack and prints what it gets
//	NOTIFY_URL=https://hooks.slack.com/services/... go run .
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// dbFile is the database `bide view` opens; set it to the file your service uses.
const dbFile = "runs.db"

// message is the JSON body of one notification. Slack's incoming webhooks read the text field, so
// pointing NOTIFY_URL at one posts the sentence; the other fields are for a receiver of your own.
type message struct {
	Text   string `json:"text"`
	RunID  string `json:"run_id"`
	Kind   string `json:"kind"` // "outcome_unknown" or "approval_pending"
	Step   string `json:"step"` // the tool call or Step that needs a person
	Reason string `json:"reason"`
	View   string `json:"view"` // how to look at the run
}

// classify turns an error from Run, RecoverLoop or a Waker into the event it reports, if it reports
// one. key is the same for every report of the same event, so a halt that RecoverLoop finds again on
// every pass is one event. Anything else (a store error, a model error) is not for a person here.
func classify(err error) (key string, m message, ok bool) {
	var halt *agent.OutcomeUnknown
	var gate *agent.ApprovalPending
	switch {
	case errors.As(err, &halt):
		step := halt.Op.ID
		if halt.Op.ToolName != "" {
			step = halt.Op.ToolName + " (" + halt.Op.ID + ")"
		}
		return fmt.Sprintf("halt/%s/%s/%d", halt.RootRunID, halt.Op.ID, halt.AttemptedAt.UnixMilli()),
			message{
				Text:   fmt.Sprintf("Run %s halted: the outcome of %s is unknown. Check the system it called, then resolve it.", halt.RootRunID, step),
				RunID:  halt.RootRunID,
				Kind:   "outcome_unknown",
				Step:   step,
				Reason: "the process stopped after the call started and before its result was recorded (" + string(halt.Cause) + ")",
				View:   "bide view -db " + dbFile + " -run " + halt.RootRunID,
			}, true
	case errors.As(err, &gate):
		return fmt.Sprintf("gate/%s/%s", gate.RootRunID, gate.ToolUseID),
			message{
				Text:   fmt.Sprintf("Run %s is waiting for approval of %s.", gate.RootRunID, gate.ToolName),
				RunID:  gate.RootRunID,
				Kind:   "approval_pending",
				Step:   gate.ToolName + " (" + gate.ToolUseID + ")",
				Reason: "the tool needs a human decision before it runs",
				View:   "bide view -db " + dbFile + " -run " + gate.RootRunID,
			}, true
	}
	return "", message{}, false
}

// notifier posts each event once per process. An event is marked sent only after the receiver
// answers 2xx, so a failed POST is tried again the next time the event is reported.
type notifier struct {
	url     string
	client  *http.Client
	mu      sync.Mutex
	sent    map[string]bool
	reports int // how many times an event was reported, duplicates included
}

func newNotifier(url string) *notifier {
	return &notifier{url: url, client: &http.Client{Timeout: 10 * time.Second}, sent: map[string]bool{}}
}

// Handle is the one function all three hooks call. It ignores an error that is not an event.
func (n *notifier) Handle(err error) {
	key, m, ok := classify(err)
	if !ok {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock() // held across the POST: a second report of the same event waits, then finds it sent
	n.reports++
	if n.sent[key] {
		return
	}
	body, _ := json.Marshal(m)
	resp, perr := n.client.Post(n.url, "application/json", bytes.NewReader(body))
	if perr != nil {
		log.Printf("notify: %v (will try again when the event is reported again)", perr)
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		log.Printf("notify: receiver answered %s (will try again)", resp.Status)
		return
	}
	n.sent[key] = true
}

// crashOnce wraps a store and fails the first write of the named record. The charge tool has
// already run by then, so this is what a crash between the call and its recorded result leaves
// behind. A real service has no such wrapper: it just crashes.
type crashOnce struct {
	agent.Store
	name string
	done atomic.Bool
}

func (c *crashOnce) Unwrap() agent.Store { return c.Store } // keeps the store's run listing and leases visible

func (c *crashOnce) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	if name == c.name && c.done.CompareAndSwap(false, true) {
		return agent.Entry{}, false, errors.New("simulated crash before the result was recorded")
	}
	return c.Store.Insert(ctx, runID, name, data)
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The receiver. With NOTIFY_URL set it is your Slack or webhook; without, a local server that
	// prints each message and counts them.
	url := os.Getenv("NOTIFY_URL")
	var posts atomic.Int32
	if url == "" {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			fmt.Printf("receiver: POST %s\n", b)
			posts.Add(1)
		}))
		defer srv.Close()
		url = srv.URL
	}
	n := newNotifier(url)

	store := &crashOnce{Store: agent.NewMemStore(), name: agent.ToolResultStep("c1")}
	j, err := agent.NewJournal(store)
	if err != nil {
		log.Fatal(err)
	}

	// charge is a side effect (the default Safety): bide runs it at most once.
	charge := agent.MustFunc("charge", "charge the customer's card", func(context.Context, struct{}) (string, error) {
		fmt.Println("charge: the card was charged")
		return "charged", nil
	})
	model := agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "charge", `{}`), agenttest.TextTurn("done"))

	// Hook 3: a Waker resumes a run whose durable timer is due. Report what the resume returns.
	// No timer is set here, so it stays quiet; in a service this is where a halt after a wakeup
	// would be missed otherwise.
	var a *agent.Agent
	waker := agent.NewMemWaker(func(ctx context.Context, runID string) error {
		_, err := a.Resume(ctx, runID)
		n.Handle(err)
		return err
	})
	waker.Start(ctx, time.Second, n.Handle)
	a, err = agent.New(model, j, agent.WithTools(charge), agent.WithWaker(waker))
	if err != nil {
		log.Fatal(err)
	}

	// Hook 1: the error Run returns. Here the process "crashes" while recording the result.
	_, err = a.Run(ctx, "order-1042", agent.UserText("charge the card"))
	fmt.Println("run:", err)
	n.Handle(err)

	// Hook 2: RecoverLoop re-drives unfinished runs. It does not hand a halt or a pending approval
	// to WithRecoverErrors, because a run waiting for a person is not a failure, so report from the
	// Resumer, which sees every outcome. A halted run comes back on every pass, and the notifier
	// posts the first and ignores the rest.
	resume := agent.ResumeAgent(a)
	watched := func(ctx context.Context, runID string, start agent.RunStart) error {
		err := resume(ctx, runID, start)
		n.Handle(err)
		return err
	}
	loopCtx, stop := context.WithTimeout(ctx, 600*time.Millisecond)
	defer stop()
	_ = agent.RecoverLoop(loopCtx, j, watched,
		agent.WithRecoverInterval(100*time.Millisecond),
		agent.WithRecoverErrors(n.Handle))

	n.mu.Lock()
	fmt.Printf("the halt was reported %d times, and the receiver got %d POST\n", n.reports, posts.Load())
	n.mu.Unlock()
}
```

```text
charge: the card was charged
run: tool "charge": record step "tool:c1" of run order-1042: simulated crash before the result was recorded (storage) (tool)
receiver: POST {"text":"Run order-1042 halted: the outcome of charge (c1) is unknown. Check the system it called, then resolve it.","run_id":"order-1042","kind":"outcome_unknown","step":"charge (c1)","reason":"the process stopped after the call started and before its result was recorded (crashed)","view":"bide view -db runs.db -run order-1042"}
the halt was reported 6 times, and the receiver got 1 POST
```

The `charge` tool ran, then the store failed to record its result: the state a real crash between the
charge and the journal write leaves. `Run` returned a storage error, which is not for a person, so the
notifier ignored it. `RecoverLoop` then re-drove the run on every pass; each pass came back with
`*agent.OutcomeUnknown`, the Resumer reported it each time (the count varies with timing), and the
receiver was posted to once.

## Point it at Slack

Create an incoming webhook for a channel and run the program with its URL:

```sh
NOTIFY_URL=https://hooks.slack.com/services/T000/B000/XXXX go run .
```

Slack posts the `text` field. The other fields are for a receiver of your own: `kind` is
`outcome_unknown` or `approval_pending`, `step` names the tool call or step, and `view` is the command
that opens the run in the [run viewer](../guides/run-viewer.md). Keep the webhook URL in an environment
variable or a secret store, not in the code: anyone who has it can post to the channel. The message
carries the run ID and tool name, never the tool's arguments or results, which can hold card numbers or
personal data. Add detail only after deciding who may read it.

In a service, call `n.Handle` in the same three places as the program does. Do not approve or resolve
from a link in the message: decide in the viewer or with `SubmitDecision` and `agent.ResolveHalt`, where
the decision is signed and checked.

## What it does not promise

The `sent` map is in memory, so this recipe is a notifier, not a guarantee:

- **A restart may notify again.** The map is empty in a new process, so the first pass after a restart
  posts the halt again.
- **A crash between the halt and the POST may drop the notification.** If the process dies after the run
  halted and before the POST left, nobody is told, until a later pass in a process that is running sees
  the halt. A service that runs `RecoverLoop` finds it again; one that only calls `Run` does not.
- **A failed POST is retried only when the event is reported again.** The event is marked sent after a 2xx,
  so a failure is retried on the next pass, but a run that is reported once (the error from `Run`) is not.
- **There is no deadline or escalation.** A message nobody reads is not noticed.

The built-in notifier closes these gaps: its delivery state is kept in an audited log, so it survives restarts and crashes, retries without end, and is itself evidence of when the approvers were told. See the [notifications guide](../guides/notifications.md) and the [design note](../design/notifier.md). This recipe stays as the smallest notifier you can write by hand.

## What the viewer shows

```sh
bide view -db runs.db -run order-1042
```

The run shows as halted (outcome unknown), and its timeline lists the `charge` call with its attempt and
no result. A run waiting for approval shows as waiting, with its gate. The viewer reads the journal, so
it shows these states with or without a notifier.

## What a crash does

A crash anywhere does not change what the run does: `charge` ran at most once, and the halt waits until
`agent.ResolveHalt` records what really happened. A crash of the notifying process only costs a message
(see above); it never causes a second charge.

## See also

[Durable steps](../guides/durable-steps.md), [Debugging and recovery](../guides/debugging.md),
[Human approval](../guides/hitl-approval.md), and the [customer record recipe](customer-record.md), which
produces the halt this one reports.
