# Recipe: ambient agent

## When to use it

An agent that is not waiting on a chat: it sleeps until a time, wakes, checks something and acts. A
nightly report, a follow-up three days after a ticket closes, a watcher that scales a queue. The wait
is a journaled timer, not a goroutine blocked in `time.Sleep`, so the process can exit or crash during
it and the run still wakes at the same instant.

## Run it with no API key

```sh
mkdir ambient && cd ambient
go mod init ambient
```

Save the program below as `main.go`, then:

```sh
go mod tidy
go run .
```

```go
// Command ambient is an agent that sleeps, wakes, checks something and acts. The sleep is
// durable: the wake time is journaled, so a process that dies during the wait resumes at the same
// instant. It runs with no API key; set OPENAI_API_KEY to let a real model decide the steps.
//
//	go run .   # sleeps 5 seconds, reads the backlog, scales the queue up
//
// Press Ctrl-C during the sleep and run it again: the clock does not restart.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/model/openai"
	"github.com/bide-ai/bide/store/sqlite"
)

const task = "Wait for the next check. Then read the backlog of the jobs queue. If it is over 100, scale the queue up; otherwise do nothing. Report what you did."

// model is scripted by default, so the recipe needs no API key or network.
func model() agent.Model {
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		return openai.New(key, openai.WithModel("gpt-4o-mini"))
	}
	return agenttest.NewScriptedModel(
		agenttest.ToolTurn("c1", "wait", `{}`),
		agenttest.ToolTurn("c2", "backlog", `{"queue": "jobs"}`),
		agenttest.ToolTurn("c3", "scale_up", `{"queue": "jobs"}`),
		agenttest.TextTurn("The jobs queue was over 100, so I scaled it up."),
	)
}

type Empty struct{}

type Queue struct {
	Queue string `json:"queue"`
}

// wait holds the durable timer. Sleep journals the wake time the first time it is reached and
// pauses the run with *agent.TimerPending until then. Use agent.WaitUntil(ctx, name, t) for an
// absolute time, such as the next 09:00. Sleep needs a retry-safe tool, hence ReadOnly.
var wait = agent.MustFunc("wait", "wait until the next check is due", func(ctx context.Context, _ Empty) (string, error) {
	if err := agent.Sleep(ctx, "tick", 5*time.Second); err != nil {
		return "", err // *agent.TimerPending: the run pauses here
	}
	return "the wait is over", nil
}, agent.WithSafety(agent.Safety{ReadOnly: true}))

// backlog reads the queue depth. It changes nothing, so a resume may run it again.
var backlog = agent.MustFunc("backlog", "read the backlog of a queue", func(_ context.Context, q Queue) (int, error) {
	if n, err := strconv.Atoi(os.Getenv("BACKLOG")); err == nil {
		return n, nil
	}
	return 500, nil
}, agent.WithSafety(agent.Safety{ReadOnly: true}))

// scaleUp stands in for your autoscaler API. It is a side effect (the default Safety), so bide
// runs it at most once, and a crash while it ran halts the run instead of scaling twice.
var scaleUp = agent.MustFunc("scale_up", "add workers to a queue", func(_ context.Context, q Queue) (string, error) {
	fmt.Printf("autoscaler: added workers to %s\n", q.Queue)
	return "scaled up", nil
})

func main() {
	ctx := context.Background()
	store, err := sqlite.Open("ambient.db")
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	j, err := agent.NewJournal(store)
	if err != nil {
		log.Fatal(err)
	}
	a, err := agent.New(model(), j, agent.WithTools(wait, backlog, scaleUp))
	if err != nil {
		log.Fatal(err)
	}

	for {
		res, err := a.Run(ctx, "watch-jobs", agent.UserText(task))
		var timer *agent.TimerPending
		if errors.As(err, &timer) {
			fmt.Printf("sleeping until %s\n", timer.FireAt.Format(time.TimeOnly))
			// This program stays alive and waits. A service would return here and let a Waker
			// (agent.WithWaker), a cron job or agent.RecoverLoop run the agent again when it is due.
			time.Sleep(time.Until(timer.FireAt))
			continue
		}
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println("done:", res.Message.Text())
		return
	}
}
```

```text
sleeping until 09:21:30
autoscaler: added workers to jobs
done: The jobs queue was over 100, so I scaled it up.
```

The agent called `wait`, which called `agent.Sleep`. The first time it ran, `Sleep` journaled the wake
time and the run paused with `*agent.TimerPending`. This program stays alive and sleeps until the wake
time, then runs the agent again, which resumes past the timer.

To see the timer is durable, press Ctrl-C during the five seconds and run `go run .` again: it prints
the same `sleeping until` time, not a new one five seconds from now.

`agent.WaitUntil(ctx, "daily", t)` is the same with an absolute time, such as the next 09:00; compute
`t` in the tool and give each timer in a run its own name.

## With a real model

Set `OPENAI_API_KEY`, delete `ambient.db` and its `-wal` and `-shm` files, and run. The model is given the
task and the three tools, and decides the order: wait, read the backlog, and scale up only if it is
over 100. The scripted model always scales up, whatever `backlog` returns. Set `BACKLOG=20` to give a
real model a quiet day:

```sh
rm -f ambient.db ambient.db-wal ambient.db-shm
BACKLOG=20 OPENAI_API_KEY=sk-... go run .
```

## Let something else wake it

A long-running service should not block in a loop. Give the agent a `Waker` and `Sleep` registers the
wake itself: `agent.WithWaker(w)` with `agent.NewMemWaker`, whose callback calls `a.Resume`. `MemWaker`
lives in the process, so for a service that restarts also run `agent.RecoverLoop`, which finds paused
runs in the journal and drives the ones whose timers are due; or hand the trigger to cron or a queue
that calls `a.Run` with the same run ID. See [Durable steps](../guides/durable-steps.md#durable-timers-sleep--waituntil-and-the-waker)
for the wiring.

## What the viewer shows

```sh
bide view -db ambient.db
```

While the agent sleeps, the run is in progress and its page follows it live. The timeline lists the run
start, the model turn that called `wait`, and `timer:tick` with the journaled wake time. After the wake
it continues with the tool results (`backlog`, then `scale_up`, each with its Safety) and the final
answer, and the run shows as completed.

## What a crash does

A crash during the sleep: the wake time is in the journal, so the next start waits only for what is
left of it. A crash inside `scale_up` after the autoscaler was called halts the run
(`*agent.OutcomeUnknown`) instead of scaling twice; `backlog` and `wait` are read-only, so they simply
run again.

## See also

[Signals and ambient](../guides/signals.md), [Durable steps](../guides/durable-steps.md), and
[`examples/signals`](https://github.com/bide-ai/bide/tree/main/examples/signals).
