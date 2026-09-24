package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// echoArgs is the input schema of the in-memory server's one tool.
type echoArgs struct {
	Text string `json:"text"`
}

// TestTools stands up an in-memory MCP server exposing one read-only echo tool,
// connects an in-memory client, and asserts Tools() faithfully adapts it into an
// agent.Tool: right name, ReadOnly safety derived from the annotation, a non-empty
// dynamic schema, and a working Call. No network, no external process.
func TestTools(t *testing.T) {
	ctx := context.Background()

	// Server side: one tool annotated read-only that echoes its input back.
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0.1.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "echo",
		Description: "Echoes the provided text.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(_ context.Context, _ *mcp.CallToolRequest, in echoArgs) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "echo: " + in.Text}},
		}, nil, nil
	})

	// Wire an in-memory client<->server pair.
	clientT, serverT := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer serverSession.Close()

	session, err := Connect(ctx, clientT)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()

	tools, err := Tools(ctx, session)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(tools))
	}
	tool := tools[0]

	if tool.Name() != "echo" {
		t.Errorf("Name() = %q, want %q", tool.Name(), "echo")
	}
	if s := tool.Safety(); !s.ReadOnly || s.Idempotent || s.RequiresApproval {
		t.Errorf("Safety() = %+v, want {ReadOnly:true}", s)
	}
	if len(tool.ArgsSchema()) == 0 {
		t.Errorf("ArgsSchema() is empty, want the server's dynamic input schema")
	}

	args, _ := json.Marshal(echoArgs{Text: "hello"})
	out, err := tool.Call(ctx, args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(string(out), "echo: hello") {
		t.Errorf("Call result = %s, want it to contain %q", out, "echo: hello")
	}
}

// TestSafetyMapping checks the annotation -> Safety derivation across the three cases:
// read-only, idempotent, and unannotated (destructive default).
func TestSafetyMapping(t *testing.T) {
	cases := []struct {
		name       string
		ann        *mcp.ToolAnnotations
		readOnly   bool
		idempotent bool
	}{
		{"readonly", &mcp.ToolAnnotations{ReadOnlyHint: true}, true, false},
		{"idempotent", &mcp.ToolAnnotations{IdempotentHint: true}, false, true},
		{"readonly wins over idempotent", &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true}, true, false},
		{"unannotated", nil, false, false},
		{"destructive", &mcp.ToolAnnotations{}, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := &tool{def: &mcp.Tool{Name: "x", Annotations: c.ann}}
			s := tr.Safety()
			if s.ReadOnly != c.readOnly || s.Idempotent != c.idempotent {
				t.Errorf("Safety() = %+v, want ReadOnly=%v Idempotent=%v", s, c.readOnly, c.idempotent)
			}
		})
	}
}
