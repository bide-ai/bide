package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// transferServer lists one tool, "transfer", and counts the calls it answers.
func transferServer(t *testing.T, annotations map[string]any) (*rawServer, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	def := map[string]any{"name": "transfer", "inputSchema": map[string]any{"type": "object"}}
	if annotations != nil {
		def["annotations"] = annotations
	}
	b, _ := json.Marshal(def)
	return &rawServer{tools: []json.RawMessage{b}, call: func(c *rawCall) {
		n.Add(1)
		c.Reply(textResult("sent", false))
	}}, &n
}

// An MCP tool gated by WithSafety pauses for approval before the server sees the call, and
// resumes past the approval like a local tool, making the call exactly once.
func TestWithSafety_ApprovalPausesAndResumes(t *testing.T) {
	srv, calls := transferServer(t, nil)
	tools, err := Tools(context.Background(), connectRaw(t, srv), WithSafety("transfer", agent.Safety{RequiresApproval: true}))
	if err != nil {
		t.Fatal(err)
	}
	store := agent.NewMemStore()
	m := agent.NewScriptedModel(agent.ToolTurn("c1", "transfer", `{"cents":500}`), agent.TextTurn("done"))
	a := agent.New(m, store, tools...)
	_, err = a.Run(context.Background(), "r1", "send $5")
	var pend *agent.PendingApproval
	if !errors.As(err, &pend) || pend.ToolName != "transfer" || pend.ToolUseID != "c1" {
		t.Fatalf("run err = %v, want *PendingApproval for transfer", err)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("the server received %d calls before approval, want 0", n)
	}
	if err := agent.Approve(context.Background(), store, "r1", "c1", true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "r1", "send $5"); err != nil {
		t.Fatalf("resume err = %v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("the server received %d calls, want 1", n)
	}
}

type testVerifier string

func (v testVerifier) Verify(message, sig []byte) bool {
	return bytes.Equal(sig, append([]byte(v+"|"), message...))
}

// An m-of-n gate on an MCP tool holds the call until k of the named approvers approve.
func TestWithSafety_QuorumApproval(t *testing.T) {
	srv, calls := transferServer(t, nil)
	pol := &agent.ApprovalPolicy{Need: 2, Approvers: []string{"alice", "bob", "carol"}}
	tools, err := Tools(context.Background(), connectRaw(t, srv), WithSafety("transfer", agent.Safety{Approval: pol}))
	if err != nil {
		t.Fatal(err)
	}
	verifiers := func(id string) (agent.ApproverVerifier, bool) {
		for _, a := range pol.Approvers {
			if a == id {
				return testVerifier(id), true
			}
		}
		return nil, false
	}
	store := agent.NewMemStore()
	m := agent.NewScriptedModel(agent.ToolTurn("c1", "transfer", `{"cents":500}`), agent.TextTurn("done"))
	a := agent.New(m, store, tools...).WithApproverVerifiers(verifiers)
	var pend *agent.PendingApproval
	for i, approver := range []string{"", "alice", "bob"} {
		if approver != "" {
			sig := []byte(approver + "|")
			sig = append(sig, agent.ApprovalDecisionBytes(pend.Subject(), approver, true)...)
			if err := agent.ApproveAs(context.Background(), store, "r1", "c1", approver, true, sig); err != nil {
				t.Fatal(err)
			}
		}
		_, err := a.Run(context.Background(), "r1", "send $5")
		if i < 2 {
			if !errors.As(err, &pend) || pend.Quorum == nil {
				t.Fatalf("after %d approvals: err = %v, want *PendingApproval with a quorum", i, err)
			}
			if n := calls.Load(); n != 0 {
				t.Fatalf("after %d approvals the server received %d calls", i, n)
			}
			continue
		}
		if err != nil {
			t.Fatalf("after 2 approvals: err = %v", err)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("the server received %d calls, want 1", n)
	}
}

// A Safety set by name is the tool's Safety, whatever the server's annotations say and whether
// or not they are trusted; other tools keep the default.
func TestWithSafety_OverridesAnnotations(t *testing.T) {
	srv, _ := transferServer(t, map[string]any{"readOnlyHint": true})
	tools, err := Tools(context.Background(), connectRaw(t, srv), TrustAnnotations(), WithSafety("transfer", agent.Safety{}))
	if err != nil {
		t.Fatal(err)
	}
	if s := tools[0].Safety(); s.ReadOnly || s.Idempotent || s.RequiresApproval {
		t.Fatalf("Safety() = %+v, want the zero Safety set by WithSafety", s)
	}
	key := func(json.RawMessage) string { return "k" }
	srv, _ = transferServer(t, nil)
	tools, err = Tools(context.Background(), connectRaw(t, srv), WithSafety("transfer", agent.Safety{Idempotent: true, IdempotencyKey: key}))
	if err != nil {
		t.Fatal(err)
	}
	if s := tools[0].Safety(); !s.Idempotent || s.IdempotencyKey == nil {
		t.Fatalf("Safety() = %+v, want the Safety set by WithSafety", s)
	}
}

// A Safety for a tool the server does not list is a mistake that would leave the real tool
// ungated (a typo in an approval gate), so Tools refuses it.
func TestWithSafety_UnknownToolIsAConfigError(t *testing.T) {
	srv, _ := transferServer(t, nil)
	_, err := Tools(context.Background(), connectRaw(t, srv), WithSafety("tranfser", agent.Safety{RequiresApproval: true}))
	if !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("err = %v, want ErrConfig naming the unlisted tool", err)
	}
}
