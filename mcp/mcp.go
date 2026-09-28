// Package mcp adapts a Model Context Protocol server's tools into agent.Tool. Our
// agent is the MCP client/host; an MCP server is a runtime source of tools whose
// schemas are only known at connect time (the untyped json.RawMessage path, not a Go
// struct). Tools() lists a connected session's tools and wraps each one so the agent
// core can call it like any native tool. Built on the official SDK,
// github.com/modelcontextprotocol/go-sdk.
//
// The payoff is in Safety(): MCP tool annotations map directly onto agent.Safety, so
// an MCP-sourced tool inherits side-effect-safe durable resume for free. A tool the
// server marks readOnlyHint is always safe to re-run after a crash; one marked
// idempotentHint is safe to retry; an unannotated (destructive or unknown) tool gets
// the zero Safety, which halts the run on an unknown-outcome resume rather than risk
// firing a side effect twice. No per-tool configuration on our side.
//
// Connect optionally wires three more client capabilities (see Option): an
// elicitation resolver (WithElicitation) the server can call to request
// structured input mid-tool-call; a tools-list-changed callback
// (WithToolListChanged) so a long-lived host can re-list when the server's tool
// set changes; and client identification (WithClientInfo). Tools() paginates
// the server's tool list in full (following the opaque cursor), so a server
// that returns its tools across several pages is never silently truncated.
//
// The resolver also answers elicitation embedded in a tool's multi-round-trip
// input requests (SEP-2322), since the SDK routes those to the same handler.
//
// Durability boundary: the resolver answers elicitation live, within the
// interaction that requested it; it is not a durable, resume-across-crash pause.
// Durability is inherited at the tool-call boundary (a read-only or idempotent
// tool re-runs, and so re-elicits, on resume, while an unannotated tool halts on
// an unknown-outcome resume). For input that must survive a crash and resume in a
// fresh process, use the native agent.Interrupt in a native tool.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/blackwell-systems/bide/agent"
)

// Tools lists the tools exposed by a connected MCP client session and returns each one
// wrapped as an agent.Tool. The session must already be connected (see Connect). The
// returned tools call back through the session, so keep it open for their lifetime.
func Tools(ctx context.Context, session *mcp.ClientSession) ([]agent.Tool, error) {
	var tools []agent.Tool
	// session.Tools follows the pagination cursor internally, so a server that
	// splits its tool list across several pages is listed in full.
	for def, err := range session.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("mcp: list tools: %w (%w)", err, agent.ErrTool)
		}
		tools = append(tools, &tool{session: session, def: def})
	}
	return tools, nil
}

// ElicitFunc answers an MCP server's elicitation/create request: the server asks
// the host for structured input (a message plus a JSON schema), either as a
// standalone request or embedded in a tool's multi-round-trip input requests.
// Return an *mcp.ElicitResult with Action "accept" (and Content matching the
// requested schema), "decline", or "cancel". See the package doc for the
// durability boundary: the resolver answers live, it is not a durable pause.
type ElicitFunc func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error)

// DeclineElicitation is a safe default resolver that declines every
// elicitation. Use it when the host has no way to gather the requested input.
func DeclineElicitation(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
	return &mcp.ElicitResult{Action: "decline"}, nil
}

type config struct {
	name, version string
	elicit        ElicitFunc
	onToolsChange func(context.Context)
}

// Option configures the MCP client that Connect builds.
type Option func(*config)

// WithElicitation registers a resolver for the server's elicitation/create
// requests. Setting it advertises the elicitation capability to the server.
func WithElicitation(f ElicitFunc) Option { return func(c *config) { c.elicit = f } }

// WithToolListChanged registers a callback fired when the server notifies that
// its tool list changed. The host should re-call Tools on the session to pick up
// the new set: the agent core takes a fixed tool set per run, so a changed set
// applies to the next run the host builds, not to one already in flight.
func WithToolListChanged(f func(context.Context)) Option {
	return func(c *config) { c.onToolsChange = f }
}

