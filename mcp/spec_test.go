package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// listed returns the tools a raw server listing defs maps to, with opts.
func listed(t *testing.T, defs []map[string]any, opts ...ToolsOption) ([]agent.Tool, error) {
	t.Helper()
	var raw []json.RawMessage
	for _, d := range defs {
		b, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, b)
	}
	return Tools(context.Background(), connectRaw(t, &rawServer{tools: raw}), opts...)
}

var objectSchema = map[string]any{"type": "object"}

// Tools maps each server tool onto an agent.ToolSpec: name, title, description, input and output
// schemas, the safety its annotations give (only when trusted), the host's approval gate, and
// the host's per-call timeout.
func TestSpec_MCPMapping(t *testing.T) {
	out := map[string]any{"type": "object", "properties": map[string]any{"n": map[string]any{"type": "integer"}}}
	defs := []map[string]any{
		{"name": "read", "title": "Read it", "description": "reads", "inputSchema": objectSchema, "outputSchema": out,
			"annotations": map[string]any{"readOnlyHint": true, "title": "annotation title"}},
		{"name": "upsert", "inputSchema": objectSchema, "annotations": map[string]any{"idempotentHint": true, "title": "Upsert it"}},
		{"name": "wipe", "inputSchema": objectSchema, "annotations": map[string]any{"destructiveHint": true}},
		{"name": "plain", "inputSchema": objectSchema},
	}
	pol := &agent.ApprovalPolicy{Need: 2, Approvers: []string{"alice", "bob"}}
	tools, err := listed(t, defs, TrustAnnotations(), WithCallTimeout(3*time.Second), WithApproval("wipe", pol), WithApproval("plain", agent.SingleApproval()))
	if err != nil {
		t.Fatal(err)
	}
	outJSON, _ := json.Marshal(out)
	inJSON, _ := json.Marshal(objectSchema)
	want := map[string]agent.ToolSpec{
		"read":   {Name: "read", Title: "Read it", Description: "reads", Input: inJSON, Output: outJSON, Safety: agent.Safety{ReadOnly: true}, Timeout: 3 * time.Second},
		"upsert": {Name: "upsert", Title: "Upsert it", Input: inJSON, Safety: agent.Safety{Idempotent: true}, Timeout: 3 * time.Second},
		"wipe":   {Name: "wipe", Input: inJSON, Approval: pol, Timeout: 3 * time.Second},
		"plain":  {Name: "plain", Input: inJSON, Approval: agent.SingleApproval(), Timeout: 3 * time.Second},
	}
	for _, tool := range tools {
		got := agent.SpecOf(tool)
		w := want[got.Name]
		if !reflect.DeepEqual(got, w) {
			t.Errorf("%s: spec\n got %+v\nwant %+v", got.Name, got, w)
		}
		// The old method set describes the same tool.
		if tool.Name() != got.Name || tool.Description() != got.Description || string(tool.ArgsSchema()) != string(got.Input) || tool.Safety() != got.Safety {
			t.Errorf("%s: the old methods disagree with Spec", got.Name)
		}
	}

	// Untrusted annotations give no safety, and without WithCallTimeout there is no timeout.
	tools, err = listed(t, defs)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		if s := agent.SpecOf(tool); s.Safety != (agent.Safety{}) || s.Timeout != 0 || s.Approval != nil {
			t.Errorf("%s untrusted: spec %+v, want a side effect with no timeout or gate", s.Name, s)
		}
	}
}

// A policy the Spec returns is a copy: changing it changes neither the tool nor the option's
// argument.
func TestSpec_ApprovalIsCopied(t *testing.T) {
	pol := &agent.ApprovalPolicy{Need: 1, Approvers: []string{"alice", "bob"}}
	tools, err := listed(t, []map[string]any{{"name": "x", "inputSchema": objectSchema}}, WithApproval("x", pol))
	if err != nil {
		t.Fatal(err)
	}
	pol.Approvers[0] = "mallory"
	s := agent.SpecOf(tools[0])
	if s.Approval.Approvers[0] != "alice" {
		t.Fatalf("the option kept the caller's policy: %v", s.Approval.Approvers)
	}
	s.Approval.Approvers[0] = "mallory"
	if agent.SpecOf(tools[0]).Approval.Approvers[0] != "alice" {
		t.Fatal("changing a returned spec changed the tool")
	}
}

// An output schema that is not an object schema is refused, like an input schema.
func TestSpec_NonObjectOutputSchemaIsRefused(t *testing.T) {
	for name, s := range map[string]any{"string schema": map[string]any{"type": "string"}, "not an object": "yes", "array": []any{1}} {
		_, err := listed(t, []map[string]any{{"name": "x", "inputSchema": objectSchema, "outputSchema": s}})
		if !errors.Is(err, agent.ErrProtocol) {
			t.Errorf("%s: Tools = %v, want ErrProtocol", name, err)
		}
	}
}

// WithApproval refuses a nil or invalid policy, and a name the server does not list.
func TestWithApproval_Refusals(t *testing.T) {
	defs := []map[string]any{{"name": "transfer", "inputSchema": objectSchema}}
	for name, opt := range map[string]ToolsOption{
		"nil policy":    WithApproval("transfer", nil),
		"need 0":        WithApproval("transfer", &agent.ApprovalPolicy{}),
		"need above n":  WithApproval("transfer", &agent.ApprovalPolicy{Need: 3, Approvers: []string{"a", "b"}}),
		"duplicate":     WithApproval("transfer", &agent.ApprovalPolicy{Need: 1, Approvers: []string{"a", "a"}}),
		"misspelt name": WithApproval("tranfser", agent.SingleApproval()),
	} {
		if _, err := listed(t, defs, opt); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("%s: Tools = %v, want ErrConfig", name, err)
		}
	}
}

// The agent runs an MCP call under the tool's WithCallTimeout deadline: a server that never
// answers a side effect leaves the call's outcome unknown, so nothing is recorded, and a resume
// halts rather than send it again.
func TestSpec_TimeoutHaltsASideEffect(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv := &rawServer{tools: []json.RawMessage{rawTool("transfer")}, call: func(c *rawCall) {
		calls.Add(1)
		<-release // never answers in time
	}}
	tools, err := Tools(context.Background(), connectRaw(t, srv), WithCallTimeout(50*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	store := agent.NewMemStore()
	m := agent.NewScriptedModel(agent.ToolTurn("c1", "transfer", `{}`), agent.TextTurn("done"))
	a := agent.New(m, store, tools...)
	if _, err := a.Run(context.Background(), "r1", "go"); !errors.Is(err, agent.ErrToolOutcomeUnknown) {
		t.Fatalf("run err = %v, want ErrToolOutcomeUnknown", err)
	}
	if _, err := a.Run(context.Background(), "r1", "go"); !agent.IsPause(err) {
		t.Fatalf("resume err = %v, want an OutcomeUnknown halt", err)
	}
	var ou *agent.OutcomeUnknown
	if _, err := a.Run(context.Background(), "r1", "go"); !errors.As(err, &ou) {
		t.Fatalf("resume err = %v, want *OutcomeUnknown", err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("the server received %d calls, want 1", n)
	}
}
