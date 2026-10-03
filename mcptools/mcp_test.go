package mcptools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bide-ai/bide/agent"
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

	tools, err := Tools(ctx, session, TrustAnnotations()) // a server we trust to label its tools
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(tools))
	}
	tool := tools[0]

	if tool.Spec().Name != "echo" {
		t.Errorf("Name() = %q, want %q", tool.Spec().Name, "echo")
	}
	if s := tool.Spec().Safety; s != (agent.Safety{ReadOnly: true}) {
		t.Errorf("Safety() = %+v, want {ReadOnly:true}", s)
	}
	if len(tool.Spec().Input) == 0 {
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

// TestTools_Paginates forces the server to return its tools one per page
// (PageSize 1) and asserts Tools() follows the cursor and returns all of them,
// so a multi-page server is never silently truncated.
func TestTools_Paginates(t *testing.T) {
	ctx := context.Background()

	server := mcp.NewServer(&mcp.Implementation{Name: "paged-server", Version: "0.1.0"}, &mcp.ServerOptions{PageSize: 1})
	for _, name := range []string{"a", "b", "c"} {
		mcp.AddTool(server, &mcp.Tool{
			Name:        name,
			Description: "tool " + name,
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		}, func(_ context.Context, _ *mcp.CallToolRequest, _ echoArgs) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
		})
	}

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
	if len(tools) != 3 {
		t.Fatalf("got %d tools across pages, want 3 (pagination cursor not followed)", len(tools))
	}
}

// TestElicitation_ResolverAnswers exercises the real production path on the
// current protocol: a tool returns a multi-round-trip input request (SEP-2322)
// asking the host for input; the SDK's client middleware fulfills it by invoking
// our WithElicitation resolver, then retries the call, and the tool reads the
// answer. This drives the resolver through our Tools()/Call wrappers end to end.
func TestElicitation_ResolverAnswers(t *testing.T) {
	ctx := context.Background()

	server := mcp.NewServer(&mcp.Implementation{Name: "elicit-server", Version: "0.1.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "ask", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(_ context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			if len(req.Params.InputResponses) == 0 {
				return &mcp.CallToolResult{
					InputRequests: mcp.InputRequestMap{
						"q": &mcp.ElicitParams{
							Message: "what is the answer?",
							RequestedSchema: map[string]any{
								"type":       "object",
								"properties": map[string]any{"answer": map[string]any{"type": "string"}},
							},
						},
					},
					RequestState: "step=1",
				}, nil, nil
			}
			ans := req.Params.InputResponses["q"].(*mcp.ElicitResult).Content["answer"].(string)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "answer:" + ans}}}, nil, nil
		})

	clientT, serverT := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer serverSession.Close()

	var elicited bool
	session, err := Connect(ctx, clientT, WithElicitation(
		func(_ context.Context, r *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			elicited = true
			if r.Params.Message != "what is the answer?" {
				t.Errorf("elicit message = %q", r.Params.Message)
			}
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"answer": "yes"}}, nil
		}))
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()

	tools, err := Tools(ctx, session)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	out, err := tools[0].Call(ctx, []byte(`{}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !elicited {
		t.Error("elicitation resolver was never invoked")
	}
	if !strings.Contains(string(out), "answer:yes") {
		t.Errorf("Call result = %s, want it to contain %q", out, "answer:yes")
	}
}

// TestToolListChanged_Notifies asserts the WithToolListChanged callback fires when
// the server adds a tool after the session is connected, so a long-lived host can
// re-list.
func TestToolListChanged_Notifies(t *testing.T) {
	ctx := context.Background()

	server := mcp.NewServer(&mcp.Implementation{Name: "dyn-server", Version: "0.1.0"}, nil)
	// One tool at connect time so the server advertises the tools capability.
	mcp.AddTool(server, &mcp.Tool{Name: "seed", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(_ context.Context, _ *mcp.CallToolRequest, _ echoArgs) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
		})

	clientT, serverT := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer serverSession.Close()

	changed := make(chan struct{}, 1)
	session, err := Connect(ctx, clientT, WithToolListChanged(func(context.Context) {
		select {
		case changed <- struct{}{}:
		default:
		}
	}))
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()

	// Mutate the server's tool set after connect: this sends tools/list_changed.
	mcp.AddTool(server, &mcp.Tool{Name: "added", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(_ context.Context, _ *mcp.CallToolRequest, _ echoArgs) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
		})

	select {
	case <-changed:
	case <-time.After(2 * time.Second):
		t.Fatal("tools/list_changed callback did not fire within 2s")
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
			tr := &tool{def: &mcp.Tool{Name: "x", Annotations: c.ann}, cfg: &toolsConfig{trust: true}}
			s := tr.Safety()
			if s.ReadOnly != c.readOnly || s.Idempotent != c.idempotent {
				t.Errorf("trusted: Safety() = %+v, want ReadOnly=%v Idempotent=%v", s, c.readOnly, c.idempotent)
			}
			if s := (&tool{def: tr.def, cfg: &toolsConfig{}}).Safety(); s.ReadOnly || s.Idempotent {
				t.Errorf("untrusted: Safety() = %+v, want the zero Safety", s)
			}
		})
	}
}
