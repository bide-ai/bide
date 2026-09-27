# Model Context Protocol integration

The `mcp` package adapts a **Model Context Protocol** server's tools into `agent.Tool`.
Your agent is the MCP client/host; an MCP server is a *runtime* source of tools whose
schemas are only known at connect time. `Tools()` lists a connected session's tools and
wraps each one so the agent core can call it like any native tool. It is built on the
official SDK, `github.com/modelcontextprotocol/go-sdk`.

The package is deliberately thin: two exported functions and an internal adapter. It does
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
- **Map MCP annotations onto `agent.Safety`.** This is the payoff. An MCP-sourced tool
  inherits side-effect-safe durable resume with no per-tool configuration on your side.

## Safety mapping

`Safety()` derives `agent.Safety` from the MCP tool's `Annotations` block:

| MCP annotation | Derived `agent.Safety` | Resume behavior |
|---|---|---|
| `readOnlyHint == true` | `Safety{ReadOnly: true}` | always safe to re-run after a crash |
| `idempotentHint == true` | `Safety{Idempotent: true}` | safe to retry |
| destructive or **unannotated** | `Safety{}` (the zero value) | halts the run on an unknown-outcome resume rather than risk firing a side effect twice |

`ReadOnly` wins if both hints are set: a read-only tool has no side effect to double-fire.
An absent `Annotations` block is treated as the destructive default per the MCP spec, which
is the conservative choice for resume. This is the only place a policy decision is made; the
wrapped tool carries no other configuration.

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
  tool-call boundary (a read-only or idempotent tool re-runs, and so re-elicits, on resume,
  while an unannotated tool halts on an unknown-outcome resume). For input that must survive a
  crash and resume in a fresh process, use the native `agent.Interrupt` in a native tool.

- **`WithToolListChanged(callback)`** fires the callback when the server notifies that its
  tool list changed. Re-call `Tools` on the session to pick up the new set. The agent core
  takes a fixed tool set per run, so a changed set applies to the next run you build, not one
  already in flight.

- **`WithClientInfo(name, version)`** overrides the client name and version reported to the
  server (default `"Bide"` / `"0.1.0"`).

## Exported API

```go
// Connect builds a client and opens a session on the given transport. Options
// wire optional client capabilities (elicitation, tools-list-changed, client info).
func Connect(ctx context.Context, transport mcp.Transport, opts ...Option) (*mcp.ClientSession, error)

// Tools lists a connected session's tools (paginated in full), each wrapped as an agent.Tool.
func Tools(ctx context.Context, session *mcp.ClientSession) ([]agent.Tool, error)

// Options for Connect.
func WithElicitation(f ElicitFunc) Option
func WithToolListChanged(f func(context.Context)) Option
func WithClientInfo(name, version string) Option

// ElicitFunc answers a server elicitation request; DeclineElicitation is a safe default.
type ElicitFunc func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error)
func DeclineElicitation(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error)
```

Both wrap failures with `agent.ErrTool` so they classify alongside the framework's other
tool errors. The wrapped tool's `Call` returns the server's result content as raw JSON; if
the server flags the result `IsError`, the content is surfaced as a Go error (wrapped with
`agent.ErrTool`) so the agent core sees a failure and can self-correct.

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

	agent "github.com/blackwell-systems/bide"
	"github.com/blackwell-systems/bide/mcp"
	"github.com/blackwell-systems/bide/model/anthropic"
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

	tools, err := mcp.Tools(ctx, session)
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
