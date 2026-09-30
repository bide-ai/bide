package audit_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// emptyEvidence seals a package for a run with no completed tool call, so it proves nothing: no
// action, grant, run certificate, or consistency proof.
func emptyEvidence(t *testing.T) (audit.EvidencePackage, ed25519.PublicKey) {
	t.Helper()
	ctx := context.Background()
	store := agent.NewMemStore()
	if _, err := store.Do(ctx, "r", "note", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`1`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pkg, err := audit.Evidence(ctx, store, "r", edS(priv), 1700000000)
	if err != nil {
		t.Fatalf("Evidence: %v", err)
	}
	return pkg, pub
}

// A package that proves nothing is not evidence of anything: a sealed, authentic, empty package
// must not verify, or "PASS (0 items)" reads as a clean audit of a run nobody examined.
func TestEvidence_EmptyPackageDoesNotVerify(t *testing.T) {
	pkg, pub := emptyEvidence(t)
	if len(pkg.Actions) != 0 || pkg.Grants != nil || pkg.RunCertificate != nil || pkg.Consistency != nil {
		t.Fatalf("setup: package is not empty: %+v", pkg)
	}
	rep, err := pkg.Verify(edV(pub))
	if err := reportErr(rep.OK, err); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if rep.OK {
		t.Fatalf("an empty evidence package verified: %+v", rep)
	}
	if len(rep.Problems) == 0 {
		t.Fatalf("an empty package failed with no problem named: %+v", rep)
	}
}

// One proven item is enough: a package proving a single tool call verifies.
func TestEvidence_OneItemVerifies(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	if _, err := store.Do(ctx, "r", "charge", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`{"ok":true}`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pkg, err := audit.Evidence(ctx, store, "r", edS(priv), 1700000000)
	if err != nil {
		t.Fatalf("Evidence: %v", err)
	}
	rep, err := pkg.Verify(edV(pub))
	if err != nil || !rep.OK || len(rep.Items) != 1 {
		t.Fatalf("a one-item package: OK=%v items=%d problems=%v err=%v; want OK with 1 item", rep.OK, len(rep.Items), rep.Problems, err)
	}
}
