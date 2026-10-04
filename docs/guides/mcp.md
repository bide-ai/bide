# Model Context Protocol integration

The `mcptools` package (`github.com/bide-ai/bide/mcptools`) adapts a **Model Context Protocol** server's tools into `agent.Tool`.
Your agent is the MCP client/host; an MCP server is a *runtime* source of tools whose
schemas are only known at connect time. `Tools()` lists a connected session's tools and
wraps each one so the agent core can call it like any native tool. It is built on the
official SDK, `github.com/modelcontextprotocol/go-sdk`.

The package is deliberately thin: two entry points (`Connect` and `Tools`), a handful of options
(`TrustAnnotations`, `WithSafety`, `WithApproval`, `WithCallTimeout`, `WithMaxResultBytes`,
`WithMaxDescriptionBytes`, `WithElicitation`, `DeclineElicitation`, `WithToolListChanged`,
`WithClientInfo`), and an internal adapter. It does
not embed a server, spawn processes, or manage transports for you; you bring a
`mcp.Transport` (stdio, in-memory, streamable HTTP, ...) and the package turns a connected
session into agent tools.

## What it does

- **Connect to an MCP server as a dynamic tool source.** `Connect` is a convenience over
  the SDK's client + transport: it builds a client and opens a session on the transport
  you pass. Callers who already hold a `*mcp.ClientSession` can skip it.
- **Discover tools at runtime.** `Tools` lists a connected session's tools, following the
  pagination cursor in full so a server that splits its tools across several pages is never
  silently truncated, and wraps every one. Because an MCP tool's schema is only known at
  connect time, each wrapped tool follows the untyped `json.RawMessage` path rather than a Go
  struct: its `Spec().Input` is the server's `InputSchema` as raw JSON for the `schema`
  package to dialectize per provider.
- **Refuse a malformed tool list.** The server's tool list is untrusted input. `Tools` fails
  with an error wrapping `agent.ErrProtocol` if a tool's name is outside the MCP grammar (1 to
  128 of `A-Z a-z 0-9 _ - .`), so no control character, space, or Unicode lookalike reaches your
  logs, journal, or approval prompts; if two tools share a name; or if a tool's input schema is
  not a JSON Schema object of type `"object"`. The MCP grammar is wider than the providers':
  Anthropic and OpenAI accept `^[a-zA-Z0-9_-]{1,64}$` (no dots or colons, at most 64
  characters), and Gemini accepts `^[a-zA-Z_][a-zA-Z0-9_.:-]{0,63}$`. Each bundled adapter
  declares its rule (`agent.ToolRules`), so `agent.New` and `Agent.With` refuse, with
  `agent.ErrConfig` naming the tool, a name the agent's model cannot take: a dotted name builds
  for Gemini and is refused for OpenAI or Anthropic. A server tool named like one of your own tools
  (or like a tool from another server) does not replace it: `agent.New` refuses the clash with
  `agent.ErrConfig`, so the model's call never reaches the wrong tool.
- **Give each tool an `agent.Safety`.** Every MCP-sourced tool gets side-effect-safe
  durable resume. By default each is treated as a side effect; for a server you trust, its
  annotations decide.

## Safety mapping

MCP tool annotations are hints, and the spec says to treat them as untrusted unless they come
from a trusted server. A server that labelled a destructive tool read-only would otherwise have
it re-run on resume, retried by `ToolRetry`, and cached by `ToolCache`. So by default every tool
from `Tools` has the zero `agent.Safety`: it runs at most once, and halts the run on an
unknown-outcome resume.

For a server you trust to label its tools, pass `TrustAnnotations()`:

<!-- docsnip: setup ctx context.Context; import mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"; session *mcpsdk.ClientSession -->
```go
tools, err := mcptools.Tools(ctx, session, mcptools.TrustAnnotations())
```

Each tool's `Spec().Safety` is then derived from the MCP tool's `Annotations` block:

