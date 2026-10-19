//go:build !bide_audit_only

package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bide-ai/bide/internal/term"
)

const (
	flowPolicy = "../../examples/plan/flows/refund-policy.yaml"
	flowBypass = "../../examples/plan/flows/refund-policy-bypass.yaml"
	flowV1     = "../../examples/plan/flows/refund-v1.yaml"
	flowV2     = "../../examples/plan/flows/refund-v2.yaml"
)

// colorCases are bide check's text outputs: a violated assertion with its counterexample, a config
// whose assertions all hold, a weakened approval gate in a diff, and a strengthened one.
var colorCases = []struct {
	golden string
	args   []string
}{
	{"check-violation", []string{"check", flowBypass, flowPolicy}},
	{"diff-weakened", []string{"check", flowV1, "-diff", flowV2, "-check"}},
	{"diff-strengthened", []string{"check", flowV2, "-diff", flowV1, "-check"}},
}

// golden compares got with testdata/color/<name>.golden, or rewrites it with -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "color", name+".golden")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (generate it with go test . -run %s -update in cmd/bide)", err, t.Name())
	}
	if w := strings.ReplaceAll(string(want), "\r\n", "\n"); got != w {
		t.Errorf("%s differs from %s:\ngot:\n%s\nwant:\n%s", name, path, got, w)
	}
}

// TestCheckPlainGolden: written to a buffer (not a terminal), and with -color=never, bide check
// prints the plain lines it always printed: no escape and no mark.
func TestCheckPlainGolden(t *testing.T) {
	for _, c := range colorCases {
		_, auto, _ := runBide(c.args...)
		_, never, _ := runBide(append(c.args, "-color=never")...)
		if auto != never {
			t.Errorf("%s: the default and -color=never differ:\n%s\n%s", c.golden, auto, never)
		}
		if strings.ContainsAny(auto, "\x1b✓✗") {
			t.Errorf("%s: plain output carries an escape or a mark:\n%q", c.golden, auto)
		}
		golden(t, c.golden+".plain", auto)
	}
}

// TestCheckColorGolden: with -color=always the same lines carry color and marks, and a weakened
// gate is called out first, in bold red. A line too long for a terminal is split over indented
// lines, so the colored output is checked to keep every word of the plain output.
func TestCheckColorGolden(t *testing.T) {
	for _, c := range colorCases {
		_, out, _ := runBide(append(c.args, "-color=always")...)
		golden(t, c.golden+".color", out)
		_, plain := runBideOut(c.args...)
		if missing := missingWords(plain, stripANSI(out)); len(missing) > 0 {
			t.Errorf("%s: the colored output lacks %q of the plain output's words:\n%s", c.golden, missing, stripANSI(out))
		}
	}
	_, out, _ := runBide("check", flowBypass, "-color=always")
	for _, want := range []string{
		"\x1b[31m✗\x1b[0m ",         // a cross on the error
		"\x1b[31mviolated\x1b[0m: ", // the verdict in red
		// the assertion in one color, then the reason and the path on lines of their own
		`: "every path to charge_vip passes approval >= 2 of 3"` + "\n    ",
		"    \x1b[31mcounterexample:\x1b[0m \x1b[31mvalidate\x1b[0m", // the path in red
		"\x1b[1;31mcharge_vip\x1b[0m\n",                              // the node it reaches, in bold red
		"\x1b[31m1 violated\x1b[0m",
		"\x1b[32m4 hold\x1b[0m",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("colored check output lacks %q:\n%q", want, out)
		}
	}
	_, out, _ = runBide("check", flowPolicy, "-color=always")
	if !strings.HasPrefix(out, "\x1b[32m✓\x1b[0m ") {
		t.Errorf("assertions that all hold are not marked with a check:\n%q", out)
	}
	_, out, _ = runBide("check", flowV1, "-diff", flowV2, "-check", "-color=always")
	if first, _, _ := strings.Cut(out, "\n"); !strings.Contains(first, "\x1b[1;31mapproval policy weakened: charge: approval 2 of 3 -> 1 of 3\x1b[0m") {
		t.Errorf("the weakened gate is not the first line, in bold red:\n%q", out)
	}
	for _, want := range []string{"\x1b[32madded\x1b[0m", "\x1b[31mremoved\x1b[0m", "\x1b[33mchanged\x1b[0m", "\x1b[1;31m(weakened)\x1b[0m"} {
		if !strings.Contains(out, want) {
			t.Errorf("colored diff lacks %q:\n%q", want, out)
		}
	}
	// A risk class in a diff: side effect red, read-only and idempotent green.
	dir := t.TempDir()
	safe := filepath.Join(dir, "safe.yaml")
	writeFile(t, safe, strings.Replace(readFile(t, flowPolicy), "{name: charge, block: charge, safety: side_effect}", "{name: charge, block: charge, safety: idempotent}", 1))
	_, out, _ = runBide("check", safe, "-diff", flowPolicy, "-check", "-color=always")
	if !strings.Contains(out, "safety \x1b[31mside_effect\x1b[0m -> \x1b[32midempotent\x1b[0m") {
		t.Errorf("the safety change is not colored as the risk map colors it:\n%q", out)
	}
	if code, _, _ := runBide("check", flowPolicy, "-color=sometimes"); code != 2 {
		t.Errorf("-color=sometimes: exit %d, want 2", code)
	}
}

// TestCheckColorEnv: on a terminal (term.IsTerminal replaced), bide check colors by default, and
// not with NO_COLOR set, with TERM=dumb, or with -color=never.
func TestCheckColorEnv(t *testing.T) {
	was := term.IsTerminal
	term.IsTerminal = func(io.Writer) bool { return true }
	t.Cleanup(func() { term.IsTerminal = was })
	t.Setenv("TERM", "xterm-256color") // Windows colors only a terminal it knows reads ANSI
	t.Setenv("NO_COLOR", "")
	colored := func(args ...string) bool {
		_, out, _ := runBide(append([]string{"check", flowBypass}, args...)...)
		return strings.Contains(out, "\x1b[")
	}
	if !colored() {
		t.Error("a terminal is not colored")
	}
	if colored("-color=never") {
		t.Error("-color=never on a terminal is colored")
	}
	t.Setenv("NO_COLOR", "1")
	if colored() {
		t.Error("NO_COLOR=1 on a terminal is colored")
	}
	if !colored("-color=always") {
		t.Error("-color=always does not override NO_COLOR")
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")
	if colored() {
		t.Error("TERM=dumb is colored")
	}
}

func runBideOut(args ...string) (int, string) {
	code, out, _ := runBide(args...)
	return code, out
}

// missingWords lists the words of plain (split on spaces and punctuation) that colored lacks,
// but "is": a violated assertion reads "violated" on a terminal and "is violated" in plain text.
func missingWords(plain, colored string) []string {
	split := func(s string) []string {
		return strings.FieldsFunc(s, func(r rune) bool { return strings.ContainsRune(" \n\t\"(),;:", r) })
	}
	have := map[string]int{}
	for _, w := range split(colored) {
		have[w]++
	}
	var missing []string
	for _, w := range split(plain) {
		if w == "is" {
			continue
		}
		if have[w] == 0 {
			missing = append(missing, w)
			continue
		}
		have[w]--
	}
	return missing
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeFile(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}
