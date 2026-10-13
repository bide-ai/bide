package main

import (
	"os"
	"regexp"
	"runtime"
)

// Color for what this example prints. It is on only when the output is a terminal, NO_COLOR is
// unset and TERM is not "dumb" (on Windows, only in a terminal known to read ANSI escapes), so a
// pipe, a CI log and go test read plain text. A colored line adds a mark (✓ ! ✗) beside its words,
// so nothing is said by color alone. This file is a copy of the small helper in bide init's scaffold
// (the examples are separate modules, so they cannot import bide's internal/term).

var (
	colorOut = isTerminal(os.Stdout) && colorOK(os.Getenv, runtime.GOOS)
	colorErr = isTerminal(os.Stderr) && colorOK(os.Getenv, runtime.GOOS)
)

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// colorOK reports whether a terminal should get color, given its environment.
func colorOK(getenv func(string) string, goos string) bool {
	if getenv("NO_COLOR") != "" || getenv("TERM") == "dumb" {
		return false
	}
	return goos != "windows" || getenv("WT_SESSION") != "" || getenv("TERM_PROGRAM") == "vscode"
}

// rule paints the text its first group matches, after mark, in the SGR code: 32 green (done,
// recorded), 33 amber (paused, halted), 34 blue (reconciled), 31 red (refused, failed), 1 bold.
type rule struct {
	re         *regexp.Regexp
	code, mark string
}

// rules style a line by how it begins; the first that matches wins.
var rules = []rule{
	{regexp.MustCompile(`^(step \d  \w+)`), "1", ""},
	{regexp.MustCompile(`^(status: (?:halted|paused))`), "33", "!"},
	{regexp.MustCompile(`^(status: done)`), "32", "✓"},
	{regexp.MustCompile(`^(reconcile: resolved)`), "34", "✓"},
	{regexp.MustCompile(`^(reconcile: no refund)`), "33", "!"},
	{regexp.MustCompile(`^(reconcile:)`), "34", ""},
	{regexp.MustCompile(`^ +(Stripe answered:)`), "32", "✓"},
	{regexp.MustCompile(`^ +(-crash-after-send:)`), "1;31", "✗"},
	{regexp.MustCompile(`^(refused:)`), "31", "✗"},
	{regexp.MustCompile(`^(recorded:)`), "32", "✓"},
}

// style is line with its leading words painted when on; line itself when off or no rule matches.
func style(on bool, line string) string {
	if !on {
		return line
	}
	for _, r := range rules {
		if m := r.re.FindStringSubmatchIndex(line); m != nil {
			word := line[m[2]:m[3]]
			if r.mark != "" {
				word = r.mark + " " + word
			}
			return line[:m[2]] + "\x1b[" + r.code + "m" + word + "\x1b[0m" + line[m[3]:]
		}
	}
	return line
}

// errorLabel is "error:" for stderr, bold red on a terminal.
func errorLabel() string {
	if !colorErr {
		return "error:"
	}
	return "\x1b[1;31m✗ error:\x1b[0m"
}