| MCP annotation | Derived `agent.Safety` | Resume behavior |
|---|---|---|
| `readOnlyHint == true` | `Safety{ReadOnly: true}` | always safe to re-run after a crash |
| `idempotentHint == true` | `Safety{Idempotent: true}` | safe to retry |
| destructive or **unannotated** | `Safety{}` (the zero value) | halts the run on an unknown-outcome resume rather than risk firing a side effect twice |

### Per-tool safety and approval gates

`WithSafety(name, safety)` sets one tool's `agent.Safety` (`ReadOnly` or `Idempotent`) from the
host side, in place of the default and of the server's annotations (trusted or not).
`WithApproval(name, policy)` gives one tool a human approval gate, 1-of-1
(`agent.SingleApproval()`) or m-of-n, so it pauses and resumes exactly like a local tool; a nil or
invalid policy fails `Tools` with `agent.ErrConfig`:

<!-- docsnip: setup ctx context.Context; import mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"; session *mcpsdk.ClientSession -->
```go
tools, err := mcptools.Tools(ctx, session,
	mcptools.WithApproval("transfer", agent.SingleApproval()),
	mcptools.WithApproval("wire", &agent.ApprovalPolicy{Need: 2, Approvers: []string{"alice", "bob", "carol"}}),
	mcptools.WithSafety("search", agent.Safety{ReadOnly: true}), // retry-safe on your word, not the server's
)
```

The run returns `*agent.ApprovalPending` before the server sees the call; record the decision
with `agent.Approve` (or `agent.SubmitDecision` for a quorum) and run it again. `Tools` fails with
`agent.ErrConfig` if the server does not list a tool you named (with either option), so a misspelt
gate never leaves the real tool ungated.

Each tool's `agent.ToolSpec` (its `Spec()`) carries the rest of what the server lists: `Title`
is the tool's `title`, or its annotations' title if that is empty; `Output` is its `outputSchema`
(a declared output schema that is not an object schema fails `Tools` with `agent.ErrProtocol`, as
an input schema does); `Timeout` is the `WithCallTimeout` value.

A trusted server that changes a tool's annotations cannot make a call already in flight
retry-safe after the fact: a call that fired as a side effect and lost its result halts the
resume even if the server now labels the tool read-only.

`ReadOnly` wins if both hints are set: a read-only tool has no side effect to double-fire.
An absent `Annotations` block is treated as the destructive default per the MCP spec, which
is the conservative choice for resume. Whether to trust the annotations is the policy decision for a whole server; `WithSafety`
(above) decides for one tool.

## Limits

A server's answers are untrusted input, and each one costs every later turn: a tool
description is sent to the model with every request, and a result is journaled and sent back
on every later turn of the run. `Tools` sets two caps by default. Each one refuses what is
over it with an error; neither one truncates.

| Limit | Default | Over the limit | Option |
|---|---|---|---|
| Tool result | `DefaultMaxResultBytes`, 1 MiB of JSON | `Call` fails with `ErrResultTooLarge` (wraps `agent.ErrTool`) | `WithMaxResultBytes(n)` |
| Tool description | `DefaultMaxDescriptionBytes`, 8 KiB | `Tools` fails with `agent.ErrProtocol` | `WithMaxDescriptionBytes(n)` |

- **1 MiB for a result.** 1 MiB of text is about 250,000 tokens, more than most models' whole
  context window, so a larger result cannot be used as it is, and the journal would hold it for
  the life of the run. The tool did run, so an oversized result is a definite failure whose
  error says so ("tool ran, but its result is N bytes"); the model is not told the call did not
  happen. An `isError` result is held to the same limit.
- **8 KiB for a description.** A description written for a model to read is a few sentences to
  a page. 8 KiB (about 2,000 tokens) leaves room for a detailed one, while a server cannot add
  megabytes to every request or hide long instructions aimed at the model in it.

Pass `n <= 0` to remove either limit.

