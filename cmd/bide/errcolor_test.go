package main

import (
	"io"
	"strings"
	"testing"

	"github.com/bide-ai/bide/internal/term"
)

const errPrefix = "\x1b[1;31merror:\x1b[0m "

// onTerminal makes every writer a terminal for the test, with a TERM that colors.
func onTerminal(t *testing.T) {
	t.Helper()
	was := term.IsTerminal
	term.IsTerminal = func(io.Writer) bool { return true }
	t.Cleanup(func() { term.IsTerminal = was })
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "")
}

// TestErrorPrefix: on a terminal an error on stderr opens with a bold red "error:"; piped, the
// line is the one it always was.
func TestErrorPrefix(t *testing.T) {
	cases := [][]string{
		{"nope"},
		{"help", "nope"},
		{"version", "extra"},
		{"audit", "nope"},
		{"audit", "-version", "x"},
	}
	for _, args := range cases {
		_, _, plain := runBide(args...)
		if strings.ContainsAny(plain, "\x1b") || strings.HasPrefix(plain, "error:") && args[0] != "audit" {
			t.Errorf("bide %v: piped stderr carries color or a prefix:\n%q", args, plain)
		}
	}
	onTerminal(t)
	for _, args := range cases {
		_, _, colored := runBide(args...)
		if !strings.Contains(colored, errPrefix) {
			t.Errorf("bide %v: no bold red error: prefix on a terminal:\n%q", args, colored)
		}
		t.Setenv("NO_COLOR", "1")
		_, _, off := runBide(args...)
		t.Setenv("NO_COLOR", "")
		if strings.Contains(off, "\x1b") {
			t.Errorf("bide %v: NO_COLOR left color on:\n%q", args, off)
		}
	}
}

// TestPipedErrorsUnchanged pins the text of piped errors.
func TestPipedErrorsUnchanged(t *testing.T) {
	_, _, got := runBide("nope")
	if !strings.HasPrefix(got, "bide: unknown command \"nope\"\n\nbide: the accountability layer for AI agents that move money. Usage:\n") {
		t.Errorf("unknown command, piped:\n%q", got)
	}
	_, _, got = runBide("version", "extra")
	if !strings.HasPrefix(got, "bide version: unexpected argument \"extra\"\n") {
		t.Errorf("version extra, piped:\n%q", got)
	}
}

// TestCommandListColor: on a terminal the headings and command names are bold and the summaries
// dim; piped, the list is plain, and the words are the same.
func TestCommandListColor(t *testing.T) {
	_, plain, _ := runBide()
	if strings.Contains(plain, "\x1b") {
		t.Fatalf("piped command list is colored:\n%q", plain)
	}
	onTerminal(t)
	for _, args := range [][]string{nil, {"help"}} {
		_, out, _ := runBide(args...)
		for _, want := range []string{
			"\x1b[1mUsage:\x1b[0m",
			"\x1b[1mCommands:\x1b[0m",
			"  \x1b[1maudit\x1b[0m    \x1b[2mproduce and verify audit proofs offline\x1b[0m\n",
			"\x1b[1mbide help <command>\x1b[0m",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("bide %v lacks %q:\n%q", args, want, out)
			}
		}
		if got := stripANSI(out); got != plain {
			t.Errorf("bide %v: stripped of color it differs from the piped list:\n%q\n%q", args, got, plain)
		}
	}
	// A command's own -h text stays plain apart from our exit-status heading.
	_, out, _ := runBide("help", "version")
	if !strings.Contains(out, "\x1b[1mExit status:\x1b[0m") {
		t.Errorf("exit status heading is not bold:\n%q", out)
	}
	if strings.Contains(strings.Split(out, "\x1b[1mExit status")[0], "\x1b") {
		t.Errorf("flag help is styled:\n%q", out)
	}
	_, out, _ = runBide("version")
	if !strings.HasPrefix(out, "\x1b[1mbide\x1b[0m ") || !strings.Contains(out, "\x1b[2m(") {
		t.Errorf("version is not lightly styled:\n%q", out)
	}
	if got := stripANSI(out); got != func() string { t.Setenv("NO_COLOR", "1"); _, o, _ := runBide("version"); return o }() {
		t.Errorf("version stripped of color differs from plain:\n%q", got)
	}
}

// stripANSI drops the SGR escapes from s.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
