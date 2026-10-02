package main

// Cross-process end-to-end test for the m-of-n approval example. Every actor is a SEPARATE
// process against one on-disk SQLite journal, the way approval works in practice: the agent
// run pauses and exits, approvers record signed decisions from their own processes later, the
// run resumes in a fresh process, and an auditor verifies a portable evidence file with no
// access to the journal. The refund side effect is counted through a witness file, which is
// the only observable that spans processes.

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bide-ai/bide/audit"
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
	keys := filepath.Join(dir, "approver-keys.json")

	// The agent asks to refund and the gate pauses the run; the process exits.
	out := mustRun(t, app, "run", "-db", db, "-witness", witness)
	wantLines(t, out, "status: paused", "approved: 0 of 2", "waiting on: ops, finance, risk")

	// Decisions that must not count, each from its own process.
	mustRun(t, app, "approve", "-db", db, "-as", "mallory") // valid key, not eligible
	mustRun(t, app, "approve", "-db", db, "-as", "risk", "-forge")
	out = mustRun(t, app, "run", "-db", db, "-witness", witness)
	wantLines(t, out, "status: paused", "approved: 0 of 2")

	// risk's real decision still counts: the forgery did not take risk's place.
	mustRun(t, app, "approve", "-db", db, "-as", "risk")
	out = mustRun(t, app, "run", "-db", db, "-witness", witness)
	wantLines(t, out, "status: paused", "approved: 1 of 2", "waiting on: ops, finance")
	if n := refunds(t, witness); n != 0 {
		t.Fatalf("refund ran %d times below quorum, want 0", n)
	}

	// With -check, a decision that would not count is refused at submission, not recorded.
	out = mustRun(t, app, "approve", "-db", db, "-as", "ops", "-forge", "-check")
	if !strings.Contains(out, "refused:") {
		t.Fatalf("a forged decision with -check was not refused:\n%s", out)
	}
	out = mustRun(t, app, "run", "-db", db, "-witness", witness)
	wantLines(t, out, "status: paused", "approved: 1 of 2")

	mustRun(t, app, "approve", "-db", db, "-as", "ops", "-check")
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
	out = mustRun(t, app, "evidence", "-db", db, "-out", evidence, "-keys-out", keys)
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
		`call: refund {"order":42,"amount":120}`,
		"counted: risk, ops",
		"ignored: mallory (not an eligible approver)",
		"ignored: risk (signature does not verify for this call)",
		"k-of-n: PASS (2 of 2)")

	// The standalone CLI verifies the same files, for an auditor who does not write Go:
	// every proof, then the full m-of-n check against the expected policy and approver keys.
	auditCLI := build(t, dir, "bide-audit", "github.com/bide-ai/bide/cmd/bide-audit")
	out = mustRun(t, auditCLI, "verify-evidence", "-evidence", evidence, "-pubkey", logPub)
	if !strings.Contains(out, "PASS") {
		t.Fatalf("bide-audit verify-evidence did not pass:\n%s", out)
	}
	approvals := func(file, need string) []string {
		return []string{"verify-approvals", "-evidence", file, "-pubkey", logPub, "-call", "refund-1",
			"-need", need, "-approvers", "ops,finance,risk", "-approver-keys", keys}
	}
	out = mustRun(t, auditCLI, approvals(evidence, "2")...)
	wantLines(t, out,
		`approval gate on call "refund-1": refund {"order":42,"amount":120}`,
		"PASS  risk approved (signature verifies for this exact call)",
		"PASS  ops approved (signature verifies for this exact call)",
		"PASS: 2 of 2 required approvals verified, from complete evidence")
	// An auditor who expects a stricter policy than the one enforced is told so.
	if out, code := run(t, auditCLI, approvals(evidence, "3")...); code == 0 || !strings.Contains(out, "enforced") {
		t.Fatalf("verify-approvals accepted a policy mismatch (exit %d):\n%s", code, out)
	}

	// Tampering with a disclosed decision breaks verification in both verifiers, even when the log
	// key holder reseals the package.
	tampered := filepath.Join(dir, "tampered.json")
	flipped := false
	editEvidence(t, evidence, tampered, true, func(act *audit.EvidenceAction) bool {
		// The proof carries the record's stored bytes: flip ops's approval in them.
		if rec, err := act.Bundle.Record(); err == nil && act.Kind == audit.KindApproval && rec.Approver() == "ops" {
			edited := bytes.Replace(act.Bundle.RecordBytes, []byte(`"approved":true,`), nil, 1)
			flipped = flipped || !bytes.Equal(edited, act.Bundle.RecordBytes)
			act.Bundle.RecordBytes = edited
		}
		return true
	})
	if !flipped {
		t.Fatal("found no approval by ops to tamper with")
	}
	if out, code := run(t, app, "verify", "-in", tampered); code == 0 {
		t.Fatalf("verify accepted tampered evidence:\n%s", out)
	}
	if out, code := run(t, auditCLI, "verify-evidence", "-evidence", tampered, "-pubkey", logPub); code == 0 {
		t.Fatalf("bide-audit accepted tampered evidence:\n%s", out)
	}
	if out, code := run(t, auditCLI, approvals(tampered, "2")...); code == 0 {
		t.Fatalf("bide-audit verify-approvals accepted tampered evidence:\n%s", out)
	}

	// Leaving a decision out of the file breaks the package seal, so verify-evidence rejects it.
	unsealed := filepath.Join(dir, "unsealed.json")
	dropMallory := func(act *audit.EvidenceAction) bool {
		return act.Kind != audit.KindApproval || act.Label != "mallory"
	}
	editEvidence(t, evidence, unsealed, false, dropMallory)
	if out, code := run(t, auditCLI, "verify-evidence", "-evidence", unsealed, "-pubkey", logPub); code == 0 || !strings.Contains(out, "seal") {
		t.Fatalf("verify-evidence accepted evidence edited after sealing (exit %d):\n%s", code, out)
	}

	// The log key holder can omit a decision and reseal: every remaining proof is valid, so
	// verify-evidence, which checks proofs only, passes it, but the approval check catches it.
	omitted := filepath.Join(dir, "omitted.json")
	editEvidence(t, evidence, omitted, true, dropMallory)
	out, code := run(t, app, "verify", "-in", omitted)
	if code == 0 || !strings.Contains(out, "problem: the evidence omits") {
		t.Fatalf("verify accepted evidence with a decision left out (exit %d):\n%s", code, out)
	}
	if out, code := run(t, auditCLI, "verify-evidence", "-evidence", omitted, "-pubkey", logPub); code != 0 {
		t.Fatalf("verify-evidence rejected evidence whose remaining proofs are all valid:\n%s", out)
	}
	if out, code := run(t, auditCLI, approvals(omitted, "2")...); code == 0 || !strings.Contains(out, "omits") {
		t.Fatalf("verify-approvals accepted evidence with a decision left out (exit %d):\n%s", code, out)
	}
}

// editEvidence rewrites the evidence file's actions through the typed package, which keeps
// every recorded byte intact (a generic JSON edit would re-order object keys and invalidate
// proofs that commit to the exact bytes). edit may modify an action in place, and returns false
// to drop it. With reseal, the package is resealed with the log key, as its producer could.
func editEvidence(t *testing.T, in, out string, reseal bool, edit func(act *audit.EvidenceAction) bool) {
	t.Helper()
	b, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}
	var pkg audit.EvidencePackage
	if err := json.Unmarshal(b, &pkg); err != nil {
		t.Fatal(err)
	}
	var kept []audit.EvidenceAction
	for i := range pkg.Actions {
		if edit(&pkg.Actions[i]) {
			kept = append(kept, pkg.Actions[i])
		}
	}
	pkg.Actions = kept
	if reseal {
		if err := pkg.Seal(logKey()); err != nil {
			t.Fatal(err)
		}
	}
	b, err = json.Marshal(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
