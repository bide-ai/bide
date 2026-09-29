package main

// Cross-process end-to-end test for the m-of-n approval example. Every actor is a SEPARATE
// process against one on-disk SQLite journal, the way approval works in practice: the agent
// run pauses and exits, approvers record signed decisions from their own processes later, the
// run resumes in a fresh process, and an auditor verifies a portable evidence file with no
// access to the journal. The refund side effect is counted through a witness file, which is
// the only observable that spans processes.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// exe adds the platform's executable suffix, so the built binaries run on Windows too.
func exe(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// build compiles pkg (a local dir or an import path) to a temp binary. It skips, rather than
// fails, when the toolchain is unavailable, following the repo's prerequisite convention.
func build(t *testing.T, dir, name, pkg string) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH; skipping cross-process e2e")
	}
	bin := filepath.Join(dir, exe(name))
	cmd := exec.Command("go", "build", "-o", bin, pkg)
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("go build %s failed (toolchain or deps unavailable): %v\n%s", pkg, err, out)
	}
	return bin
}

// run executes one process and returns its combined output and exit code.
func run(t *testing.T, bin string, args ...string) (string, int) {
	t.Helper()
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		t.Fatalf("exec %s %v: %v\n%s", bin, args, err, out)
	}
	return string(out), 0
}

// mustRun executes one process and fails the test on a non-zero exit.
func mustRun(t *testing.T, bin string, args ...string) string {
	t.Helper()
	out, code := run(t, bin, args...)
	if code != 0 {
		t.Fatalf("%s %v exited %d:\n%s", filepath.Base(bin), args, code, out)
	}
	return out
}

func wantLines(t *testing.T, out string, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if !strings.Contains(out, l+"\n") {
			t.Fatalf("output missing %q:\n%s", l, out)
		}
	}
}

func refunds(t *testing.T, witness string) int {
	t.Helper()
	b, err := os.ReadFile(witness)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}

func TestApprovalAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	app := build(t, dir, "approval", ".")
	db := filepath.Join(dir, "approval.db")
	witness := filepath.Join(dir, "refunds.txt")
	evidence := filepath.Join(dir, "evidence.json")

	// The agent asks to refund and the gate pauses the run; the process exits.
	out := mustRun(t, app, "run", "-db", db, "-witness", witness)
	wantLines(t, out, "status: paused", "approved: 0 of 2", "waiting on: ops, finance, risk")

	// Decisions that must not count, each from its own process.
	mustRun(t, app, "approve", "-db", db, "-as", "mallory") // valid key, not eligible
	mustRun(t, app, "approve", "-db", db, "-as", "risk", "-forge")
	out = mustRun(t, app, "run", "-db", db, "-witness", witness)
	wantLines(t, out, "status: paused", "approved: 0 of 2")

	mustRun(t, app, "approve", "-db", db, "-as", "ops")
	out = mustRun(t, app, "run", "-db", db, "-witness", witness)
	wantLines(t, out, "status: paused", "approved: 1 of 2")
	if n := refunds(t, witness); n != 0 {
		t.Fatalf("refund ran %d times below quorum, want 0", n)
	}

	mustRun(t, app, "approve", "-db", db, "-as", "finance")
	out = mustRun(t, app, "run", "-db", db, "-witness", witness)
	wantLines(t, out, "status: done")
	if n := refunds(t, witness); n != 1 {
		t.Fatalf("refund ran %d times at quorum, want 1", n)
	}

	// Resuming a finished run in yet another process does not refund again.
	mustRun(t, app, "run", "-db", db, "-witness", witness)
	if n := refunds(t, witness); n != 1 {
		t.Fatalf("refund ran %d times after a second resume, want 1", n)
	}

	// Export evidence, then verify it as an auditor with public keys only.
	out = mustRun(t, app, "evidence", "-db", db, "-out", evidence)
	var logPub string
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(l, "log public key: "); ok {
			logPub = strings.TrimSpace(v)
		}
	}
	if logPub == "" {
		t.Fatalf("evidence did not print the log public key:\n%s", out)
	}
	out = mustRun(t, app, "verify", "-in", evidence)
	wantLines(t, out,
		"proofs: PASS",
		"counted: ops, finance",
		"ignored: risk (signature does not verify under the approver's key)",
		"k-of-n: PASS (2 of 2)")

	// The standalone CLI verifies the same file against the log key.
	audit := build(t, dir, "bide-audit", "github.com/bide-ai/bide/cmd/bide-audit")
	out = mustRun(t, audit, "verify-evidence", "-evidence", evidence, "-pubkey", logPub)
	if !strings.Contains(out, "PASS") {
		t.Fatalf("bide-audit verify-evidence did not pass:\n%s", out)
	}

	// Tampering with a disclosed decision breaks verification in both verifiers.
	tampered := filepath.Join(dir, "tampered.json")
	tamperApproval(t, evidence, tampered)
	if out, code := run(t, app, "verify", "-in", tampered); code == 0 {
		t.Fatalf("verify accepted tampered evidence:\n%s", out)
	}
	if out, code := run(t, audit, "verify-evidence", "-evidence", tampered, "-pubkey", logPub); code == 0 {
		t.Fatalf("bide-audit accepted tampered evidence:\n%s", out)
	}
}

// tamperApproval flips finance's disclosed decision to a denial in the evidence file. The
// record no longer matches its inclusion proof, so the proof fails.
func tamperApproval(t *testing.T, in, out string) {
	t.Helper()
	b, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}
	var pkg map[string]any
	if err := json.Unmarshal(b, &pkg); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range pkg["actions"].([]any) {
		act := a.(map[string]any)
		if act["kind"] != "approval" {
			continue
		}
		rec := act["bundle"].(map[string]any)["record"].(map[string]any)
		if rec["approver"] == "finance" {
			rec["approved"] = false
			found = true
		}
	}
	if !found {
		t.Fatal("no finance approval in the evidence to tamper with")
	}
	b, err = json.Marshal(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
