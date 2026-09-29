package audit_test

import (
	"context"
	"crypto/ed25519"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// buildApprovalRun journals signed decisions by alice and bob on tool call "tu1", then the
// tool result, then a late decision by carol, and returns the store, the approver keys, and a
// log key plus a signed STH over the whole journal.
func buildApprovalRun(t *testing.T) (agent.Durable, map[string]ed25519.PublicKey, ed25519.PublicKey, audit.SignedTreeHead) {
	t.Helper()
	ctx := context.Background()
	store := agent.NewMemStore()
	keys := map[string]ed25519.PublicKey{}
	decide := func(id string, approved bool) {
		pub, priv, _ := ed25519.GenerateKey(nil)
		keys[id] = pub
		sig := ed25519.Sign(priv, agent.ApprovalDecisionBytes("run", "tu1", id, approved))
		if err := agent.ApproveAs(ctx, store, "run", "tu1", id, approved, sig); err != nil {
			t.Fatalf("ApproveAs %s: %v", id, err)
		}
	}
	decide("alice", true)
	decide("bob", true)
	if _, err := store.Do(ctx, "run", "tool:tu1", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepToolResult, ToolUseID: "tu1"}, nil
	}); err != nil {
		t.Fatalf("journal tool result: %v", err)
	}
	decide("carol", true)

	pub, priv, _ := ed25519.GenerateKey(nil)
	th, err := audit.NewTreeHead(ctx, store, "run", 1000)
	if err != nil {
		t.Fatalf("NewTreeHead: %v", err)
	}
	return store, keys, pub, audit.SignTreeHead(th, priv)
}

// TestProveApproval: a present approver's decision proves, verifies under the log key, binds
// to the STH size, and discloses a signature that verifies under the approver's key; a missing
// approver errors.
func TestProveApproval(t *testing.T) {
	ctx := context.Background()
	store, keys, pub, sth := buildApprovalRun(t)

	pb, err := audit.ProveApproval(ctx, store, "run", "tu1", "alice", sth)
	if err != nil {
		t.Fatalf("ProveApproval: %v", err)
	}
	if ok, err := pb.Verify(pub); err != nil || !ok {
		t.Fatalf("approval bundle failed to verify (ok=%v err=%v)", ok, err)
	}
	if pb.Inclusion.Size != sth.Size {
		t.Fatalf("inclusion size %d, want STH size %d", pb.Inclusion.Size, sth.Size)
	}
	r := pb.Record
	if r.Kind != agent.StepApproval || r.Approver != "alice" || !r.Approved {
		t.Fatalf("wrong record disclosed: %+v", r)
	}
	if !ed25519.Verify(keys["alice"], agent.ApprovalDecisionBytes("run", r.ToolUseID, r.Approver, r.Approved), r.Signature) {
		t.Fatal("disclosed decision signature does not verify under the approver's key")
	}

	otherPub, _, _ := ed25519.GenerateKey(nil)
	if ok, _ := pb.Verify(otherPub); ok {
		t.Fatal("approval bundle verified under the wrong log key")
	}

	if _, err := audit.ProveApproval(ctx, store, "run", "tu1", "mallory", sth); err == nil {
		t.Fatal("ProveApproval for an approver with no decision should error")
	}
	if _, err := audit.ProveApproval(ctx, store, "run", "tu2", "alice", sth); err == nil {
		t.Fatal("ProveApproval for an unknown tool call should error")
	}
}

// TestApprovalEvidence: decisions come first in approver order, the action last; approvers
// with no decision or a decision after the action are omitted, duplicates are proven once,
// and every bundle verifies under the one STH with decisions indexed before the action.
func TestApprovalEvidence(t *testing.T) {
	ctx := context.Background()
	store, _, pub, sth := buildApprovalRun(t)

	acts, err := audit.ApprovalEvidence(ctx, store, "run", "tu1", []string{"bob", "mallory", "alice", "bob", "carol"}, sth)
	if err != nil {
		t.Fatalf("ApprovalEvidence: %v", err)
	}
	want := []struct{ kind, label string }{{"approval", "bob"}, {"approval", "alice"}, {"tool", "tu1"}}
	if len(acts) != len(want) {
		t.Fatalf("got %d actions, want %d: %+v", len(acts), len(want), acts)
	}
	action := acts[len(acts)-1]
	for i, w := range want {
		a := acts[i]
		if a.Kind != w.kind || a.Label != w.label {
			t.Fatalf("action %d = (%s, %s), want (%s, %s)", i, a.Kind, a.Label, w.kind, w.label)
		}
		if ok, err := a.Bundle.Verify(pub); err != nil || !ok {
			t.Fatalf("action %d failed to verify (ok=%v err=%v)", i, ok, err)
		}
		if a.Bundle.STH.Size != sth.Size || a.Bundle.Inclusion.Size != sth.Size {
			t.Fatalf("action %d not bound to the STH size %d", i, sth.Size)
		}
		if a.Kind == "approval" && a.Bundle.Inclusion.Index >= action.Bundle.Inclusion.Index {
			t.Fatalf("decision %s indexed at %d, not before the action at %d", a.Label, a.Bundle.Inclusion.Index, action.Bundle.Inclusion.Index)
		}
	}

	if _, err := audit.ApprovalEvidence(ctx, store, "run", "tu2", []string{"alice"}, sth); err == nil {
		t.Fatal("ApprovalEvidence for a tool call with no result should error")
	}
}
