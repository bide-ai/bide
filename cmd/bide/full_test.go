//go:build !bide_audit_only

package main

import (
	"strings"
	"testing"
)

// TestCheck: bide check exits 0 on a clean config, 1 on one with an error (printing file:node:
// message lines), takes several files, and exits 2 with no file.
func TestCheck(t *testing.T) {
	const clean, typo = "../../examples/plan/flows/refund.yaml", "../../examples/plan/flows/refund-typo.yaml"
	if code, stdout, stderr := runBide("check", clean); code != 0 {
		t.Errorf("bide check %s: exit %d, want 0\n%s%s", clean, code, stdout, stderr)
	}
	code, stdout, stderr := runBide("check", clean, typo)
	if code != 1 || !strings.Contains(stdout, typo+":") {
		t.Errorf("bide check %s %s: exit %d, want 1 and a %s: line\n%s%s", clean, typo, code, typo, stdout, stderr)
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