**Per-call timeout.** `WithCallTimeout(d)` bounds each call by `d`, on top of the run's context,
and sets the tool's `ToolSpec.Timeout`, so the agent runs the call, tool middleware included,
under that deadline too (a plan flow, which calls the tool directly, still gets the tool's own).
There is no default: without it a call waits as long as the run's context allows. A result that
arrives after the deadline is recorded. A call that fails after it may still be running on the
server, so its outcome is unknown (`agent.ErrToolOutcomeUnknown`, see below), and a side effect
records nothing and halts on resume rather than run again. That includes an error the server
reports once the deadline has passed (a late JSON-RPC error): the agent cannot tell a late known
failure from a late answer to a call that took effect, so it takes the safe side and halts.

**Unknown outcomes are the safe side.** Only two failures are known not to have run the tool:
a JSON-RPC error from the server, and a call on a session already closed. Every other transport
failure counts as an unknown outcome. That includes a streamable HTTP server that cannot be
dialled at all: the SDK does not report a refused connection distinctly from one that dropped
mid-request, so a side effect whose server is down halts the run for confirmation instead of
failing outright. Confirm with `agent.ResolveHalt` once you know the call did not reach the
server.

## Optional client capabilities

`Connect` takes options that wire three more MCP client capabilities. All are opt-in; the
default `Connect(ctx, transport)` behaves exactly as before.

- **`WithElicitation(resolver)`** registers a resolver for the server's `elicitation/create`
  requests: the server asks the host for structured input (a message plus a JSON schema).
  Setting it advertises the elicitation capability to the server. The same resolver answers
  both standalone elicitation and elicitation embedded in a tool's multi-round-trip input
  requests (SEP-2322, the mechanism the current protocol uses), because the SDK routes both
  to it. `DeclineElicitation` is a safe default that declines every request.

  **Durability boundary.** The resolver answers elicitation live, within the interaction that
  requested it; it is not a durable, resume-across-crash pause. Durability is inherited at the
  tool-call boundary (a tool trusted as read-only or idempotent re-runs, and so re-elicits, on
  resume, while any other tool halts on an unknown-outcome resume). For input that must survive a
  crash and resume in a fresh process, use the native `agent.Interrupt` in a native tool.

- **`WithToolListChanged(callback)`** fires the callback when the server notifies that its
  tool list changed. Re-call `Tools` on the session to pick up the new set. The agent core
  takes a fixed tool set per run, so a changed set applies to the next run you build, not one
  already in flight.

- **`WithClientInfo(name, version)`** overrides the client name and version reported to the
  server (default `"bide"` / `"0.1.0"`).

## Exported API

<!-- docsnip: api github.com/bide-ai/bide/mcptools; import mcp "github.com/modelcontextprotocol/go-sdk/mcp" -->
```go
// Connect builds a client and opens a session on the given transport. Options
// wire optional client capabilities (elicitation, tools-list-changed, client info).
func Connect(ctx context.Context, transport mcp.Transport, opts ...Option) (*mcp.ClientSession, error)

// Tools lists a connected session's tools (paginated in full), each wrapped as an agent.Tool.
// Each is a side effect unless TrustAnnotations is passed.
func Tools(ctx context.Context, session *mcp.ClientSession, opts ...ToolsOption) ([]agent.Tool, error)

// TrustAnnotations maps a trusted server's annotations onto agent.Safety.
func TrustAnnotations() ToolsOption

// WithSafety sets one tool's agent.Safety (ReadOnly, Idempotent), overriding annotations.
func WithSafety(name string, s agent.Safety) ToolsOption

// WithApproval gates one tool on human approval (agent.SingleApproval or an m-of-n policy).
func WithApproval(name string, p *agent.ApprovalPolicy) ToolsOption

// Limits (see Limits): a per-call timeout (no default) and caps on results and descriptions.
func WithCallTimeout(d time.Duration) ToolsOption
func WithMaxResultBytes(n int) ToolsOption         // default DefaultMaxResultBytes (1 MiB)
func WithMaxDescriptionBytes(n int) ToolsOption    // default DefaultMaxDescriptionBytes (8 KiB)
var ErrResultTooLarge error                        // wraps agent.ErrTool

// Options for Connect.
func WithElicitation(f ElicitFunc) Option
func WithToolListChanged(f func(context.Context)) Option
func WithClientInfo(name, version string) Option

// ElicitFunc answers a server elicitation request; DeclineElicitation is a safe default.
type ElicitFunc func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error)
func DeclineElicitation(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error)
```

`Connect` and `Tools` wrap connection and listing failures with `agent.ErrTool` so they classify alongside the framework's other
tool errors. The wrapped tool's `Call` returns the server's result content as raw JSON (or its
`structuredContent`, when the server sends that alone); if
the server flags the result `IsError`, the content is surfaced as a Go error (wrapped with
`agent.ErrTool`) so the agent core sees a failure and can self-correct.

A call whose answer never arrives, because the connection dropped or the deadline passed after
the request was sent, may still have run on the server. `Call` then fails with
`agent.ErrToolOutcomeUnknown` rather than an ordinary failure. For a side effect the agent
records no result: the run stops with that error and a resume halts (`OutcomeUnknown`) instead of
telling the model the call failed, which would invite it to run the side effect again. A
retry-safe tool's lost call is an ordinary failure the model sees. A JSON-RPC error from the
server, or a call on a session that is already closed, is an ordinary failure too: the server
answered, or the request was never sent.

> The returned tools call back through the session, so **keep the session open for their
> lifetime.** Close it (and any server-side session) when the agent is done.

## Minimal usage example

The example below wires an in-memory client/server pair, which needs no network or external
process, then hands the discovered tools straight to an `Agent`. For a real server, replace
the transport with (for example) `mcp.NewCommandTransport(exec.Command("some-mcp-server"))`.

```go
package main

import (
	"context"
	"log"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/mcptools"
	"github.com/bide-ai/bide/model/anthropic"
)

func main() {
	ctx := context.Background()

	// Stand up an MCP server exposing one read-only tool.
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "demo", Version: "0.1.0"}, nil)
	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "echo",
		Description: "Echoes the provided text.",
		Annotations: &sdkmcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(_ context.Context, _ *sdkmcp.CallToolRequest, in struct {
		Text string `json:"text"`
	}) (*sdkmcp.CallToolResult, any, error) {
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "echo: " + in.Text}},
		}, nil, nil
	})

	clientT, serverT := sdkmcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer serverSession.Close()

	// Connect as the MCP client and discover its tools at runtime.
	session, err := mcptools.Connect(ctx, clientT)
	if err != nil {
		log.Fatal(err)
	}
	defer session.Close()

	tools, err := mcptools.Tools(ctx, session, mcptools.TrustAnnotations()) // our own in-memory server
	if err != nil {
		log.Fatal(err)
	}

	// The discovered MCP tools are plain agent.Tool values now. The echo tool's
	// readOnlyHint became Safety{ReadOnly: true}, so it is retry-safe on resume.
	j, err := agent.NewJournal(agent.NewMemStore())
	if err != nil {
		log.Fatal(err)
	}
	a, err := agent.New(anthropic.New("sk-..."), j, agent.WithTools(tools...))
	if err != nil {
		log.Fatal(err)
	}

	res, err := a.Run(ctx, "run-1", agent.UserText("Echo the word hello."))
	if err != nil {
		log.Fatal(err)
	}
	log.Println(res.Message.Text())
}
```

## Limitations

- The package adapts **tools**, plus **elicitation** (via `WithElicitation`) and
  **tools-list-changed** notifications (via `WithToolListChanged`). MCP **resources**,
  **prompts**, and **sampling** are not surfaced; use the underlying SDK session directly for
  those.
- Discovery is a **snapshot** at the time you call `Tools`. `WithToolListChanged` tells you
  when to re-list, but the package does not maintain a live, self-updating tool set for you:
  the agent core takes a fixed tool set per run by design.
- The package is a **client/host only.** It does not expose a Bide agent *as* an MCP
  server for other hosts to call.
- Transport lifecycle (spawning a subprocess, HTTP endpoints, reconnection) is the SDK's and
  the caller's responsibility. `Connect` only opens the session.
