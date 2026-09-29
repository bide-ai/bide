# Model Context Protocol integration

The `mcp` package adapts a **Model Context Protocol** server's tools into `agent.Tool`.
Your agent is the MCP client/host; an MCP server is a *runtime* source of tools whose
schemas are only known at connect time. `Tools()` lists a connected session's tools and
wraps each one so the agent core can call it like any native tool. It is built on the
official SDK, `github.com/modelcontextprotocol/go-sdk`.

The package is deliberately thin: two entry points (`Connect` and `Tools`), a handful of options
(`TrustAnnotations`, `WithElicitation`, `DeclineElicitation`, `WithToolListChanged`,
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
  struct: `ArgsSchema()` returns the server's `InputSchema` as raw JSON for the `schema`
  package to dialectize per provider.
- **Refuse a malformed tool list.** The server's tool list is untrusted input. `Tools` fails
  with an error wrapping `agent.ErrProtocol` if a tool's name is outside the MCP grammar (1 to
  128 of `A-Z a-z 0-9 _ - .`), so no control character, space, or Unicode lookalike reaches your
  logs, journal, or approval prompts; if two tools share a name; or if a tool's input schema is
  not a JSON Schema object of type `"object"`. A server tool named like one of your own tools
  (or like a tool from another server) does not replace it: `agent.New` records the clash and
  every run of that agent fails with `agent.ErrConfig`, so the model's call never reaches the
  wrong tool.
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

```go
tools, err := mcp.Tools(ctx, session, mcp.TrustAnnotations())
```

`Safety()` then derives `agent.Safety` from the MCP tool's `Annotations` block:

| MCP annotation | Derived `agent.Safety` | Resume behavior |
|---|---|---|
| `readOnlyHint == true` | `Safety{ReadOnly: true}` | always safe to re-run after a crash |
| `idempotentHint == true` | `Safety{Idempotent: true}` | safe to retry |
| destructive or **unannotated** | `Safety{}` (the zero value) | halts the run on an unknown-outcome resume rather than risk firing a side effect twice |

`ReadOnly` wins if both hints are set: a read-only tool has no side effect to double-fire.
An absent `Annotations` block is treated as the destructive default per the MCP spec, which
is the conservative choice for resume. Whether to trust the annotations at all is the only
policy decision; the wrapped tool carries no other configuration.

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

```go
// Connect builds a client and opens a session on the given transport. Options
// wire optional client capabilities (elicitation, tools-list-changed, client info).
func Connect(ctx context.Context, transport mcp.Transport, opts ...Option) (*mcp.ClientSession, error)

// Tools lists a connected session's tools (paginated in full), each wrapped as an agent.Tool.
// Each is a side effect unless TrustAnnotations is passed.
func Tools(ctx context.Context, session *mcp.ClientSession, opts ...ToolsOption) ([]agent.Tool, error)

// TrustAnnotations maps a trusted server's annotations onto agent.Safety.
func TrustAnnotations() ToolsOption

// Options for Connect.
func WithElicitation(f ElicitFunc) Option
func WithToolListChanged(f func(context.Context)) Option
func WithClientInfo(name, version string) Option

// ElicitFunc answers a server elicitation request; DeclineElicitation is a safe default.
type ElicitFunc func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error)
func DeclineElicitation(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error)
```

`Connect` and `Tools` wrap failures with `agent.ErrTool` so they classify alongside the framework's other
tool errors. The wrapped tool's `Call` returns the server's result content as raw JSON; if
the server flags the result `IsError`, the content is surfaced as a Go error (wrapped with
`agent.ErrTool`) so the agent core sees a failure and can self-correct.

A call whose answer never arrives, because the connection dropped or the deadline passed after
the request was sent, may still have run on the server. `Call` then fails with
`agent.ErrToolOutcomeUnknown` rather than an ordinary failure. For a side effect the agent
records no result: the run stops with that error and a resume halts (`ResumeHalt`) instead of
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
	"github.com/bide-ai/bide/mcp"
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
	session, err := mcp.Connect(ctx, clientT)
	if err != nil {
		log.Fatal(err)
	}
	defer session.Close()

	tools, err := mcp.Tools(ctx, session, mcp.TrustAnnotations()) // our own in-memory server
	if err != nil {
		log.Fatal(err)
	}

	// The discovered MCP tools are plain agent.Tool values now. The echo tool's
	// readOnlyHint became Safety{ReadOnly: true}, so it is retry-safe on resume.
	a := agent.New(anthropic.New("sk-..."), agent.NewMemStore(), tools...)

	msg, err := a.Run(ctx, "run-1", "Echo the word hello.")
	if err != nil {
		log.Fatal(err)
	}
	log.Println(msg.Text())
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
