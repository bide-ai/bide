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
		{"status: halted. x", "\x1b[33m! status: halted\x1b[0m. x"},
		{"status: done", "\x1b[32m✓ status: done\x1b[0m"},
		{"step 3  refund   POST /v1/refunds", "\x1b[1mstep 3  refund\x1b[0m   POST /v1/refunds"},
		{"        Stripe answered: re_1 succeeded", "        \x1b[32m✓ Stripe answered:\x1b[0m re_1 succeeded"},
		{"reconcile: resolved x", "\x1b[34m✓ reconcile: resolved\x1b[0m x"},
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
	out, code := proc(t, "http://127.0.0.1:1", "sk_test_x", "run")
	if code != 1 || !strings.HasPrefix(out, "error: run: -payment-intent") {
		t.Fatalf("exit %d, output %q", code, out)
	}
}
