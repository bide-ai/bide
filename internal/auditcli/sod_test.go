package auditcli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/plan"
)

// verify-approvals -exclude-principal checks a gate that excludes the requester: it names the
// requester, ignores their decision, and fails without the flag (the gate enforced another policy).
func TestCLI_VerifyApprovalsExcludePrincipal(t *testing.T) {
	ctx, ops := context.Background(), agent.WithIdentity(agent.Identity{OnBehalfOf: "ops"})
	dir := t.TempDir()
	_, logPriv, _ := ed25519.GenerateKey(rand.Reader)
	logSigner := audit.Ed25519Signer{Priv: logPriv}
	ids := []string{"ops", "finance", "risk"}
	signers := map[string]audit.Ed25519Signer{}
	for _, id := range ids {
		_, priv, _ := ed25519.GenerateKey(rand.Reader)
		signers[id] = audit.Ed25519Signer{Priv: priv}
	}
	resolver := func(id string) (agent.ApproverVerifier, bool) {
		s, ok := signers[id]
		if !ok {
			return nil, false
		}
		return audit.Ed25519Verifier{Pub: s.Priv.Public().(ed25519.PublicKey)}, true
	}
	b := plan.New[int, int]("refunds").WithApproverVerifiers(resolver)
	b.Step("refund", func(_ context.Context, n int) (int, error) { return n, nil },
		plan.Approval(&agent.ApprovalPolicy{Need: 2, Approvers: ids, ExcludePrincipal: true}))
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	store := agenttest.MemJournal()
	const runID = "run-sod"
	_, err = f.Run(ctx, store, runID, 120, ops)
	var pend *agent.ApprovalPending
	if !errors.As(err, &pend) {
		t.Fatalf("Run = %v; want a pause at the gate", err)
	}
	for _, id := range ids {
		sig, err := signers[id].Sign(agent.ApprovalDecisionBytes(pend.Subject(), id, true, testDecidedAt))
		if err != nil {
			t.Fatal(err)
		}
		if err := agent.SubmitDecision(ctx, store, agent.Decision{RunID: runID, ToolUseID: pend.ToolUseID, ApproverID: id, Approved: true, DecidedAt: testDecidedAt, Alg: audit.AlgEd25519, Signature: sig}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.Run(ctx, store, runID, 120); err != nil {
		t.Fatalf("Run at quorum: %v", err)
	}
	pkg, err := audit.Evidence(ctx, store, runID, logSigner, time.Now().Add(-time.Hour).UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Actions, err = audit.ApprovalEvidence(ctx, store, runID, "node:refund", pkg.STH); err != nil {
		t.Fatal(err)
	}
	if err := pkg.Seal(logSigner); err != nil {
		t.Fatal(err)
	}
	evidence, keys := filepath.Join(dir, "evidence.json"), filepath.Join(dir, "keys.json")
	writeJSON(t, evidence, pkg)
	writeJSON(t, keys, map[string]string{"ops": keyText(signers["ops"]), "finance": keyText(signers["finance"]), "risk": keyText(signers["risk"])})
	args := []string{"verify-approvals", "-evidence", evidence, "-pubkey", keyText(logSigner), "-call", "node:refund", "-need", "2", "-approvers", "ops,finance,risk", "-approver-keys", keys}

	code, stdout, stderr := runCLI(append(args, "-exclude-principal")...)
	if code != 0 {
		t.Fatalf("verify-approvals -exclude-principal: exit %d, want 0\n%s%s", code, stdout, stderr)
	}
	for _, want := range []string{"requesters ops", "ignored ops: " + agent.ReasonRequester, "PASS  finance approved", "PASS  risk approved"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, stdout)
		}
	}
	if code, stdout, stderr := runCLI(args...); code != 1 || !strings.Contains(stdout, "exclude_principal") {
		t.Fatalf("verify-approvals without -exclude-principal: exit %d, want 1 naming the exclusion\n%s%s", code, stdout, stderr)
	}
}
