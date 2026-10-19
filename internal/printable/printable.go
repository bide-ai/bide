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
// refuses every character with the Unicode property Default_Ignorable_Code_Point, which is what a
// renderer is allowed to draw as nothing (the Hangul and Khmer fillers, the grapheme joiner, the
// variation selectors, the Mongolian selectors, the format characters), U+2800 (the Braille blank),
// and a combining mark at the start with nothing to combine with. A space at either end counts, as
// the first or last character.
//
// Default_Ignorable_Code_Point is derived as the Unicode standard derives it (PropList.txt):
// Other_Default_Ignorable_Code_Point, plus the format characters (general category Cf), plus the
// variation selectors, minus white space, the interlinear annotation characters U+FFF9 to U+FFFB,
// the Egyptian hieroglyph format controls U+13430 to U+13440 and the prepended concatenation marks;
// the Cf characters are refused by IsPrint already, so the set added here is the other two. It comes
// from the tables of the Go toolchain that built this program (Unicode UnicodeVersion).
func Bad(s string) (r rune, ok bool) {
	for i, c := range s {
		if !unicode.IsPrint(c) || ignorable(c) {
			return c, true
		}
		if i == 0 && unicode.IsMark(c) {
			return c, true // a mark with nothing to combine with
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

// UnicodeVersion is the version of the Unicode tables the check uses.
const UnicodeVersion = unicode.Version

// ignorable is a character that is not drawn: Default_Ignorable_Code_Point outside general category
// Cf (see Bad), or U+2800.
func ignorable(c rune) bool {
	return c == 0x2800 || unicode.Is(unicode.Other_Default_Ignorable_Code_Point, c) || unicode.Is(unicode.Variation_Selector, c)
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
		case !unicode.IsPrint(c) || ignorable(c):
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
	return !unicode.IsPrint(c) || ignorable(c)
}

// ansi is a terminal color sequence, which Writer passes through.
var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// partialANSI is the start of a color sequence with its end still to come.
var partialANSI = regexp.MustCompile(`^\x1b(\[[0-9;]*)?$`)

// Writer is w with Escape applied to everything written to it, except the terminal color sequences
// the command's own styling writes (ESC [ ... m). It is the second line of defence: a name that
// reached the output by a path that did not refuse it is still shown in a form that cannot spoof.
//
// The writer holds back the tail of a character or of a color sequence a write ends in the middle
// of, and writes it with the next write; Close writes what is still held (escaped, as the invalid text
// it is), so nothing is dropped.
func Writer(w io.Writer) *EscapingWriter { return &EscapingWriter{w: w} }

// escWriter keeps the start of a character a Write ended in the middle of, and escapes it with the
// rest on the next Write, so a character split across two writes is not taken for two bad ones.
// EscapingWriter is the writer Writer returns. Close writes what is still held back (the start of a
// character or color sequence the last write ended in) with each unfinished byte written out, and does
// not close the writer underneath.
type EscapingWriter struct {
	w    io.Writer
	rest []byte
}

func (e *EscapingWriter) Write(p []byte) (int, error) {
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

// Close writes what is held back, escaped as invalid text.
func (e *EscapingWriter) Close() error {
	if len(e.rest) == 0 {
		return nil
	}
	rest := e.rest
	e.rest = nil
	var b strings.Builder
	for _, c := range rest {
		if c == 0x1b {
			b.WriteString(`\u{1B}`)
			continue
		}
		fmt.Fprintf(&b, `\x{%02X}`, c)
	}
	_, err := io.WriteString(e.w, b.String())
	return err
}
