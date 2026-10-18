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
)

// TestCLI_VerifyDecisionCoverage: a 2-of-2 gate whose decisions were recorded by a process that
// signs nothing. The verb passes while every decision is covered or within the bound, fails once a
// decision is outside every head past the bound (by records, then by age), refuses heads that are
// not this journal's, and reports a bad command line as usage.
func TestCLI_VerifyDecisionCoverage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_, logPriv, _ := ed25519.GenerateKey(rand.Reader)
	logSigner := audit.Ed25519Signer{Priv: logPriv}
	approvers := map[string]audit.Ed25519Signer{}
	for _, id := range []string{"alice", "bob"} {
		_, priv, _ := ed25519.GenerateKey(rand.Reader)
		approvers[id] = audit.Ed25519Signer{Priv: priv}
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
	const runID = "run-cover"
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
	headNow := func() audit.SignedTreeHead {
		th, err := audit.NewTreeHead(ctx, store, runID, time.Now().Add(-time.Minute).UnixNano())
		if err != nil {
			t.Fatal(err)
		}
		return signHead(t, th, logSigner)
	}
	before := headNow()
	recs, _ := store.History(ctx, runID)
	_, call, _ := agent.FindToolCall(recs, "c1")
	subject := agent.ApprovalSubject{RunID: runID, ToolUseID: "c1", ToolName: call.Name, Args: call.Args}
	var afterAlice audit.SignedTreeHead
	for _, id := range []string{"alice", "bob"} {
		sig, err := approvers[id].Sign(agent.ApprovalDecisionBytes(subject, id, true, asked.Add(time.Second)))
		if err != nil {
			t.Fatal(err)
		}
		if err := agent.SubmitDecision(ctx, store, agent.Decision{RunID: runID, ToolUseID: "c1", ApproverID: id, Approved: true, DecidedAt: asked.Add(time.Second),
			Alg: audit.AlgEd25519, Signature: sig}, agent.WithClock(func() time.Time { return asked.Add(2 * time.Second) })); err != nil {
			t.Fatal(err)
		}
		if id == "alice" {
			afterAlice = headNow()
		}
	}
	whole := headNow()
	alicePos := afterAlice.Size - 1 // alice's decision is the last record the head after her decision covers
	journalPath := filepath.Join(dir, "journal.json")
	writeJSON(t, journalPath, exportJournal(t, store, runID))
	paths := map[string]string{}
	for name, h := range map[string]audit.SignedTreeHead{"before": before, "alice": afterAlice, "whole": whole} {
		paths[name] = filepath.Join(dir, name+".json")
		writeJSON(t, paths[name], h)
	}
	logKey := keyText(logSigner)
	cover := func(extra ...string) (int, string, string) {
		return runCLI(append([]string{"verify-decision-coverage", "-journal", journalPath, "-pubkey", logKey}, extra...)...)
	}

	// Alice is covered, bob's is the journal's last record: pending, within a one-record bound.
	// The heads are given largest first.
	code, stdout, stderr := cover("-heads", paths["alice"], "-heads", paths["before"], "-max-records", "1")
	if code != 0 || !strings.Contains(stdout, "alice") || !strings.Contains(stdout, "covered") || !strings.Contains(stdout, "pending") {
		t.Fatalf("exit %d\n%s%s", code, stdout, stderr)
	}
	// Only the head before the decisions: alice's has bob's record after it.
	code, stdout, _ = cover("-heads", paths["before"], "-max-records", "1")
	if code != exitNotVerified || !strings.Contains(stdout, "FAIL") || !strings.Contains(stdout, "overdue") || !strings.Contains(stdout, "records") {
		t.Fatalf("overdue by records: exit %d\n%s", code, stdout)
	}
	// No bound given: nothing is overdue, but a decision outside every head is shown pending.
	if code, stdout, _ = cover("-heads", paths["before"]); code != 0 || !strings.Contains(stdout, "pending") {
		t.Fatalf("no bound: exit %d\n%s", code, stdout)
	}
	// A whole-journal head covers everything.
	if code, stdout, _ = cover("-heads", paths["whole"], "-max-records", "1"); code != 0 || strings.Contains(stdout, "pending") {
		t.Fatalf("whole-journal head: exit %d\n%s", code, stdout)
	}
	// The tail under no head is stated: the records after the newest head, and none when it covers all.
	if code, stdout, _ = cover("-heads", paths["before"]); !strings.Contains(stdout, "are under no signed head yet") {
		t.Fatalf("no tail line for a head that leaves records after it: exit %d\n%s", code, stdout)
	}
	if _, stdout, _ = cover("-heads", paths["whole"]); strings.Contains(stdout, "under no signed head") {
		t.Fatalf("a tail line for a head that covers the whole journal:\n%s", stdout)
	}
	// -max-unanchored separates a head held back from a stale one: the head was signed a minute
	// before the real clock.
	if code, stdout, _ = cover("-heads", paths["before"], "-max-unanchored", "1h"); code != 0 || !strings.Contains(stdout, "within -max-unanchored") || !strings.Contains(stdout, "positions from 0") {
		t.Fatalf("a tail inside -max-unanchored: exit %d\n%s", code, stdout)
	}
	late := time.Now().Add(3 * time.Hour).UTC().Format(time.RFC3339)
	if code, stdout, _ = cover("-heads", paths["before"], "-max-unanchored", "1h", "-now", late); code != exitNotVerified || !strings.Contains(stdout, "likely failed publish") {
		t.Fatalf("a tail over -max-unanchored: exit %d\n%s", code, stdout)
	}
	if code, stdout, _ = cover("-heads", paths["whole"], "-max-unanchored", "1h", "-now", late); code != 0 {
		t.Fatalf("a head covering the whole journal must pass however old: exit %d\n%s", code, stdout)
	}
	if code, stdout, _ = cover("-heads", paths["before"], "-now", late); code != 0 || !strings.Contains(stdout, "cannot say whether") {
		t.Fatalf("without -max-unanchored the tail is reported and not failed: exit %d\n%s", code, stdout)
	}
	if code, _, _ = cover("-heads", paths["before"], "-max-unanchored", "-1s"); code != exitUsage {
		t.Fatalf("a negative -max-unanchored: exit %d, want usage", code)
	}
	// By age: both decisions were recorded at 12:00:02, an hour and a bit before -now.
	code, stdout, _ = cover("-heads", paths["before"], "-max-age", "1h", "-max-age-tolerance", "5m", "-now", "2026-10-13T13:03:00Z")
	if code != 0 {
		t.Fatalf("inside the tolerance: exit %d\n%s", code, stdout)
	}
	code, stdout, _ = cover("-heads", paths["before"], "-max-age", "1h", "-max-age-tolerance", "5m", "-now", "2026-10-13T13:08:00Z")
	if code != exitNotVerified || !strings.Contains(stdout, "age") {
		t.Fatalf("past the tolerance: exit %d\n%s", code, stdout)
	}
	// A head signed by another key covers nothing and is said so.
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	th, _ := audit.NewTreeHead(ctx, store, runID, time.Now().Add(-time.Minute).UnixNano())
	forged := filepath.Join(dir, "forged.json")
	writeJSON(t, forged, signHead(t, th, audit.Ed25519Signer{Priv: otherPriv}))
	code, stdout, _ = cover("-heads", forged, "-max-records", "1")
	if code != exitNotVerified || !strings.Contains(stdout, "not counted") || strings.Contains(stdout, "DIVERGENT") {
		t.Fatalf("a forged head: exit %d\n%s", code, stdout)
	}
	// A wrong log key ignores every head: a warning, and with a bound a failure.
	_, otherPub, _ := ed25519.GenerateKey(rand.Reader)
	wrongKey := keyText(audit.Ed25519Signer{Priv: otherPub})
	code, stdout, _ = runCLI("verify-decision-coverage", "-journal", journalPath, "-heads", paths["whole"], "-pubkey", wrongKey)
	if code != 0 || !strings.Contains(stdout, "WARNING") {
		t.Fatalf("a wrong key, no bound: exit %d\n%s", code, stdout)
	}
	code, stdout, _ = runCLI("verify-decision-coverage", "-journal", journalPath, "-heads", paths["whole"], "-pubkey", wrongKey, "-max-records", "50")
	if code != exitNotVerified || !strings.Contains(stdout, "WARNING") {
		t.Fatalf("a wrong key, with a bound: exit %d\n%s", code, stdout)
	}
	// An export with alice's decision removed: every head that reaches past it diverges, so the verb
	// fails (the not-verified class) and says to re-export, instead of passing a shorter list.
	var x audit.JournalExport
	exp := exportJournal(t, store, runID)
	x = exp
	x.Records = append(append([][]byte(nil), exp.Records[:alicePos]...), exp.Records[alicePos+1:]...)
	deleted := filepath.Join(dir, "journal-deleted.json")
	writeJSON(t, deleted, x)
	code, stdout, _ = runCLI("verify-decision-coverage", "-journal", deleted, "-heads", paths["alice"], "-heads", paths["whole"], "-pubkey", logKey, "-max-records", "1")
	if code != exitNotVerified || !strings.Contains(stdout, "DIVERGENT") || !strings.Contains(stdout, "re-export") {
		t.Fatalf("a deleted decision: exit %d\n%s", code, stdout)
	}
	// Usage.
	for _, args := range [][]string{
		{"verify-decision-coverage", "-pubkey", logKey},
		{"verify-decision-coverage", "-journal", journalPath},
		{"verify-decision-coverage", "-journal", journalPath, "-pubkey", logKey, "-max-records", "-1"},
		{"verify-decision-coverage", "-journal", journalPath, "-pubkey", logKey, "-max-age", "-1h"},
	} {
		if code, _, _ := runCLI(args...); code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, exitUsage)
		}
	}
}
