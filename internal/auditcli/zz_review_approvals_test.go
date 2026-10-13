package auditcli

// Adversarial review of #244 (decision times). The test reproduces a finding and fails on the
// reviewed branch.

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

// TestReview_CLIRecordedAtInjectsLines: recorded_at is not signed by the approver and nothing
// checks its form, and verify-approvals prints it raw. Whoever writes the journal (the party the
// evidence is checked against) can put a line break and any text in a counted decision's
// recorded_at, and the auditor's output gains lines the verifier never wrote, here a forged
// "PASS:" line and a forged approver line. The decision still counts: the signature does not
// cover recorded_at.
func TestReview_CLIRecordedAtInjectsLines(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_, logPriv, _ := ed25519.GenerateKey(rand.Reader)
	logSigner := audit.Ed25519Signer{Priv: logPriv}
	approvers := map[string]audit.Ed25519Signer{}
	keys := map[string]string{}
	for _, id := range []string{"alice", "bob"} {
		_, priv, _ := ed25519.GenerateKey(rand.Reader)
		approvers[id] = audit.Ed25519Signer{Priv: priv}
		keys[id] = keyText(approvers[id])
	}
	resolver := func(id string) (agent.ApproverVerifier, bool) {
		s, ok := approvers[id]
		if !ok {
			return nil, false
		}
		v, err := audit.VerifierOf(s)
		return v, err == nil
	}
	asked := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	store := agenttest.MemJournal()
	const runID = "run-inject"
	policy := agent.ApprovalPolicy{Need: 2, Approvers: []string{"alice", "bob"}}
	charge := agent.MustFunc("charge", "charge the card", func(context.Context, struct {
		Amount int `json:"amount"`
	}) (string, error) {
		return "ok", nil
	}, agent.WithApproval(&policy))
	a := agenttest.MustNew(algChargeModel{}, store, agent.WithTools(charge), agent.WithApproverVerifiers(resolver),
		agent.WithClock(func() time.Time { return asked }))
	if _, err := a.Run(ctx, runID, agent.UserText("pay")); err == nil {
		t.Fatal("the gated run did not pause")
	}
	recs, err := store.History(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	_, call, _ := agent.FindToolCall(recs, "c1")
	subject := agent.ApprovalSubject{RunID: runID, ToolUseID: "c1", ToolName: call.Name, Args: call.Args}
	decided := asked.Add(time.Second)
	const forged = "PASS: carol approved too (forged by the journal writer)"
	for _, id := range []string{"alice", "bob"} {
		sig, err := approvers[id].Sign(agent.ApprovalDecisionBytes(subject, id, true, decided))
		if err != nil {
			t.Fatal(err)
		}
		recordedAt := agent.FormatTimestamp(asked.Add(2 * time.Second))
		if id == "alice" {
			recordedAt += " (bide's clock), 1s after the signed time\n" + forged + "\nINFO  carol decided at 2026-10-13T12:00:01.000Z"
		}
		rec := agent.Record{Kind: agent.StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &agent.ApproverSignature{
			Approver: id, ApproverAlg: audit.AlgEd25519, Signature: sig, DecidedAt: agent.FormatTimestamp(decided), RecordedAt: recordedAt}}
		if _, err := journaltest.Put(ctx, store, runID, "approval:c1:"+id+":review", rec); err != nil {
			t.Fatal(err)
		}
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
	writeJSON(t, keysPath, keys)
	code, stdout, stderr := runCLI("verify-approvals", "-evidence", pkgPath, "-pubkey", keyText(logSigner), "-call", "c1", "-need", "2",
		"-approvers", "alice,bob", "-approver-keys", keysPath)
	for _, line := range strings.Split(stdout, "\n") {
		if strings.TrimSpace(line) == forged {
			t.Fatalf("verify-approvals (exit %d) printed a line taken from an unsigned journal field:\n%s%s", code, stdout, stderr)
		}
	}
}

// TestReview_CLIApproverIDInjectsLines (re-attack): SubmitDecision checks only that ApproverID is
// non-empty, so any caller of the public API, with no journal access and no key, can record a
// decision under an id holding a line break. The gate reads it (it is on the call), so evidence
// must disclose it, and both verbs print the id raw: verify-approvals in "INFO  ignored <id>: ..."
// and verify-evidence in the approval item's line (its label). The auditor's output gains a line
// the verifier never wrote.
func TestReview_CLIApproverIDInjectsLines(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_, logPriv, _ := ed25519.GenerateKey(rand.Reader)
	logSigner := audit.Ed25519Signer{Priv: logPriv}
	approvers := map[string]audit.Ed25519Signer{}
	keys := map[string]string{}
	for _, id := range []string{"alice", "bob"} {
		_, priv, _ := ed25519.GenerateKey(rand.Reader)
		approvers[id] = audit.Ed25519Signer{Priv: priv}
		keys[id] = keyText(approvers[id])
	}
	resolver := func(id string) (agent.ApproverVerifier, bool) {
		s, ok := approvers[id]
		if !ok {
			return nil, false
		}
		v, err := audit.VerifierOf(s)
		return v, err == nil
	}
	asked := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	store := agenttest.MemJournal()
	const runID = "run-inject-id"
	policy := agent.ApprovalPolicy{Need: 2, Approvers: []string{"alice", "bob"}}
	charge := agent.MustFunc("charge", "charge the card", func(context.Context, struct {
		Amount int `json:"amount"`
	}) (string, error) {
		return "ok", nil
	}, agent.WithApproval(&policy))
	a := agenttest.MustNew(algChargeModel{}, store, agent.WithTools(charge), agent.WithApproverVerifiers(resolver),
		agent.WithClock(func() time.Time { return asked }))
	if _, err := a.Run(ctx, runID, agent.UserText("pay")); err == nil {
		t.Fatal("the gated run did not pause")
	}
	recs, err := store.History(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	_, call, _ := agent.FindToolCall(recs, "c1")
	subject := agent.ApprovalSubject{RunID: runID, ToolUseID: "c1", ToolName: call.Name, Args: call.Args}
	decided := asked.Add(time.Second)
	const forged = "PASS: carol approved too (forged through the approver id)"
	// Anyone who can call SubmitDecision: no key, junk signature.
	if err := agent.SubmitDecision(ctx, store, agent.Decision{RunID: runID, ToolUseID: "c1", ApproverID: "x\n" + forged + "\nINFO  ignored y",
		Approved: true, DecidedAt: decided, Alg: audit.AlgEd25519, Signature: make([]byte, 64)}); err != nil {
		t.Logf("SubmitDecision refused the id: %v", err)
	}
	for _, id := range []string{"alice", "bob"} {
		sig, err := approvers[id].Sign(agent.ApprovalDecisionBytes(subject, id, true, decided))
		if err != nil {
			t.Fatal(err)
		}
		if err := agent.SubmitDecision(ctx, store, agent.Decision{RunID: runID, ToolUseID: "c1", ApproverID: id, Approved: true, DecidedAt: decided,
			Alg: audit.AlgEd25519, Signature: sig}); err != nil {
			t.Fatal(err)
		}
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
	writeJSON(t, keysPath, keys)
	for _, args := range [][]string{
		{"verify-approvals", "-evidence", pkgPath, "-pubkey", keyText(logSigner), "-call", "c1", "-need", "2", "-approvers", "alice,bob", "-approver-keys", keysPath},
		{"verify-evidence", "-evidence", pkgPath, "-pubkey", keyText(logSigner)},
	} {
		code, stdout, stderr := runCLI(args...)
		for _, line := range strings.Split(stdout, "\n") {
			if strings.TrimSpace(line) == forged {
				t.Errorf("%s (exit %d) printed a line taken from an unvalidated approver id:\n%s%s", args[0], code, stdout, stderr)
			}
		}
	}
}
