package auditcli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/plan"
)

// verify-approvals verifies a gated plan flow node's decisions as it does a gated tool call's:
// -call names the node's key.
func TestCLI_VerifyApprovalsOfAFlowNode(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_, logPriv, _ := ed25519.GenerateKey(rand.Reader)
	logSigner := audit.Ed25519Signer{Priv: logPriv}
	signers := map[string]audit.Ed25519Signer{}
	for _, id := range []string{"ops", "finance"} {
		_, priv, _ := ed25519.GenerateKey(rand.Reader)
		signers[id] = audit.Ed25519Signer{Priv: priv}
	}
	resolver := func(id string) (agent.ApproverVerifier, bool) {
		s, ok := signers[id]
		if !ok {
			return nil, false
		}
		v, err := audit.VerifierOf(s)
		if err != nil {
			return nil, false
		}
		av, ok := v.(agent.ApproverVerifier)
		return av, ok
	}

	b := plan.New[int, int]("refunds").WithApproverVerifiers(resolver)
	b.Step("refund", func(_ context.Context, n int) (int, error) { return n, nil },
		plan.Approval(&agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance"}}))
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	store := agenttest.MemJournal()
	const runID = "run-flow-gate"
	_, err = f.Run(ctx, store, runID, 120)
	var pend *agent.ApprovalPending
	if !errors.As(err, &pend) {
		t.Fatalf("Run = %v; want a pause at the gate", err)
	}
	for id, s := range signers {
		sig, err := s.Sign(agent.ApprovalDecisionBytes(pend.Subject(), id, true))
		if err != nil {
			t.Fatal(err)
		}
		if err := agent.SubmitDecision(ctx, store, agent.Decision{RunID: runID, ToolUseID: pend.ToolUseID, ApproverID: id, Approved: true, Alg: s.Alg(), Signature: sig}); err != nil {
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
	acts, err := audit.ApprovalEvidence(ctx, store, runID, "node:refund", pkg.STH)
	if err != nil {
		t.Fatal(err)
	}
	pkg.Actions = acts
	if err := pkg.Seal(logSigner); err != nil {
		t.Fatal(err)
	}
	evidence, keys := filepath.Join(dir, "evidence.json"), filepath.Join(dir, "keys.json")
	writeJSON(t, evidence, pkg)
	writeJSON(t, keys, map[string]string{"ops": keyText(signers["ops"]), "finance": keyText(signers["finance"])})

	args := func(need string) []string {
		return []string{"verify-approvals", "-evidence", evidence, "-pubkey", keyText(logSigner), "-call", "node:refund", "-need", need, "-approvers", "ops,finance", "-approver-keys", keys}
	}
	if code, stdout, stderr := runCLI(args("2")...); code != 0 {
		t.Fatalf("verify-approvals of the flow node: exit %d, want 0\n%s%s", code, stdout, stderr)
	}
	if code, stdout, stderr := runCLI(args("1")...); code != 1 {
		t.Fatalf("verify-approvals against another policy: exit %d, want 1\n%s%s", code, stdout, stderr)
	}
}
