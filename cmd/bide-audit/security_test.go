package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// buildCLI builds the bide-audit binary into dir and returns its path.
func buildCLI(t *testing.T, dir string) string {
	t.Helper()
	bin := auditBin(dir)
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build binary: %v\n%s", err, out)
	}
	return bin
}

// exitCode runs the command and returns its exit code and combined output.
func exitCode(t *testing.T, bin string, args ...string) (int, string) {
	t.Helper()
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), string(out)
	}
	t.Fatalf("run %v: %v", args, err)
	return -1, ""
}

// verify-absent must reject an absence proof built against a tree of another kind: here a
// tool-use absence STH "proving" that a policy the run used was never used.
func TestVerifyAbsentCLI_RejectsCrossKindForgery(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	store := agent.NewMemStore()
	if _, err := store.Do(ctx, "r", "charge", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepToolResult, ToolUseID: "charge", Result: json.RawMessage(`{"policy_digest":"EVIL"}`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	recs, _ := store.History(ctx, "r")
	th, _ := audit.NewTreeHead(ctx, store, "r", 1)
	toolSTH, err := audit.SignAbsenceRoot(recs, audit.ToolUseKeys, th, priv, 1)
	if err != nil {
		t.Fatal(err)
	}
	forged := audit.AbsenceBundle{Format: audit.AbsenceFormat, RunID: "r", STH: toolSTH, Absence: audit.Absence{Key: audit.PolicyUsedKeyFor("EVIL"), Size: 1,
		Right: &audit.Neighbor{Key: "tooluse:charge", Proof: audit.Inclusion{Index: 0, Size: 1}}}}
	path := filepath.Join(dir, "forged.json")
	writeJSON(t, path, forged)
	bin := buildCLI(t, dir)
	pubHex := hex.EncodeToString(pub)
	if code, out := exitCode(t, bin, "verify-absent", "-bundle", path, "-pubkey", pubHex); code == 0 {
		t.Fatalf("verify-absent accepted a forged policy absence proof:\n%s", out)
	}

	// A genuine proof from prove-absent against the same head verifies.
	journalPath, sthPath, goodPath := filepath.Join(dir, "journal.json"), filepath.Join(dir, "sth.json"), filepath.Join(dir, "good.json")
	writeJSON(t, journalPath, recs)
	writeJSON(t, sthPath, toolSTH)
	if code, out := exitCode(t, bin, "prove-absent", "-journal", journalPath, "-sth", sthPath, "-key", "tool:refund", "-out", goodPath); code != 0 {
		t.Fatalf("prove-absent: exit %d\n%s", code, out)
	}
	if code, out := exitCode(t, bin, "verify-absent", "-bundle", goodPath, "-pubkey", pubHex); code != 0 {
		t.Fatalf("verify-absent rejected a genuine proof: exit %d\n%s", code, out)
	}
	// prove-absent refuses to prove a policy absent against the tool-use head.
	if code, out := exitCode(t, bin, "prove-absent", "-journal", journalPath, "-sth", sthPath, "-key", "policy:EVIL"); code == 0 {
		t.Fatalf("prove-absent proved a policy absent against a tool-use head:\n%s", out)
	}
}

// A malformed public key is an unusable input: exit 4 with a message, not a panic.
func TestCLI_ShortPublicKeyExitsFour(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bundle.json")
	writeJSON(t, path, audit.ProofBundle{Format: audit.ProofFormat, STH: audit.SignedTreeHead{TreeHead: audit.TreeHead{Timestamp: 1}}}) // a head the timestamp rule admits, so the key is what fails
	bin := buildCLI(t, dir)
	code, out := exitCode(t, bin, "verify", "-bundle", path, "-pubkey", "ab")
	if code != 4 || bytes.Contains([]byte(out), []byte("panic")) || !bytes.Contains([]byte(out), []byte("public key")) {
		t.Fatalf("verify -pubkey ab: exit %d, want 4 with a message\n%s", code, out)
	}

	// An approver key of the wrong length is refused the same way.
	ctx := context.Background()
	store := agent.NewMemStore()
	if _, err := store.Do(ctx, "run1", "pay", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`1`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pkg, err := audit.Evidence(ctx, store, "run1", priv, 1)
	if err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(dir, "evidence.json")
	writeJSON(t, evidence, pkg)
	keysPath := filepath.Join(dir, "keys.json")
	writeJSON(t, keysPath, map[string]string{"alice": "abcd"})
	code, out = exitCode(t, bin, "verify-approvals", "-evidence", evidence, "-pubkey", hex.EncodeToString(pub), "-call", "c1", "-need", "1", "-approvers", "alice", "-approver-keys", keysPath)
	if code != 4 || bytes.Contains([]byte(out), []byte("panic")) || !bytes.Contains([]byte(out), []byte(`approver "alice" key`)) {
		t.Fatalf("verify-approvals with a 2-byte approver key: exit %d, want 4 with a message\n%s", code, out)
	}
}

// A bundle file that shows a reader one value for a field and hands the verifier another (a
// duplicate key, differing only in case) must be rejected, not verified.
func TestCLI_RejectsDuplicateKeys(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := agent.NewMemStore()
	if _, err := store.Do(ctx, "run1", "pay", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepToolResult, ToolUseID: "pay", Result: json.RawMessage(`{"usd":10}`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	th, _ := audit.NewTreeHead(ctx, store, "run1", 1)
	b, err := audit.ProveToolCall(ctx, store, "run1", "pay", audit.SignTreeHead(th, priv))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(b)
	bin := buildCLI(t, dir)
	pubHex := hex.EncodeToString(pub)
	good := filepath.Join(dir, "good.json")
	if err := os.WriteFile(good, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out := exitCode(t, bin, "verify", "-bundle", good, "-pubkey", pubHex); code != 0 {
		t.Fatalf("verify rejected the untouched bundle: exit %d\n%s", code, out)
	}
	for name, edit := range map[string][2]string{
		// A reader scanning the record sees the first value; encoding/json keeps the last.
		"a case-variant duplicate key": {`"record":{`, `"record":{"Name":"refund-1000000",`},
		"an exact duplicate key":       {`"record":{`, `"record":{"name":"refund-1000000",`},
		"an unknown field":             {`"record":{`, `"record":{"approved_by":"cfo",`},
		"invalid UTF-8":                {`"name":"pay"`, "\"name\":\"pay\",\"approver\":\"\xff\""},
	} {
		edited := bytes.Replace(raw, []byte(edit[0]), []byte(edit[1]), 1)
		if bytes.Equal(edited, raw) {
			t.Fatalf("%s: edit did not apply", name)
		}
		path := filepath.Join(dir, "edited.json")
		if err := os.WriteFile(path, edited, 0o644); err != nil {
			t.Fatal(err)
		}
		if code, out := exitCode(t, bin, "verify", "-bundle", path, "-pubkey", pubHex); code == 0 {
			t.Fatalf("verify accepted a bundle with %s:\n%s", name, out)
		}
	}
}
