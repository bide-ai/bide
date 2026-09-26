// Command mcp shows the mcp package adapting a Model Context Protocol server's tools into
// agent.Tool values. It stands up an in-memory MCP client/server pair (no network, no
// subprocess), discovers the server's one read-only tool at runtime, and hands the wrapped
// tool straight to an Agent. The server's readOnlyHint annotation becomes
// agent.Safety{ReadOnly: true}, so the MCP-sourced tool is retry-safe on resume with no
// per-tool configuration on the client side.
//
// It runs with NO API key: the model is a small inline scripted Model that calls the
// discovered MCP tool once, then answers in text. Swap it for a real provider (and the
// in-memory transport for a stdio/HTTP one) to point at a live MCP server.
//
//	go run ./examples/mcp
package main

import (
	"context"
	"fmt"
	"log"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	agent "github.com/dayna/go-agents"
	"github.com/dayna/go-agents/mcp"
)

// scriptModel calls the discovered "echo" tool once, then answers in text on the next turn,
// so the example needs no live LLM.
type scriptModel struct{ turn int }

func (m *scriptModel) Stream(_ context.Context, _ agent.Request) (*agent.Stream, error) {
	ch := make(chan agent.Emit, 4)
	if m.turn == 0 {
		ch <- agent.Emit{Event: agent.ToolCallDelta{Index: 0, ID: "c1", Name: "echo", ArgsFragment: []byte(`{"text":"hello"}`)}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "tool_use"}}
	} else {
		ch <- agent.Emit{Event: agent.TextDelta{Text: "The MCP echo tool replied."}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	}
	m.turn++
	close(ch)
	return agent.NewStream(ch), nil
}

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
	fmt.Printf("discovered %d MCP tool(s):\n", len(tools))
	for _, t := range tools {
		// The echo tool's readOnlyHint became Safety{ReadOnly: true}, so it is retry-safe.
		fmt.Printf("  %s: readOnly=%v\n", t.Name(), t.Safety().ReadOnly)
	}

	// The discovered MCP tools are plain agent.Tool values now; the session must stay open
	// for their lifetime because they call back through it.
	a := agent.New(&scriptModel{}, agent.NewMemStore(), tools...)
	out, err := a.Run(ctx, "mcp-1", "Echo the word hello.")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("final answer: %s\n", out.Text())
}
