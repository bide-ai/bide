//go:build !bide_audit_only

package main

import (
	"strings"
	"testing"
)

// TestFullErrorPrefix: the errors of check, view, init and demo carry the bold red prefix with
// -color=always, and none when piped.
func TestFullErrorPrefix(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{"check"},
		{"check", "-diff", "x"},
		{"check", flowPolicy + ".missing"},
		{"view"},
		{"view", "-check"},
		{"init"},
		{"init", "-module", "bad name", dir + "/a"},
		{"demo", "extra"},
	} {
		_, _, plain := runBide(args...)
		if strings.Contains(plain, "\x1b") || strings.HasPrefix(plain, "error:") {
			t.Errorf("bide %v: piped stderr is not plain:\n%q", args, plain)
		}
		_, _, colored := runBide(append([]string{args[0], "-color=always"}, args[1:]...)...)
		if !strings.Contains(colored, errPrefix+"bide ") && !strings.Contains(colored, errPrefix+flowPolicy) {
			t.Errorf("bide %v: no bold red error: prefix:\n%q", args, colored)
		}
		if got := stripANSI(colored); strings.Replace(got, "error: ", "", 1) != plain {
			t.Errorf("bide %v: colored stderr minus the prefix differs from plain:\n%q\n%q", args, got, plain)
		}
	}
	// The hint after "; " is dim.
	_, _, out := runBide("init", "-color=always", "-module", "bad name", t.TempDir()+"/a")
	if !strings.Contains(out, "is not a module path; \x1b[2mname one with -module\x1b[0m") {
		t.Errorf("hint is not dim:\n%q", out)
	}
}

// TestDoctorAndAuditErrors: doctor's usage error carries the prefix, and an audit error's hint,
// after its semicolon, is dim.
func TestDoctorAndAuditErrors(t *testing.T) {
	_, _, out := runBide("doctor", "-color=always", "extra")
	if !strings.HasPrefix(out, errPrefix+"bide doctor: unexpected argument") {
		t.Errorf("doctor: %q", out)
	}
	_, _, plain := runBide("doctor", "extra")
	if strings.Contains(plain, "\x1b") {
		t.Errorf("piped doctor error is colored: %q", plain)
	}
	_, _, out = runBide("audit", "-color=always", "verify-evidence", "-evidence", "x.json", "-pubkey", "nothex")
	if !strings.Contains(out, "; \x1b[2m") || !strings.Contains(out, errPrefix[:7]) {
		t.Errorf("audit error hint is not dim:\n%q", out)
	}
}
