# Getting started

## Prerequisites

- Go 1.27 or newer (the core uses generic methods). If `go version` is older, upgrade or set `GOTOOLCHAIN=go1.27.0`.
- For examples that call a live model, an API key (any OpenAI-compatible endpoint works via `WithBaseURL`). Several examples run with no key at all (see below).

## Install

In a module of your own (`go mod init example.com/hello` in a new directory):

```
go get github.com/bide-ai/bide/agent@latest
```

Get the `agent` package, not the module root: `go get github.com/bide-ai/bide@latest` records the module but not the dependencies of its packages, and the first build then stops with `missing go.sum entry`. Equally, write your code first and run `go mod tidy`, which adds every package you import. The core module holds the `agent` package and everything else in the core (the model adapters, `plan`, `audit`). The adapter modules (`store/sqlite`, `store/postgres`, `mcptools` (published as `mcp` up to v0.10.0), `trace`, `codec/gcf`, `govern`, and the `govern/*log` backends) are published from v0.8.0 on, tagged with the same version as the core, so you add the ones you use the same way, for example `go get github.com/bide-ai/bide/store/sqlite@latest`. To build against unreleased code instead, clone the repository and use its `go.work` (see [Building the repository](#building-the-repository)).

## Your first agent

An agent is a model, a durable store, and some tools. The loop runs to a final answer; tool results and model turns are journaled so a crashed run resumes without repeating work.

The core package is `agent`, imported from `github.com/bide-ai/bide/agent` (as the block below shows).

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/model/openai"
)

type WeatherArgs struct {
	City string `json:"city" desc:"city name"`
}

func main() {
	// Any OpenAI-compatible endpoint; get a key at openrouter.ai, or see "No API key?" below.
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
		log.Fatal(err)
	}
	a, err := agent.New(model, journal, agent.WithTools(weather))
	if err != nil {
		log.Fatal(err)
	}
	out, err := a.Run(context.Background(), "run-1", agent.UserText("Weather in SF?"))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(out.Message.Text())

	// The same run ID again: the run is finished, so its journaled answer comes back, and the
	// model is not called.
	again, err := a.Run(context.Background(), "run-1", agent.UserText("Weather in SF?"))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(again.Message.Text() == out.Message.Text()) // true
}
```

Run it with `OPENROUTER_API_KEY=sk-... go run .`. It prints the model's answer (a sentence such as "It's 72F and clear in SF.", in the model's words), then `true`: the second `Run` of `run-1` read the answer back from the journal instead of calling the model or the tool again. That is how a crashed or repeated run resumes: steps already journaled are not repeated.

### No API key? Use agenttest

`agent/agenttest` has a scripted model that plays back turns you write, so the same agent runs offline (in tests, too). Swap it in for the OpenAI model: the first turn calls the tool, the second answers.

<!-- docsnip: setup journal *agent.Journal; weather agent.Tool -->
```go
model := agenttest.NewScriptedModel(
	agenttest.ToolTurn("c1", "get_weather", `{"city":"SF"}`),
	agenttest.TextTurn("It's 72F and clear in SF."),
)
a, err := agent.New(model, journal, agent.WithTools(weather))
```

With it the program prints `It's 72F and clear in SF.` and then `true`, with no network access.

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

The repository ships runnable examples in `examples/`. Some need a live model; several run offline with a small inline model, which is the fastest way to see the durable mechanics. The examples in the core module run from anywhere, without a clone:

```
# needs a key (any OpenAI-compatible endpoint)
OPENROUTER_API_KEY=sk-... go run github.com/bide-ai/bide/examples/smoke@latest

# no key needed: durable mechanics with an inline model
go run github.com/bide-ai/bide/examples/interrupt@latest   # human-in-the-loop pause and resume
go run github.com/bide-ai/bide/examples/signals@latest     # deliver an external event into a waiting run
go run github.com/bide-ai/bide/examples/recover@latest     # durable resume after a simulated crash
```

From a clone (`git clone https://github.com/bide-ai/bide && cd bide`), the same examples run as `go run ./examples/<name>`. A few examples are modules of their own, to keep their dependencies out of the core (`approval`, `govern`, `mcp`, `observability`, `plan`): run them from the clone, where the workspace (`go.work`) resolves them, for example `go run ./examples/observability` (OTel spans printed to stdout, with token-to-cost), or `cd examples/observability && go run .`.

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
