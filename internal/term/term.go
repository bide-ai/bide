// Package term styles the bide command's text output for a terminal: a check mark and green for
// what passed, a cross and red for what failed, amber for a warning or a gate, bold cyan for a path
// or address to act on, dim for secondary detail. Status is never carried by color alone: a styled
// line keeps its words, and adds a mark (✓ or ✗) beside them.
//
// Color is on for a terminal only (an *os.File that is a character device), and off when NO_COLOR
// is set to anything or TERM is dumb. On Windows it is on only where the terminal is known to read
// ANSI escapes (Windows Terminal, ConEmu, ANSICON, VS Code, or a TERM such as xterm-256color). A
// -color=always or -color=never flag overrides all of that. Output to a file or a pipe is plain, so
// a script or CI log reads the same text it always did.
//
// The package is the standard library only, so bide audit's build stays core-only.
package term

import (
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
)

// Mode is the -color flag: auto (the default), always or never.
type Mode int

const (
	// Auto colors a terminal, unless NO_COLOR is set or TERM is dumb.
	Auto Mode = iota
	// Always colors whatever the output is.
	Always
	// Never colors nothing.
	Never
)

// FlagUsage is the help text of every -color flag.
const FlagUsage = "`when` to color the output: auto (the default: on a terminal, unless NO_COLOR is set or TERM is dumb), always or never"

// String is the mode as -color reads it.
func (m Mode) String() string {
	switch m {
	case Always:
		return "always"
	case Never:
		return "never"
	}
	return "auto"
}

// Set reads a -color value.
func (m *Mode) Set(s string) error {
	switch s {
	case "auto":
		*m = Auto
	case "always":
		*m = Always
	case "never":
		*m = Never
	default:
		return fmt.Errorf("want auto, always or never, got %q", s)
	}
	return nil
}

// Flag defines a -color flag on fs and returns its value.
func Flag(fs *flag.FlagSet) *Mode {
	m := new(Mode)
	fs.Var(m, "color", FlagUsage)
	return m
}

// For is the style for output written to w under mode m.
func For(w io.Writer, m Mode) Style {
	return Style{on: enabled(m, IsTerminal(w), os.Getenv, runtime.GOOS)}
}

// New is a style with color on or off, for a caller that has already decided.
func New(on bool) Style { return Style{on: on} }

// enabled decides whether to color: the flag first, then the terminal, NO_COLOR, TERM and, on
// Windows, whether the terminal is known to read ANSI escapes.
func enabled(m Mode, tty bool, getenv func(string) string, goos string) bool {
	switch m {
	case Always:
		return true
	case Never:
		return false
	}
	if !tty || getenv("NO_COLOR") != "" {
		return false
	}
	t := getenv("TERM")
	if t == "dumb" {
		return false
	}
	if goos == "windows" {
		return getenv("WT_SESSION") != "" || getenv("ConEmuANSI") == "ON" || getenv("ANSICON") != "" ||
			getenv("TERM_PROGRAM") == "vscode" || strings.HasPrefix(t, "xterm") ||
			strings.Contains(t, "color") || strings.Contains(t, "ansi")
	}
	return true
}

// IsTerminal reports whether w is a terminal. A test of a command's terminal output replaces it,
// to check what NO_COLOR and TERM do on a terminal without opening one.
var IsTerminal = isTerminal

// isTerminal reports whether w is a terminal: an *os.File that is a character device.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// Style styles text, or leaves it as it is when color is off. The zero Style is off.
type Style struct{ on bool }

// On reports whether s colors.
func (s Style) On() bool { return s.on }

func (s Style) sgr(code, text string) string {
	if !s.on || text == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

// OK is what passed or verified: green.
func (s Style) OK(t string) string { return s.sgr("32", t) }

// Fail is what failed or was violated: red.
func (s Style) Fail(t string) string { return s.sgr("31", t) }

// Warn is a warning, a halt or an approval gate: amber.
func (s Style) Warn(t string) string { return s.sgr("33", t) }

// Path is a path, an address or a command to act on: bold cyan.
func (s Style) Path(t string) string { return s.sgr("1;36", t) }

// Dim is secondary detail: a digest, a count, a file name repeated on every line.
func (s Style) Dim(t string) string { return s.sgr("2", t) }

// Heading is a heading: bold.
func (s Style) Heading(t string) string { return s.sgr("1", t) }

// Bold is bold, for emphasis no role names.
func (s Style) Bold(t string) string { return s.sgr("1", t) }

// Blue is blue, for the one state the roles do not cover: a halt resolved.
func (s Style) Blue(t string) string { return s.sgr("34", t) }

// Cyan is cyan, for a neutral step.
func (s Style) Cyan(t string) string { return s.sgr("36", t) }

// BoldOK is a verdict that holds: bold green.
func (s Style) BoldOK(t string) string { return s.sgr("1;32", t) }

// BoldFail is a verdict that does not hold, or a weakened gate: bold red.
func (s Style) BoldFail(t string) string { return s.sgr("1;31", t) }

// Pass is a green check mark and a space when color is on, and nothing when it is off: a colored
// line carries a mark beside its words, a plain line stays as it was.
func (s Style) Pass() string {
	if !s.on {
		return ""
	}
	return s.OK("✓") + " "
}

// Cross is a red cross and a space when color is on, and nothing when it is off.
func (s Style) Cross() string {
	if !s.on {
		return ""
	}
	return s.Fail("✗") + " "
}

// Bang is an amber exclamation mark and a space when color is on, and nothing when it is off.
func (s Style) Bang() string {
	if !s.on {
		return ""
	}
	return s.Warn("!") + " "
}
