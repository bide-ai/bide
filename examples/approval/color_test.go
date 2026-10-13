package main

import (
	"strings"
	"testing"
)

// TestColorRules: color is for a terminal that wants it. NO_COLOR, TERM=dumb and a Windows terminal
// not known to read ANSI escapes turn it off.
func TestColorRules(t *testing.T) {
	env := func(kv ...string) func(string) string {
		return func(k string) string {
			for i := 0; i < len(kv); i += 2 {
				if kv[i] == k {
					return kv[i+1]
				}
			}
			return ""
		}
	}
	for _, tc := range []struct {
		name string
		env  func(string) string
		goos string
		want bool
	}{
		{"a terminal", env("TERM", "xterm-256color"), "linux", true},
		{"NO_COLOR", env("TERM", "xterm", "NO_COLOR", "1"), "linux", false},
		{"TERM=dumb", env("TERM", "dumb"), "darwin", false},
		{"windows terminal", env("WT_SESSION", "x"), "windows", true},
		{"windows console", env(), "windows", false},
	} {
		if got := colorOK(tc.env, tc.goos); got != tc.want {
			t.Errorf("%s: colorOK = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestStyle: with color off a line is untouched; on, its leading words are painted after a mark.
func TestStyle(t *testing.T) {
	for _, tc := range []struct{ line, want string }{
		{"status: paused", "\x1b[33m! status: paused\x1b[0m"},
		{"recorded: risk approved (FORGED signature)", "\x1b[33m! recorded:\x1b[0m risk approved (FORGED signature)"},
		{"recorded: ops approved (signed)", "\x1b[32m✓ recorded:\x1b[0m ops approved (signed)"},
		{"proofs: PASS", "proofs: \x1b[32m✓ PASS\x1b[0m"},
		{"== An auditor verifies offline", "\x1b[1m== An auditor verifies offline\x1b[0m"},
		{"nothing to style here", "nothing to style here"},
	} {
		if got := style(false, tc.line); got != tc.line {
			t.Errorf("style off changed %q to %q", tc.line, got)
		}
		if got := style(true, tc.line); got != tc.want {
			t.Errorf("style on %q = %q, want %q", tc.line, got, tc.want)
		}
	}
}

// TestPipedErrorIsPlain: an error written to a pipe has no escape, whatever the terminal.
func TestPipedErrorIsPlain(t *testing.T) {
	bin := build(t, t.TempDir(), "approval", ".")
	out, code := run(t, bin, "nope")
	if code != 1 || !strings.HasPrefix(out, "error: unknown command") {
		t.Fatalf("exit %d, output %q", code, out)
	}
}
