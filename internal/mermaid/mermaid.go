// Package mermaid writes text into a Mermaid diagram as data.
package mermaid

import (
	"strconv"
	"strings"
)

// Label returns s as a quoted Mermaid label, "..." with each character that could end the label,
// start a statement, or be read as markup written as a Mermaid entity code (#<decimal>;): the
// double quote, '#' (which starts an entity), '&' and '<' (markup in an HTML label), '`' (which
// opens a Markdown string), every control character (a line break ends a statement), and every
// byte that is not valid UTF-8 (as U+FFFD). Mermaid does
// not honour backslash escapes, so a label written with Go's %q could be ended by a quote in s.
// The label renders as s.
func Label(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		if r == '"' || r == '#' || r == '&' || r == '<' || r == '`' || r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == '\uFFFD' {
			b.WriteByte('#')
			b.WriteString(strconv.Itoa(int(r)))
			b.WriteByte(';')
			continue
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}
