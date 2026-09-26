# Getting started

## Prerequisites

- Go 1.27 or newer.
- For examples that call a live model, an API key (any OpenAI-compatible endpoint works via `WithBaseURL`). Several examples run with no key at all (see below).

## Install

```
go get github.com/dayna/go-agents
```

## Your first agent

An agent is a model, a durable store, and some tools. The loop runs to a final answer; tool results and model turns are journaled so a crashed run resumes without repeating work.

```go
package main

import (
	"context"
	"fmt"
	"os"

	agent "github.com/dayna/go-agents"
	"github.com/dayna/go-agents/model/openai"
)

type WeatherArgs struct {
	City string `json:"city" desc:"city name"`
}

func main() {
	model := openai.New(os.Getenv("OPENROUTER_API_KEY"),
		openai.WithBaseURL("https://openrouter.ai/api/v1"),
		openai.WithModel("openai/gpt-4o-mini"),
	)
	weather := agent.Func("get_weather", "Get the weather for a city",
		agent.Safety{ReadOnly: true},
		func(_ context.Context, in WeatherArgs) (string, error) {
			return "72F and clear in " + in.City, nil
		})

	a := agent.New(model, agent.NewMemStore(), weather)
	out, err := a.Run(context.Background(), "run-1", "Weather in SF?")
	if err != nil {
		panic(err)
	}
	fmt.Println(out)
}
```

`NewMemStore` is in-memory; for durable resume across restarts use the SQLite store (`store/sqlite`) or the Postgres store (`store/postgres`) for high availability. See the [durable steps guide](guides/durable-steps.md) and [debugging and recovery](guides/debugging.md).

## Run an example

The repository ships runnable examples in `examples/`. Some need a live model; several run offline with a small inline model, which is the fastest way to see the durable mechanics:

```
# needs a key (any OpenAI-compatible endpoint)
OPENROUTER_API_KEY=sk-... go run ./examples/smoke

# no key needed: durable mechanics with an inline model
go run ./examples/interrupt      # human-in-the-loop pause and resume
go run ./examples/signals        # deliver an external event into a waiting run
go run ./examples/recover        # durable resume after a simulated crash
go run ./examples/observability  # OTel spans printed to stdout, with token-to-cost
```

See [examples/README.md](../examples/README.md) for the full list.

## Building the repository

This is a multi-module workspace (`go.work`): the core is one module and adapters such as `trace`, `mcp`, `store/*`, and the `govern/*log` backends are their own modules. To build or test everything with the module versions pinned in each `go.mod` rather than the workspace, set `GOWORK=off`:

```
GOWORK=off go build ./...
GOWORK=off go test ./...
```

See [module structure](reference/module-structure.md) for the layout and [CONTRIBUTING.md](../CONTRIBUTING.md) for the full build and test conventions.

## Where next

- The vocabulary: [Concepts](CONCEPTS.md).
- The precise guarantee and its bounds: [Guarantee](GUARANTEE.md), [Known limitations](KNOWN-LIMITATIONS.md).
- The full map: the [docs index](README.md).
