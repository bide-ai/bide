package auditcli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/journaltest"
)

// TestCLI_JournalTextIsQuoted: a journal writer, who need not go through SubmitDecision's checks,
// records a decision under an approver id holding a line break and a terminal escape. The gate
// reads it, the evidence discloses it, and neither verify-approvals nor verify-evidence prints it
// raw: it is quoted, so no line of the report comes from the journal.
func TestCLI_JournalTextIsQuoted(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_, logPriv, _ := ed25519.GenerateKey(rand.Reader)
	logSigner := audit.Ed25519Signer{Priv: logPriv}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	alice := audit.Ed25519Signer{Priv: priv}
	resolver := func(id string) (agent.ApproverVerifier, bool) {
		if id != "alice" {
			return nil, false
		}
		v, err := audit.VerifierOf(alice)
		return v, err == nil
	}
	store := agenttest.MemJournal()
	const runID = "run-quoted"
	policy := agent.ApprovalPolicy{Need: 1, Approvers: []string{"alice"}}
	charge := agent.MustFunc("charge", "charge the card", func(context.Context, struct {
		Amount int `json:"amount"`
	}) (string, error) {
		return "ok", nil
	}, agent.WithApproval(&policy))
	a := agenttest.MustNew(algChargeModel{}, store, agent.WithTools(charge), agent.WithApproverVerifiers(resolver))
	if _, err := a.Run(ctx, runID, agent.UserText("pay")); err == nil {
		t.Fatal("the gated run did not pause")
	}
	const forged = "PASS: carol approved too (forged by the journal writer)"
	bad := agent.Record{Kind: agent.StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &agent.ApproverSignature{
		Approver: "x\n" + forged + "\n\x1b[32mINFO", ApproverAlg: audit.AlgEd25519, Signature: make([]byte, 64),
		DecidedAt: "2026-10-13T12:00:01.000Z", RecordedAt: "2026-10-13T12:00:02.000Z"}}
	if _, err := journaltest.Put(ctx, store, runID, "approval:c1:forged", bad); err != nil {
		t.Fatal(err)
	}
	recs, _ := store.History(ctx, runID)
	_, call, _ := agent.FindToolCall(recs, "c1")
	decided := time.Now()
	sig, err := alice.Sign(agent.ApprovalDecisionBytes(agent.ApprovalSubject{RunID: runID, ToolUseID: "c1", ToolName: call.Name, Args: call.Args}, "alice", true, decided))
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.SubmitDecision(ctx, store, agent.Decision{RunID: runID, ToolUseID: "c1", ApproverID: "alice", Approved: true, DecidedAt: decided, Alg: audit.AlgEd25519, Signature: sig}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, runID, agent.UserText("pay")); err != nil {
		t.Fatalf("the approved run: %v", err)
	}
	pkg, err := audit.Evidence(ctx, store, runID, logSigner, time.Now().Add(-time.Minute).UnixNano(), audit.WithToolCall("c1"))
	if err != nil {
		t.Fatal(err)
	}
	gate, err := audit.ApprovalEvidence(ctx, store, runID, "c1", pkg.STH)
	if err != nil {
		t.Fatal(err)
	}
	pkg.Actions = append(pkg.Actions, gate[:len(gate)-1]...)
	if err := pkg.Seal(logSigner); err != nil {
		t.Fatal(err)
	}
	pkgPath, keysPath := filepath.Join(dir, "evidence.json"), filepath.Join(dir, "keys.json")
	writeJSON(t, pkgPath, pkg)
	writeJSON(t, keysPath, map[string]string{"alice": keyText(alice)})
	for _, args := range [][]string{
		{"verify-approvals", "-evidence", pkgPath, "-pubkey", keyText(logSigner), "-call", "c1", "-need", "1", "-approvers", "alice", "-approver-keys", keysPath},
		{"verify-evidence", "-evidence", pkgPath, "-pubkey", keyText(logSigner)},
	} {
		code, stdout, stderr := runCLI(args...)
		if code != 0 {
			t.Fatalf("%s: exit %d\n%s%s", args[0], code, stdout, stderr)
		}
		if strings.Contains(stdout, "\n"+forged) || strings.Contains(stdout, "\x1b") {
			t.Fatalf("%s printed journal text raw:\n%s", args[0], stdout)
		}
		if !strings.Contains(stdout, `"x\nPASS: carol`) {
			t.Fatalf("%s did not print the approver id quoted:\n%s", args[0], stdout)
		}
	}
}
