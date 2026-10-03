# Getting started

## Prerequisites

- Go 1.27 or newer (the core uses generic methods). If `go version` is older, upgrade or set `GOTOOLCHAIN=go1.27.0`.
- For examples that call a live model, an API key (any OpenAI-compatible endpoint works via `WithBaseURL`). Several examples run with no key at all (see below).

## Install

The core module is published: `go get github.com/bide-ai/bide@latest` gives you the `agent` package and everything else in the core (the model adapters, `plan`, `audit`). The adapter modules (`store/sqlite`, `store/postgres`, `mcptools` (published as `mcp` up to v0.10.0), `trace`, `codec/gcf`, `govern`, and the `govern/*log` backends) are published from v0.8.0 on, tagged with the same version as the core, so you add the ones you use the same way, for example `go get github.com/bide-ai/bide/store/sqlite@latest`. To build against unreleased code instead, clone the repository and use its `go.work` (see [Building the repository](#building-the-repository)).

## Your first agent

An agent is a model, a durable store, and some tools. The loop runs to a final answer; tool results and model turns are journaled so a crashed run resumes without repeating work.

The core package is `agent`, imported from `github.com/bide-ai/bide/agent` (as the block below shows).

```go
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/model/openai"
)

type WeatherArgs struct {
	City string `json:"city" desc:"city name"`
}

func main() {
	model := openai.New(os.Getenv("OPENROUTER_API_KEY"),
		openai.WithBaseURL("https://openrouter.ai/api/v1"),
		openai.WithModel("openai/gpt-4o-mini"),
	)
	weather := agent.MustFunc("get_weather", "Get the weather for a city",
		func(_ context.Context, in WeatherArgs) (string, error) {
			return "72F and clear in " + in.City, nil
		}, agent.WithSafety(agent.Safety{ReadOnly: true}))

	journal, err := agent.NewJournal(agent.NewMemStore())
	if err != nil {
		panic(err)
	}
	a, err := agent.New(model, journal, agent.WithTools(weather))
	if err != nil {
		panic(err)
	}
	out, err := a.Run(context.Background(), "run-1", agent.UserText("Weather in SF?"))
	if err != nil {
		panic(err)
	}
	fmt.Println(out.Message.Text())
}
```

`NewMemStore` is in-memory; for durable resume across restarts use the SQLite store (`store/sqlite`) or the Postgres store (`store/postgres`) for high availability. See the [durable steps guide](guides/durable-steps.md) and [debugging and recovery](guides/debugging.md).

## Configuring an agent

`agent.New(model, journal, opts...)` builds an agent from options, and checks them all when the agent is built: a nil model, a duplicate or reserved tool name, a tool name the agent's model refuses (when the model declares its rule, as the bundled adapters do), a tool whose input schema is not a JSON object, an m-of-n approval policy with no `WithApproverVerifiers`, a negative limit and every other configuration problem is an error wrapping `agent.ErrConfig`, returned by `New`, never by the first run.

<!-- docsnip: setup model agent.Model; journal *agent.Journal; weather agent.Tool -->
```go
a, err := agent.New(model, journal,
	agent.WithTools(weather),
	agent.WithSystemPrompt("You are a concise assistant."),
	agent.WithMaxTurns(8),
	agent.WithSampling(agent.Temperature(0)),
)
```

An agent built this way does not change. `a.With(opts...)` returns a configured copy (a stricter budget for one tenant, an extra tool for one route) and leaves `a` as it was, so both can run at once. For any setting the last value given wins; `WithSystemPrompt` and `WithSystemPromptFunc` share one slot, so the later of the two wins. Build a `[]agent.Option` to choose options conditionally.

Some options apply at more than one scope, and each constructor's type says which: `WithMaxTurns`, for one, is an `agent.AgentRunOption`, a setting for an agent and, passed to `Run`, for a single run; `WithSafety` is an `agent.SafetyOption`, which a tool and a `Step` both take; `WithMaxConcurrency` caps an agent's tool calls and `Parallel`'s tasks alike. An option passed where it does not apply does not compile.

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

This is a multi-module workspace (`go.work`): the core is one module and adapters such as `trace`, `mcptools`, `store/*`, `govern`, and the `govern/*log` backends are their own modules. To build or test everything with the module versions pinned in each `go.mod` rather than the workspace, set `GOWORK=off`:

```
GOWORK=off go build ./...
GOWORK=off go test ./...
```

See [module structure](reference/module-structure.md) for the layout and [CONTRIBUTING.md](../CONTRIBUTING.md) for the full build and test conventions.

## Where next

- The vocabulary: [Concepts](CONCEPTS.md).
- Authoring flows as typed topology instead of plain Go: the [Flows guide](guides/flows.md) (the optional `plan` builder and declarative config).
- The precise guarantee and its bounds: [Guarantee](GUARANTEE.md), [Known limitations](KNOWN-LIMITATIONS.md).
- The full map: the [docs index](README.md).
