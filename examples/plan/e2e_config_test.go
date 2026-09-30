package main

// Cross-process end-to-end tests for the CONFIG-LOADED triage flow. These mirror the
// code-built cross-process tests in e2e_test.go exactly, but pass -config in BOTH phases so
// the flow is built by plan.Load-ing declarativeConfig rather than the Go
// builder. The point they prove: a declarative CONFIG-LOADED flow survives a real process death.
// A fresh process re-loads the SAME config, resumes the same run id against the same sqlite
// journal, the run is at-most-once, completes or halts cleanly, conforms, AND stays
// cryptographically conformable to that config across the crash and the reload (the phase-2
// reloaded flow's Digest() equals the flow:digest committed to the journal in phase 1).
//
// They reuse the helpers in e2e_test.go: buildExample, runPhase, witnessLines.

import (
	"regexp"
	"strings"
	"testing"
)

// digestFromConformance extracts the hex topology digest the example prints on its
// "Cryptographic conformance" line. That line is emitted only after the run proves the
// journaled flow:digest record is included under a signed tree head AND equals the reloaded
// flow's Digest(), so the extracted value is simultaneously the digest COMMITTED to the
// journal and the digest of the RELOADED flow. It returns "" when the line is absent.
func digestFromConformance(out string) string {
	re := regexp.MustCompile(`Cryptographic conformance: the run committed to the declared topology under the signed tree head \(digest ([0-9a-f]+)\)`)
	m := re.FindStringSubmatch(out)
	if len(m) != 2 {
		return ""
	}
	return m[1]
}

// canonicalConfigDigest runs the example once as a clean config-loaded process and returns
// the topology digest it commits to. It is the known-good value the cross-process phases are
// checked against: a fresh reload must reproduce this exact digest, or config-conformance did
// not survive the reload.
func canonicalConfigDigest(t *testing.T, bin string) string {
	t.Helper()
	dir := t.TempDir()
	out, code := runPhase(t, bin, "-config", "-db", dir+"/canonical.db", "-run", "canonical", "-amount", "500")
	if code != 0 {
		t.Fatalf("canonical config run was expected to complete cleanly, got exit %d\n%s", code, out)
	}
	d := digestFromConformance(out)
	if d == "" {
		t.Fatalf("canonical config run printed no cryptographic-conformance digest\n%s", out)
	}
	return d
}

// TestE2E_ConfigLoaded_ResumeAcrossProcesses is the config-loaded analogue of
// TestE2E_ResumeAfterKillCompletesOnce. Phase 1 is a fresh process built from declarativeConfig
// (-config) whose reserve step fires the non-idempotent effect and commits it, then crashes
// at the start of the side-effect-free finalize step (-crash before-finalize). Phase 2 is a
// fresh process, ALSO built from the SAME config (-config), that reopens the SAME journal for
// the SAME run id, resumes, hits the finalize halt, resolves it out of band (-resolve), and
// completes. It asserts:
//   - the reserve effect fired AT MOST ONCE total across the two processes;
//   - the run completes to the reserved Receipt;
//   - Conform reports no diffs;
//   - the phase-2 reloaded flow proves cryptographic conformance, and its committed digest
//     equals the canonical config digest (config-conformance held across the crash and the
//     reload: the journaled flow:digest from phase 1 matches the reloaded flow's Digest()).
func TestE2E_ConfigLoaded_ResumeAcrossProcesses(t *testing.T) {
	bin := buildExample(t)
	want := canonicalConfigDigest(t, bin)

	dir := t.TempDir()
	db := dir + "/triage.db"
	witness := dir + "/reserve.witness"
	const runID = "config-order-a"

	// Phase 1: fresh config-loaded process, reserve fires and commits, then finalize crashes.
	out1, code1 := runPhase(t, bin,
		"-config", "-db", db, "-run", runID, "-witness", witness, "-amount", "500", "-crash", "before-finalize")
	if code1 == 0 {
		t.Fatalf("phase 1 was expected to crash non-zero, got exit 0\n%s", out1)
	}
	if n := witnessLines(t, witness); n != 1 {
		t.Fatalf("after phase 1: reserve fired %d times, want 1\n%s", n, out1)
	}

	// Phase 2: fresh config-loaded process, SAME journal + run id, resumes and resolves.
	out2, code2 := runPhase(t, bin,
		"-config", "-db", db, "-run", runID, "-witness", witness, "-amount", "500", "-resolve")
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
	// The reloaded config-loaded run must prove cryptographic conformance: the flow:digest
	// committed to the journal (in phase 1) is included under a signed tree head and equals
	// the reloaded flow's Digest(). Its value must be the canonical config digest, proving
	// config-conformance held across the crash and the fresh-process reload.
	got := digestFromConformance(out2)
	if got == "" {
		t.Fatalf("phase 2 run did not prove cryptographic conformance\n%s", out2)
	}
	if got != want {
		t.Fatalf("phase 2 reloaded digest %s != canonical config digest %s: config-conformance did not survive the reload\n--- phase2 ---\n%s", got, want, out2)
	}
}

