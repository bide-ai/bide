package auditcli

import (
	"bytes"
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
)

// TestCLI_ApprovalTimesRoundTrip: an evidence package for a gate whose decisions carry signed and
// recorded times verifies through verify-evidence and verify-approvals; verify-approvals prints
// both times and the gap for each decision and notes a clock that runs ahead, without failing; a
// wider tolerance clears the note; and a decision time changed in the package fails both verbs.
func TestCLI_ApprovalTimesRoundTrip(t *testing.T) {
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
	const runID = "run-times"
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
	// alice decides 1.5s after being asked and is recorded at 4s; bob's clock runs ten minutes
	// ahead of bide's.
	for _, d := range []struct {
		id                  string
		decided, recordedAt time.Time
	}{
		{"alice", asked.Add(1500 * time.Millisecond), asked.Add(4 * time.Second)},
		{"bob", asked.Add(15 * time.Minute), asked.Add(5 * time.Minute)},
	} {
		sig, err := approvers[d.id].Sign(agent.ApprovalDecisionBytes(subject, d.id, true, d.decided))
		if err != nil {
			t.Fatal(err)
		}
		if err := agent.SubmitDecision(ctx, store, agent.Decision{RunID: runID, ToolUseID: "c1", ApproverID: d.id, Approved: true, DecidedAt: d.decided,
			Alg: audit.AlgEd25519, Signature: sig}, agent.WithClock(func() time.Time { return d.recordedAt })); err != nil {
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
	pkg.Actions = append(pkg.Actions, gate[:len(gate)-1]...) // the package already carries the call's result
	if err := pkg.Seal(logSigner); err != nil {
		t.Fatal(err)
	}
	pkgPath, keysPath := filepath.Join(dir, "evidence.json"), filepath.Join(dir, "keys.json")
	writeJSON(t, pkgPath, pkg)
	writeJSON(t, keysPath, keys)
	logKey := keyText(logSigner)
	approvals := func(path string, extra ...string) (int, string, string) {
		return runCLI(append([]string{"verify-approvals", "-evidence", path, "-pubkey", logKey, "-call", "c1", "-need", "2", "-approvers", "alice,bob", "-approver-keys", keysPath}, extra...)...)
	}

	if code, stdout, stderr := runCLI("verify-evidence", "-evidence", pkgPath, "-pubkey", logKey); code != 0 {
		t.Fatalf("verify-evidence: exit %d\n%s%s", code, stdout, stderr)
	}
	code, stdout, stderr := approvals(pkgPath)
	if code != 0 {
		t.Fatalf("verify-approvals: exit %d\n%s%s", code, stdout, stderr)
	}
	for _, want := range []string{
		"INFO  approval asked at 2026-10-13T12:00:00.000Z (bide's clock)",
		"INFO  alice decided at 2026-10-13T12:00:01.500Z (signed, the approver's clock), recorded at 2026-10-13T12:00:04.000Z (bide's clock), 2.5s after the signed time",
		"INFO  bob decided at 2026-10-13T12:15:00.000Z (signed, the approver's clock), recorded at 2026-10-13T12:05:00.000Z (bide's clock), 10m0s before the signed time",
		"note: bob: " + audit.FlagDecidedAfterRecorded,
		"PASS: 2 of 2 required approvals verified",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("verify-approvals output lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "note: alice") {
		t.Fatalf("alice's times are noted:\n%s", stdout)
	}
	if code, stdout, stderr := approvals(pkgPath, "-decision-time-tolerance", "15m"); code != 0 || strings.Contains(stdout, "note: bob") {
		t.Fatalf("verify-approvals at 15m: exit %d (note on bob: %v)\n%s%s", code, strings.Contains(stdout, "note: bob"), stdout, stderr)
	}
	if code, _, _ := approvals(pkgPath, "-decision-time-tolerance", "-1s"); code != exitUsage {
		t.Fatalf("a negative tolerance: exit %d, want %d", code, exitUsage)
	}

	// alice's decision time changed in the package: the record is no longer the one the log
	// committed to.
	tampered := pkg
	tampered.Actions = append([]audit.EvidenceAction(nil), pkg.Actions...)
	n := 0
	for i, act := range tampered.Actions {
		if act.Kind == audit.KindApproval && act.Label == "alice" {
			rb := bytes.Replace(act.Bundle.RecordBytes, []byte("12:00:01.500Z"), []byte("11:59:01.500Z"), 1)
			tampered.Actions[i].Bundle.RecordBytes = rb
			n++
		}
	}
	if n != 1 {
		t.Fatalf("alice's decision found %d times in the package", n)
	}
	if err := tampered.Seal(logSigner); err != nil { // even resealed by the log operator
		t.Fatal(err)
	}
	tamperedPath := filepath.Join(dir, "tampered.json")
	writeJSON(t, tamperedPath, tampered)
	if code, stdout, stderr := runCLI("verify-evidence", "-evidence", tamperedPath, "-pubkey", logKey); code != 1 {
		t.Fatalf("verify-evidence of a retimed decision: exit %d, want 1\n%s%s", code, stdout, stderr)
	}
	if code, stdout, stderr := approvals(tamperedPath); code != 1 {
		t.Fatalf("verify-approvals of a retimed decision: exit %d, want 1\n%s%s", code, stdout, stderr)
	}
}
