//go:build !bide_audit_only

package main

import (
	"strings"
	"testing"
)

// TestCheck: bide check exits 0 on a clean config, 1 on one with an error (printing file:node:
// message lines) or a violated assertion (printing its counterexample), takes several files, and
// exits 2 with no file.
func TestCheck(t *testing.T) {
	const clean, typo = "../../examples/plan/flows/refund.yaml", "../../examples/plan/flows/refund-typo.yaml"
	if code, stdout, stderr := runBide("check", clean); code != 0 {
		t.Errorf("bide check %s: exit %d, want 0\n%s%s", clean, code, stdout, stderr)
	}
	code, stdout, stderr := runBide("check", clean, typo)
	if code != 1 || !strings.Contains(stdout, typo+":") {
		t.Errorf("bide check %s %s: exit %d, want 1 and a %s: line\n%s%s", clean, typo, code, typo, stdout, stderr)
	}
	const policy, bypass = "../../examples/plan/flows/refund-policy.yaml", "../../examples/plan/flows/refund-policy-bypass.yaml"
	if code, stdout, stderr := runBide("check", "-strict", policy); code != 0 || !strings.Contains(stdout, "4 assertions: 4 hold") {
		t.Errorf("bide check %s: exit %d, want 0 and 4 holding assertions\n%s%s", policy, code, stdout, stderr)
	}
	code, stdout, stderr = runBide("check", bypass)
	if code != 1 || !strings.Contains(stdout, "is violated") || !strings.Contains(stdout, "(counterexample: validate -> lookup -> charge_vip)") {
		t.Errorf("bide check %s: exit %d, want 1 and the counterexample\n%s%s", bypass, code, stdout, stderr)
	}
	if code, _, _ := runBide("check"); code != 2 {
		t.Errorf("bide check with no file: exit %d, want 2", code)
	}
	if code, _, _ := runBide("check", "-strict"); code != 2 {
		t.Errorf("bide check -strict with no file: exit %d, want 2", code)
	}
}

// TestInitThenCheck: the flow config bide init writes passes bide check.
func TestInitThenCheck(t *testing.T) {
	dir := t.TempDir() + "/refund"
	if code, stdout, stderr := runBide("init", "-dev", "../..", dir); code != 0 {
		t.Fatalf("bide init: exit %d\n%s%s", code, stdout, stderr)
	}
	if code, stdout, stderr := runBide("check", "-strict", dir+"/flow.yaml"); code != 0 {
		t.Fatalf("bide check -strict on the scaffold: exit %d\n%s%s", code, stdout, stderr)
	}
}

// TestCheckDiff: bide check <file> -diff <old> -check (flags after the file) prints the changes and
// exits 0, a weakened gate included; -fail-on-weaken exits 1 on the weakened gate and 0 on the
// strengthened one; -check without -diff, and -diff with two files, are usage errors.
func TestCheckDiff(t *testing.T) {
	const v1, v2 = "../../examples/plan/flows/refund-v1.yaml", "../../examples/plan/flows/refund-v2.yaml"
	code, stdout, stderr := runBide("check", v2, "-diff", v1, "-check")
	if code != 0 || !strings.Contains(stdout, v2+": changed node charge: approval 1 of 3 -> 2 of 3 (strengthened)\n") ||
		!strings.Contains(stdout, v2+": added node review: block review; safety readonly\n") || !strings.Contains(stdout, v2+": removed node skip: block skip\n") {
		t.Errorf("bide check %s -diff %s -check: exit %d\n%s%s", v2, v1, code, stdout, stderr)
	}
	if code, stdout, stderr := runBide("check", v2, "-diff", v1, "-check", "-fail-on-weaken"); code != 0 {
		t.Errorf("a strengthened gate with -fail-on-weaken: exit %d, want 0\n%s%s", code, stdout, stderr)
	}
	code, stdout, stderr = runBide("check", v1, "-diff", v2, "-check")
	if code != 0 || !strings.Contains(stdout, v1+": approval policy weakened: charge: approval 2 of 3 -> 1 of 3\n") {
		t.Errorf("a weakened gate: exit %d, want 0 and the weakened line\n%s%s", code, stdout, stderr)
	}
	if code, stdout, stderr := runBide("check", "-fail-on-weaken", v1, "-check", "-diff", v2); code != 1 {
		t.Errorf("a weakened gate with -fail-on-weaken: exit %d, want 1\n%s%s", code, stdout, stderr)
	}
	for _, args := range [][]string{
		{"check", v1, "-check"},
		{"check", v1, "-fail-on-weaken"},
		{"check", v1, v2, "-diff", v1, "-check"},
		{"check", "-diff", v1},
	} {
		if code, _, _ := runBide(args...); code != 2 {
			t.Errorf("bide %q: exit %d, want 2", args, code)
		}
	}
	// Everything after -- is a file: -missing.yaml is read (and is not there), not parsed as a flag.
	if code, stdout, stderr := runBide("check", "--", v1, "-missing.yaml"); code != 1 || !strings.Contains(stderr, "-missing.yaml") {
		t.Errorf("bide check -- %s -missing.yaml: exit %d, want 1\n%s%s", v1, code, stdout, stderr)
	}
}

// TestCheckDiffGateBypassBroad: -fail-on-weaken exits 1 when a node every path reached through an
// approval gate becomes reachable around it, whatever its safety: an idempotent node that moves
// money, and the same edit relabeling the node read-only.
func TestCheckDiffGateBypassBroad(t *testing.T) {
	const dir = "testdata/gatebypass/"
	for _, c := range []struct{ new, old, want string }{
		{"new-idempotent.yaml", "old-idempotent.yaml", "credit: reachable without an approval gate (validate -> lookup -> credit), not before; its safety is idempotent"},
		{"new-relabeled.yaml", "old-effect.yaml", "credit: reachable without an approval gate (validate -> lookup -> credit), not before; its safety is read-only; safety lowered side effect -> read-only"},
	} {
		code, stdout, stderr := runBide("check", dir+c.new, "-diff", dir+c.old, "-check", "-fail-on-weaken")
		if code != 1 || !strings.Contains(stdout, "weakened: "+c.want+"\n") {
			t.Errorf("%s against %s: exit %d, want 1 and %q\n%s%s", c.new, c.old, code, c.want, stdout, stderr)
		}
	}
}
