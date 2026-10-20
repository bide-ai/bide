// Package themeaccess is what the module's own commands may do with the theme package that a host
// may not: write a theme's stylesheet and print text from a theme file safely. The theme package is
// public, so it exports neither; it registers the stylesheet generators here when it is loaded, and
// the pages' servers and the approve page's build call them through this package.
package themeaccess

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Escape returns s with every rune that is not printable (a control character, an escape, a
// newline, a byte that is not UTF-8) written as a Go escape, so text from a hostile file is safe to
// print and cannot start a line of its own.
func Escape(s string) string {
	var b strings.Builder
	for i, r := range s {
		switch {
		case r == utf8.RuneError && s[i] != 0xEF: // an invalid byte, not a written U+FFFD
			fmt.Fprintf(&b, "\\x%02x", s[i])
		case unicode.IsPrint(r):
			b.WriteRune(r)
		default:
			b.WriteString(strings.Trim(strconv.QuoteRune(r), "'"))
		}
	}
	return b.String()
}

var (
	css       func([]any) string
	cssDef    func([]any, string) string
	systemCSS func([]any) string
)

// Register is called once by the theme package's init with its generators, which take the themes as
// any because this package cannot import theme (theme imports it).
func Register(c func([]any) string, d func([]any, string) string, s func([]any) string) {
	css, cssDef, systemCSS = c, d, s
}

func widen[T any](themes []T) []any {
	a := make([]any, len(themes))
	for i, t := range themes {
		a[i] = t
	}
	return a
}

// CSS is the stylesheet of the checked themes (see theme.css in the theme package). T is
// *theme.Theme; any other type panics.
func CSS[T any](themes []T) string { return css(widen(themes)) }

// CSSDefault is CSS with def, the theme a reader starts in, as the :root block. A def that names no
// theme in themes panics.
func CSSDefault[T any](themes []T, def string) string { return cssDef(widen(themes), def) }

// SystemCSS is the stylesheet of a page that takes no theme input: the shipped themes picked by the
// reader's system settings alone.
func SystemCSS[T any](themes []T) string { return systemCSS(widen(themes)) }
