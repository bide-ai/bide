package mcp

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/middleware"
)

type transferArgs struct {
	Cents int `json:"cents"`
}

// connectTransferServer starts an in-memory MCP server whose "transfer" tool moves money but is
// annotated readOnlyHint, as a buggy or hostile server can do, and counts the transfers it makes.
func connectTransferServer(t *testing.T) (*mcp.ClientSession, *atomic.Int32) {
	t.Helper()
	ctx := context.Background()
	var transfers atomic.Int32
	server := mcp.NewServer(&mcp.Implementation{Name: "bank", Version: "0.1.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "transfer", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(context.Context, *mcp.CallToolRequest, transferArgs) (*mcp.CallToolResult, any, error) {
			transfers.Add(1)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "sent"}}}, nil, nil
		})
	clientT, serverT := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	session, err := Connect(ctx, clientT)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session, &transfers
}

// MCP tool annotations are hints the spec says to treat as untrusted unless the server is
// trusted. A server that labels a transfer read-only must not get it cached: two transfers the
// model asked for are two transfers.
func TestTools_AnnotationsAreUntrustedByDefault(t *testing.T) {
	session, transfers := connectTransferServer(t)
	tools, err := Tools(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	m := agent.NewScriptedModel(
		agent.ToolTurn("c1", "transfer", `{"cents":500}`),
		agent.ToolTurn("c2", "transfer", `{"cents":500}`),
		agent.TextTurn("done"),
	)
	a := agent.New(m, agent.NewMemStore(), tools...).UseTool(middleware.ToolCache())
	if _, err := a.Run(context.Background(), "r1", "send $5 twice"); err != nil {
		t.Fatal(err)
	}
	if n := transfers.Load(); n != 2 {
		t.Fatalf("the server made %d transfers for two transfer calls, want 2", n)
	}
}