// WithClientInfo overrides the client name and version reported to the server
// (default "bide"/"0.1.0").
func WithClientInfo(name, version string) Option {
	return func(c *config) { c.name, c.version = name, version }
}

// Connect is a convenience over the SDK's client + transport: it builds a client and
// opens a session on the given transport (stdio, in-memory, streamable HTTP, ...).
// Options wire optional client capabilities (elicitation, tools-list-changed,
// client identity). Callers who already hold a *mcp.ClientSession can skip this
// and pass it to Tools.
func Connect(ctx context.Context, transport mcp.Transport, opts ...Option) (*mcp.ClientSession, error) {
	cfg := config{name: "bide", version: "0.1.0"}
	for _, o := range opts {
		o(&cfg)
	}
	co := &mcp.ClientOptions{}
	if cfg.elicit != nil {
		co.ElicitationHandler = func(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return cfg.elicit(ctx, req)
		}
	}
	if cfg.onToolsChange != nil {
		co.ToolListChangedHandler = func(ctx context.Context, _ *mcp.ToolListChangedRequest) {
			cfg.onToolsChange(ctx)
		}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: cfg.name, Version: cfg.version}, co)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: connect: %w (%w)", err, agent.ErrTool)
	}
	return session, nil
}

// tool wraps a single MCP server tool as an agent.Tool. The schema is dynamic (the
// server's InputSchema at connect time), so there is no Go struct behind it.
type tool struct {
	session *mcp.ClientSession
	def     *mcp.Tool
}

func (t *tool) Name() string        { return t.def.Name }
func (t *tool) Description() string { return t.def.Description }

// ArgsSchema returns the MCP tool's InputSchema as raw JSON. From the client side the
// SDK delivers it as a map[string]any, so we marshal it back to json.RawMessage for
// the schema/ package to dialectize per provider.
func (t *tool) ArgsSchema() json.RawMessage {
	if t.def.InputSchema == nil {
		return nil
	}
	if raw, ok := t.def.InputSchema.(json.RawMessage); ok {
		return raw
	}
	b, err := json.Marshal(t.def.InputSchema)
	if err != nil {
		return nil
	}
	return b
}

// Safety derives agent.Safety from the MCP tool annotations. This mapping is what gives
// MCP tools side-effect-safe durable resume without any per-tool config:
//
//	readOnlyHint   == true -> Safety{ReadOnly: true}   // always safe to re-run
//	idempotentHint == true -> Safety{Idempotent: true} // safe to retry
//	otherwise (destructive or unannotated) -> Safety{} // halts on unknown-outcome resume
//
// ReadOnly wins if both hints are set: a read-only tool has no side effect to
// double-fire. An absent Annotations block is treated as the destructive default per
// the MCP spec, which is the conservative choice for resume.
func (t *tool) Safety() agent.Safety {
	a := t.def.Annotations
	if a == nil {
		return agent.Safety{}
	}
	switch {
	case a.ReadOnlyHint:
		return agent.Safety{ReadOnly: true}
	case a.IdempotentHint:
		return agent.Safety{Idempotent: true}
	default:
		return agent.Safety{}
	}
}

// Call invokes the tool on the MCP server with the raw JSON args and returns the
// result content as raw JSON. If the server flags the result IsError, the content is
// returned as a Go error so the agent core sees a failure and can self-correct.
func (t *tool) Call(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	params := &mcp.CallToolParams{Name: t.def.Name}
	if len(args) > 0 {
		params.Arguments = args
	}
	res, err := t.session.CallTool(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("mcp: call tool %q: %w (%w)", t.def.Name, err, agent.ErrTool)
	}
	out, err := json.Marshal(res.Content)
	if err != nil {
		return nil, fmt.Errorf("mcp: marshal result of tool %q: %w (%w)", t.def.Name, err, agent.ErrProtocol)
	}
	if res.IsError {
		return nil, fmt.Errorf("mcp: tool %q reported error: %s (%w)", t.def.Name, out, agent.ErrTool)
	}
	return out, nil
}
