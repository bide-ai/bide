// Package printable is the rule for text a reviewer reads as an identifier (a node, block or flow
// name): every character prints. A control character, an invisible or direction-changing format
// character (a zero-width space, a joiner, a bidirectional override), a space other than the plain
// one, a private-use or unassigned code point, and a space at either end are what let one name read
// as another in a diff, so a config that holds one is refused (package plan) and output that would
// show one writes it out as \u{XXXX} instead (Escape, Writer). The package is the standard library
// only, so bide audit's build stays core-only.
package printable

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Bad returns the first character of s that does not print, and whether there is one. A space at
// either end counts, as the first or last character.
func Bad(s string) (r rune, ok bool) {
	for _, c := range s {
		if !unicode.IsPrint(c) {
			return c, true
		}
	}
	if s != "" {
		if c := s[0]; c == ' ' {
			return ' ', true
		}
		if c := s[len(s)-1]; c == ' ' {
			return ' ', true
		}
	}
	return 0, false
}

// Describe is r as a reader should see it: U+202E.
func Describe(r rune) string { return fmt.Sprintf("U+%04X", r) }

// Escape is s with every character that does not print written as \u{XXXX}. A plain space, a line
// break and a tab stay (text has lines and columns); everything else that does not print is written
// out.
func Escape(s string) string {
	if !strings.ContainsFunc(s, needsEscape) {
		return s
	}
	var b strings.Builder
	for _, c := range s {
		if needsEscape(c) {
			fmt.Fprintf(&b, `\u{%X}`, c)
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

// EscapeName is Escape for an identifier: a line break and a tab are written out too, since a name
// has neither.
func EscapeName(s string) string {
	var b strings.Builder
	for _, c := range s {
		if !unicode.IsPrint(c) {
			fmt.Fprintf(&b, `\u{%X}`, c)
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

func needsEscape(c rune) bool {
	switch c {
	case '\n', '\r', '\t':
		return false
	}
	return !unicode.IsPrint(c)
}

// ansi is a terminal color sequence, which Writer passes through.
var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// Writer is w with Escape applied to everything written to it, except the terminal color sequences
// the command's own styling writes (ESC [ ... m). It is the second line of defence: a name that
// reached the output by a path that did not refuse it is still shown in a form that cannot spoof.
func Writer(w io.Writer) io.Writer { return &escWriter{w: w} }

// escWriter keeps the start of a character a Write ended in the middle of, and escapes it with the
// rest on the next Write, so a character split across two writes is not taken for two bad ones.
type escWriter struct {
	w    io.Writer
	rest []byte
}

func (e *escWriter) Write(p []byte) (int, error) {
	buf := append(e.rest, p...)
	e.rest = nil
	cut := len(buf)
	for i := len(buf) - 1; i >= 0 && i >= len(buf)-3; i-- {
		if utf8.RuneStart(buf[i]) {
			if !utf8.FullRune(buf[i:]) {
				cut = i
			}
			break
		}
	}
	e.rest = append([]byte(nil), buf[cut:]...)
	s := string(buf[:cut])
	var b strings.Builder
	last := 0
	for _, m := range ansi.FindAllStringIndex(s, -1) {
		b.WriteString(Escape(s[last:m[0]]))
		b.WriteString(s[m[0]:m[1]])
		last = m[1]
	}
	b.WriteString(Escape(s[last:]))
	if _, err := io.WriteString(e.w, b.String()); err != nil {
		return 0, err
	}
	return len(p), nil
}
