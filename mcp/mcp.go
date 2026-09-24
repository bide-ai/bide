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
package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	agent "github.com/dayna/go-agents"
)

// Tools lists the tools exposed by a connected MCP client session and returns each one
// wrapped as an agent.Tool. The session must already be connected (see Connect). The
// returned tools call back through the session, so keep it open for their lifetime.
func Tools(ctx context.Context, session *mcp.ClientSession) ([]agent.Tool, error) {
	res, err := session.ListTools(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: list tools: %w", err)
	}
	tools := make([]agent.Tool, 0, len(res.Tools))
	for _, t := range res.Tools {
		tools = append(tools, &tool{session: session, def: t})
	}
	return tools, nil
}

// Connect is a convenience over the SDK's client + transport: it builds a client and
// opens a session on the given transport (stdio, in-memory, streamable HTTP, ...).
// Callers who already hold a *mcp.ClientSession can skip this and pass it to Tools.
func Connect(ctx context.Context, transport mcp.Transport) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "go-agents", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: connect: %w", err)
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
		return nil, fmt.Errorf("mcp: call tool %q: %w", t.def.Name, err)
	}
	out, err := json.Marshal(res.Content)
	if err != nil {
		return nil, fmt.Errorf("mcp: marshal result of tool %q: %w", t.def.Name, err)
	}
	if res.IsError {
		return nil, fmt.Errorf("mcp: tool %q reported error: %s", t.def.Name, out)
	}
	return out, nil
}