// TestE2E_ConfigLoaded_HaltOnAmbiguityAcrossProcesses is the config-loaded analogue of
// TestE2E_HaltOnAmbiguityAcrossProcesses. Phase 1 is a fresh config-loaded process whose
// reserve step fires the non-idempotent effect then crashes BEFORE the result is journaled
// (-crash during-reserve), leaving an attempt marker with no result. Phase 2 is a fresh
// process, ALSO built from the SAME config (-config), that reopens the SAME journal for the
// SAME run id and must HALT at reserve (*agent.OutcomeUnknown) rather than re-fire the effect.
// It asserts the reserve effect fired EXACTLY ONCE (from phase 1 only), the halt is reported,
// and the halted partial journal still Conforms (a halt is not a divergence).
func TestE2E_ConfigLoaded_HaltOnAmbiguityAcrossProcesses(t *testing.T) {
	bin := buildExample(t)

	dir := t.TempDir()
	db := dir + "/triage.db"
	witness := dir + "/reserve.witness"
	const runID = "config-order-b"

	// Phase 1: reserve fires, then the config-loaded process dies before the result is recorded.
	out1, code1 := runPhase(t, bin,
		"-config", "-db", db, "-run", runID, "-witness", witness, "-amount", "500", "-crash", "during-reserve")
	if code1 == 0 {
		t.Fatalf("phase 1 was expected to crash non-zero, got exit 0\n%s", out1)
	}
	if n := witnessLines(t, witness); n != 1 {
		t.Fatalf("after phase 1: reserve fired %d times, want 1\n%s", n, out1)
	}

	// Phase 2: fresh config-loaded process resumes; the attempt-without-result must halt the run.
	out2, code2 := runPhase(t, bin,
		"-config", "-db", db, "-run", runID, "-witness", witness, "-amount", "500")
	if code2 != 0 {
		t.Fatalf("phase 2 reported the halt through a clean exit; got exit %d\n%s", code2, out2)
	}
	if !strings.Contains(out2, `Run halted at step "node:reserve"`) {
		t.Fatalf("phase 2 was expected to halt at reserve (*agent.OutcomeUnknown), output was:\n%s", out2)
	}
	if n := witnessLines(t, witness); n != 1 {
		t.Fatalf("across both processes: reserve fired %d times, want exactly 1 (no double-fire on resume)\n--- phase1 ---\n%s\n--- phase2 ---\n%s", n, out1, out2)
	}
	// The halted, in-flight journal is a legitimate observable, not a divergence.
	if !strings.Contains(out2, "Conform: the run followed the declared graph") {
		t.Fatalf("a halted config-loaded run's partial journal should still Conform (no divergence)\n%s", out2)
	}
}
