package auditcli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/journalhook"
	"github.com/bide-ai/bide/plan"
)

// verify-approvals -separate-from checks the escalation instance of a node under a taint policy that separates
// approvers: the approver the node's own gate counted does not count on the instance, and it fails without the flag
// (the gate enforced another rule than the policy it is given).
func TestCLI_VerifyApprovalsSeparateFrom(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_, logPriv, _ := ed25519.GenerateKey(rand.Reader)
	logSigner := audit.Ed25519Signer{Priv: logPriv}
	signers := map[string]audit.Ed25519Signer{}
	for _, id := range []string{"alice", "bob", "carol"} {
		_, priv, _ := ed25519.GenerateKey(rand.Reader)
		signers[id] = audit.Ed25519Signer{Priv: priv}
	}
	var resolver agent.ApproverVerifierFor = func(id string) (agent.ApproverVerifier, bool) {
		s, ok := signers[id]
		if !ok {
			return nil, false
		}
		return audit.Ed25519Verifier{Pub: s.Priv.Public().(ed25519.PublicKey)}, true
	}
	own := agent.ApprovalPolicy{Need: 1, Approvers: []string{"alice", "bob"}}
	b := plan.New[int, int]("refunds").WithApproverVerifiers(resolver)
	b.Step("pay", func(_ context.Context, n int) (int, error) { return n, nil }, plan.Approval(&own))
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	store := agenttest.MemJournal()
	const runID, inst = "run-sep", "node:pay:taint"
	var pend *agent.ApprovalPending
	if _, err := f.Run(ctx, store, runID, 120); !errors.As(err, &pend) {
		t.Fatalf("Run = %v; want a pause at the node's gate", err)
	}
	sign := func(p *agent.ApprovalPending, key, id string) {
		t.Helper()
		sig, err := signers[id].Sign(agent.ApprovalDecisionBytes(p.Subject(), id, true, testDecidedAt))
		if err != nil {
			t.Fatal(err)
		}
		if err := agent.SubmitDecision(ctx, store, agent.Decision{RunID: runID, ToolUseID: key, ApproverID: id, Approved: true, DecidedAt: testDecidedAt, Alg: audit.AlgEd25519, Signature: sig}); err != nil {
			t.Fatal(err)
		}
	}
	args := json.RawMessage(`120`)
	sign(pend, "node:pay", "alice")
	if ok, err := journalhook.Gate(ctx, store, runID, "node:pay", "pay", args, &own, resolver); err != nil || !ok {
		t.Fatalf("the node's gate = %v, %v", ok, err)
	}
	esc := agent.ApprovalPolicy{Need: 1, Approvers: []string{"alice", "carol"}}
	run := esc.SeparatedFrom("node:pay")
	if _, err := journalhook.Gate(ctx, store, runID, inst, "pay", args, &run, resolver); !errors.As(err, &pend) {
		t.Fatalf("the instance's first evaluation = %v, want a pause", err)
	}
	sign(pend, inst, "alice")
	sign(pend, inst, "carol")
	if ok, err := journalhook.Gate(ctx, store, runID, inst, "pay", args, &run, resolver); err != nil || !ok {
		t.Fatalf("the instance's gate = %v, %v", ok, err)
	}
	if _, err := f.Run(ctx, store, runID, 120); err != nil {
		t.Fatalf("Run at both quorums: %v", err)
	}
	pkg, err := audit.Evidence(ctx, store, runID, logSigner, time.Now().Add(-time.Hour).UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Actions, err = audit.ApprovalEvidence(ctx, store, runID, inst, pkg.STH); err != nil {
		t.Fatal(err)
	}
	if err := pkg.Seal(logSigner); err != nil {
		t.Fatal(err)
	}
	if rep, err := pkg.Verify(audit.Ed25519Verifier{Pub: logPriv.Public().(ed25519.PublicKey)}); err != nil || !rep.OK {
		t.Fatalf("pkg.Verify: %+v %v", rep, err)
	}
	evidence, keys := filepath.Join(dir, "evidence.json"), filepath.Join(dir, "keys.json")
	writeJSON(t, evidence, pkg)
	writeJSON(t, keys, map[string]string{"alice": keyText(signers["alice"]), "bob": keyText(signers["bob"]), "carol": keyText(signers["carol"])})
	base := []string{"verify-approvals", "-evidence", evidence, "-pubkey", keyText(logSigner), "-call", inst, "-need", "1", "-approvers", "alice,carol", "-approver-keys", keys}

	code, stdout, stderr := runCLI(append(base, "-separate-from", "node:pay")...)
	if code != 0 {
		t.Fatalf("verify-approvals -separate-from: exit %d, want 0\n%s%s", code, stdout, stderr)
	}
	for _, want := range []string{"ignored alice: " + agent.ReasonSibling, "PASS  carol approved"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, stdout)
		}
	}
	if code, stdout, stderr := runCLI(base...); code != 1 || !strings.Contains(stdout, "excluded_by_sibling") {
		t.Fatalf("verify-approvals without -separate-from: exit %d, want 1 naming the exclusion\n%s%s", code, stdout, stderr)
	}
}
