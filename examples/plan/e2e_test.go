package main

// Cross-process end-to-end tests for the plan example. Each test builds the example
// binary once, then invokes it as SEPARATE processes against one shared on-disk SQLite
// journal, mirroring the store/sqlite cross-process pattern (reopen the same file in a
// fresh process). Phase 1 crashes a chosen Step via the -crash flag (a real os.Exit(1),
// not an in-process panic); phase 2 is a fresh process that resumes the same run id off
// the same journal file. Both assert the non-idempotent reserve side effect fired at
// most once TOTAL across the two processes, using an appended witness file as the
// cross-process observable.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildExample compiles the example to a temp binary once per test and returns its path.
// It skips (rather than fails) when the Go toolchain is unavailable, following the repo's
// skip-when-prerequisite-missing convention.
func buildExample(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH; skipping cross-process e2e")
	}
	bin := filepath.Join(t.TempDir(), "plan-example")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("go build failed (toolchain or deps unavailable): %v\n%s", err, out)
	}
	return bin
}

// runPhase runs the example binary as one process with the given args and returns its
// combined output and exit code. It does not fail the test on a non-zero exit, because a
// crash phase is EXPECTED to exit non-zero; callers assert the code themselves.
func runPhase(t *testing.T, bin string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if ok := asExitError(err, &exitErr); ok {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("run example: %v\n%s", err, out)
		}
	}
	return string(out), code
}

// asExitError reports whether err is an *exec.ExitError and, if so, assigns it to target.
// It avoids importing errors just for one As call in the test.
func asExitError(err error, target **exec.ExitError) bool {
	if e, ok := err.(*exec.ExitError); ok {
		*target = e
		return true
	}
	return false
}

// witnessLines counts the lines in the side-effect witness file, which is how many times
// the reserve step actually fired across all processes. A missing file means zero.
func witnessLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("read witness %s: %v", path, err)
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return 0
	}
	return len(strings.Split(trimmed, "\n"))
}

// TestE2E_ResumeAfterKillCompletesOnce is scenario (a): phase 1 fires the reserve side
// effect, commits it, then crashes at the start of the side-effect-free finalize step
// (-crash before-finalize). Phase 2 is a fresh process on the same journal that resumes,
// hits the finalize halt, resolves it out of band (-resolve), and completes. The reserve
// side effect must have fired at most once total, and the completed run must Conform.
func TestE2E_ResumeAfterKillCompletesOnce(t *testing.T) {
	bin := buildExample(t)
	dir := t.TempDir()
	db := filepath.Join(dir, "triage.db")
	witness := filepath.Join(dir, "reserve.witness")
	const runID = "order-a"

	// Phase 1: fresh process, reserve fires and commits, then finalize crashes.
	out1, code1 := runPhase(t, bin,
		"-db", db, "-run", runID, "-witness", witness, "-amount", "500", "-crash", "before-finalize")
	if code1 == 0 {
		t.Fatalf("phase 1 was expected to crash non-zero, got exit 0\n%s", out1)
	}
	if n := witnessLines(t, witness); n != 1 {
		t.Fatalf("after phase 1: reserve fired %d times, want 1\n%s", n, out1)
	}

	// Phase 2: fresh process, same journal + run id, resumes and resolves the finalize halt.
	out2, code2 := runPhase(t, bin,
		"-db", db, "-run", runID, "-witness", witness, "-amount", "500", "-resolve")
	if code2 != 0 {
		t.Fatalf("phase 2 was expected to complete cleanly, got exit %d\n%s", code2, out2)
	}
	if n := witnessLines(t, witness); n != 1 {
		t.Fatalf("across both processes: reserve fired %d times, want exactly 1 (at-most-once)\n--- phase1 ---\n%s\n--- phase2 ---\n%s", n, out1, out2)
	}
	if !strings.Contains(out2, "Outcome:reserved") {
		t.Fatalf("phase 2 did not complete to a reserved receipt\n%s", out2)
	}
	if !strings.Contains(out2, "Conform: the run followed the declared graph") {
		t.Fatalf("phase 2 run did not Conform to the declared graph\n%s", out2)
	}
	// The completed run must also prove CRYPTOGRAPHIC conformance: the flow:digest
	// record is included under a signed audit tree head and equals the declared
	// topology's Digest(). This is the offline-verifiable "followed the signed diagram".
	if !strings.Contains(out2, "Cryptographic conformance: the run committed to the declared topology under the signed tree head") {
		t.Fatalf("phase 2 run did not prove cryptographic conformance\n%s", out2)
	}
}

// TestE2E_HaltOnAmbiguityAcrossProcesses is scenario (b): phase 1 fires the reserve side
// effect then crashes BEFORE reserve's result is journaled (-crash during-reserve),
// leaving an attempt marker with no result. Phase 2 is a fresh process on the same
// journal that resumes and must HALT at reserve (*plan.HaltAmbiguous) rather than re-fire
// the effect. The reserve side effect must have fired exactly once, from phase 1 only.
func TestE2E_HaltOnAmbiguityAcrossProcesses(t *testing.T) {
	bin := buildExample(t)
	dir := t.TempDir()
	db := filepath.Join(dir, "triage.db")
	witness := filepath.Join(dir, "reserve.witness")
	const runID = "order-b"

	// Phase 1: reserve fires, then the process dies before the result is recorded.
	out1, code1 := runPhase(t, bin,
		"-db", db, "-run", runID, "-witness", witness, "-amount", "500", "-crash", "during-reserve")
	if code1 == 0 {
		t.Fatalf("phase 1 was expected to crash non-zero, got exit 0\n%s", out1)
	}
	if n := witnessLines(t, witness); n != 1 {
		t.Fatalf("after phase 1: reserve fired %d times, want 1\n%s", n, out1)
	}

	// Phase 2: fresh process resumes; the attempt-without-result must halt the run.
	out2, code2 := runPhase(t, bin,
		"-db", db, "-run", runID, "-witness", witness, "-amount", "500")
	if code2 != 0 {
		t.Fatalf("phase 2 reported the halt through a clean exit; got exit %d\n%s", code2, out2)
	}
	if !strings.Contains(out2, `Run halted at step "reserve"`) {
		t.Fatalf("phase 2 was expected to halt at reserve (HaltAmbiguous), output was:\n%s", out2)
	}
	if n := witnessLines(t, witness); n != 1 {
		t.Fatalf("across both processes: reserve fired %d times, want exactly 1 (no double-fire on resume)\n--- phase1 ---\n%s\n--- phase2 ---\n%s", n, out1, out2)
	}
	// The halted, in-flight journal is a legitimate observable, not a divergence.
	if !strings.Contains(out2, "Conform: the run followed the declared graph") {
		t.Fatalf("a halted run's partial journal should still Conform (no divergence)\n%s", out2)
	}
}
