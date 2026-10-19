// Package printable is the rule for text a reviewer reads as an identifier (a node, block or flow
// name): every character prints. A control character, an invisible or direction-changing format
// character (a zero-width space, a joiner, a bidirectional override), a space other than the plain
// one, a private-use or unassigned code point, and a space at either end are what let one name read
// as another in a diff, so a config that holds one is refused (package plan) and output that would
// show one writes it out as \u{XXXX} instead (Escape, Writer). The package is the standard library
// only, so bide audit's build stays core-only.
package printable

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Bad returns the first character of s that does not print, and whether there is one. Besides the
// characters unicode.IsPrint refuses (control, format, non-plain spaces, private-use, unassigned), it
// refuses the ones that print as nothing: the Default_Ignorable fillers and joiners (U+034F, U+115F,
// U+1160, U+17B4, U+17B5, U+180B to U+180F, U+2800, U+3164, U+FFA0), a variation selector that does
// not follow a base character, and a combining mark with nothing to combine with at the start. A
// space at either end counts, as the first or last character.
func Bad(s string) (r rune, ok bool) {
	prevBase := false
	for i, c := range s {
		if !unicode.IsPrint(c) || blank(c) {
			return c, true
		}
		if i == 0 && unicode.IsMark(c) {
			return c, true // a mark with nothing to combine with, or a variation selector with no base
		}
		if isVariationSelector(c) && !prevBase {
			return c, true
		}
		prevBase = unicode.In(c, unicode.L, unicode.N, unicode.P, unicode.S)
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

// blank is a character that is assigned, is not a space or a format character, and still draws as
// nothing (or as a blank): it can stand in for a space or an empty name.
func blank(c rune) bool {
	switch c {
	case 0x034F, 0x115F, 0x1160, 0x17B4, 0x17B5, 0x2800, 0x3164, 0xFFA0:
		return true
	}
	return c >= 0x180B && c <= 0x180F
}

func isVariationSelector(c rune) bool {
	return c >= 0xFE00 && c <= 0xFE0F || c >= 0xE0100 && c <= 0xE01EF
}

// Describe is r as a reader should see it: U+202E.
func Describe(r rune) string { return fmt.Sprintf("U+%04X", r) }

// Escape is s with every character that does not print written as \u{XXXX}. A plain space, a line
// break and a tab stay (text has lines and columns); everything else that does not print is written
// out.
func Escape(s string) string {
	if utf8.ValidString(s) && !strings.ContainsFunc(s, needsEscape) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		c, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case c == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x{%02X}`, s[i]) // a byte that is not UTF-8
		case needsEscape(c):
			fmt.Fprintf(&b, `\u{%X}`, c)
		default:
			b.WriteRune(c)
		}
		i += size
	}
	return b.String()
}

// EscapeName is Escape for an identifier: a line break and a tab are written out too, since a name
// has neither.
func EscapeName(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		c, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case c == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x{%02X}`, s[i])
		case !unicode.IsPrint(c) || blank(c):
			fmt.Fprintf(&b, `\u{%X}`, c)
		default:
			b.WriteRune(c)
		}
		i += size
	}
	return b.String()
}

func needsEscape(c rune) bool {
	switch c {
	case '\n', '\r', '\t':
		return false
	}
	return !unicode.IsPrint(c) || blank(c)
}

// ansi is a terminal color sequence, which Writer passes through.
var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// partialANSI is the start of a color sequence with its end still to come.
var partialANSI = regexp.MustCompile(`^\x1b(\[[0-9;]*)?$`)

// Writer is w with Escape applied to everything written to it, except the terminal color sequences
// the command's own styling writes (ESC [ ... m). It is the second line of defence: a name that
// reached the output by a path that did not refuse it is still shown in a form that cannot spoof.
//
// Each write should hold whole characters and whole color sequences, or the rest of one in the next
// write: the writer holds back the tail of a character or of a color sequence a write ends in the
// middle of, and writes it with the next. A write that ends the stream in the middle of one loses it.
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
	// A color sequence cut in two by a write is held until the rest comes.
	if i := bytes.LastIndexByte(buf[:cut], 0x1b); i >= 0 && partialANSI.Match(buf[i:cut]) {
		cut = i
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
