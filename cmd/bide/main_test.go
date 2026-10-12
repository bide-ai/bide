package main

import (
	"bytes"
	"strings"
	"testing"
)

func runBide(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// TestDispatch: the command list, help and usage errors behave the same for every command.
func TestDispatch(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want int
		out  string // in stdout+stderr
	}{
		{nil, 0, "Commands:"},
		{[]string{"help"}, 0, "Commands:"},
		{[]string{"-h"}, 0, "Commands:"},
		{[]string{"nope"}, 2, `unknown command "nope"`},
		{[]string{"help", "nope"}, 2, `unknown command "nope"`},
		{[]string{"help", "version", "extra"}, 2, "unknown command"},
		{[]string{"help", "audit", "verify"}, 0, "-bundle"},
		{[]string{"audit", "verify", "-h"}, 2, "-bundle"}, // audit reserves 0 for verified
		{[]string{"version", "extra"}, 2, `unexpected argument "extra"`},
		{[]string{"version"}, 0, "artifact formats read:"},
		{[]string{"version", "-json"}, 0, `"formats"`},
	} {
		code, stdout, stderr := runBide(tc.args...)
		if code != tc.want || !strings.Contains(stdout+stderr, tc.out) {
			t.Errorf("bide %s: exit %d, want %d, and output containing %q:\n%s%s", strings.Join(tc.args, " "), code, tc.want, tc.out, stdout, stderr)
		}
	}
}

// TestEveryCommandHelp: bide help <command> and bide <command> -h print the command's usage and
// flags and its exit statuses, and exit 0, for every command, audit included.
func TestEveryCommandHelp(t *testing.T) {
	for _, c := range commands {
		for _, args := range [][]string{{"help", c.name}, {c.name, "-h"}} {
			code, stdout, stderr := runBide(args...)
			if code != 0 || !strings.Contains(stdout, "Exit status:") || !strings.Contains(stdout, "-") {
				t.Errorf("bide %s: exit %d, want 0 and the usage and exit statuses:\n%s%s", strings.Join(args, " "), code, stdout, stderr)
			}
		}
	}
}

// TestVersionLine: bide version names the program bide.
func TestVersionLine(t *testing.T) {
	if _, stdout, _ := runBide("version"); !strings.HasPrefix(stdout, "bide ") {
		t.Fatalf("bide version printed %q", stdout)
	}
}
