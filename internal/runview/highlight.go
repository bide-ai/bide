package runview

import (
	"bytes"
	"encoding/json"
	"html/template"
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxPayload caps the JSON one timeline row carries, so a record with a large result does not
// make the page large; the row says how much was cut.
const maxPayload = 8 << 10

// prettyJSON indents raw JSON with two spaces, cut to maxPayload bytes. Text that is not valid JSON
// is returned as it is (cut the same way), so the row still shows it.
func prettyJSON(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	s := string(raw)
	if json.Indent(&buf, raw, "", "  ") == nil {
		s = buf.String()
	}
	if len(s) > maxPayload {
		cut := maxPayload
		for cut > 0 && !utf8.RuneStart(s[cut]) { // do not split a UTF-8 sequence
			cut--
		}
		s = s[:cut] + "\n… " + strconv.Itoa(len(s)-cut) + " more bytes not shown (the JSON endpoint and the journal hold them)"
	}
	return s
}

// highlightJSON wraps the tokens of JSON text in spans for styling: object keys (jk), strings
// (jv), numbers (jn), true and false (jb), null (jz) and punctuation (jp). It scans the text and
// never parses it, so JSON cut short, or not JSON at all, still shows, and every character of the
// text is HTML-escaped and kept: stripping the tags gives the text back.
func highlightJSON(src string) template.HTML {
	var b strings.Builder
	esc := template.HTMLEscapeString
	span := func(class, text string) { b.WriteString(`<span class="` + class + `">` + esc(text) + `</span>`) }
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '"':
			j := i + 1
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					j++
				}
				j++
			}
			j = min(j+1, len(src)) // past the closing quote, when there is one
			k := j
			for k < len(src) && (src[k] == ' ' || src[k] == '\t') {
				k++
			}
			if k < len(src) && src[k] == ':' {
				span("jk", src[i:j])
			} else {
				span("jv", src[i:j])
			}
			i = j
		case c == '-' || c >= '0' && c <= '9':
			j := i + 1
			for j < len(src) && strings.IndexByte("0123456789.eE+-", src[j]) >= 0 {
				j++
			}
			span("jn", src[i:j])
			i = j
		case c >= 'a' && c <= 'z':
			j := i
			for j < len(src) && src[j] >= 'a' && src[j] <= 'z' {
				j++
			}
			switch w := src[i:j]; w {
			case "true", "false":
				span("jb", w)
			case "null":
				span("jz", w)
			default:
				b.WriteString(esc(w))
			}
			i = j
		case strings.IndexByte("{}[],:", c) >= 0:
			span("jp", src[i:i+1])
			i++
		default:
			// Whitespace, and anything else: kept, escaped, one UTF-8 sequence at a time.
			j := i + 1
			for j < len(src) && src[j]&0xC0 == 0x80 {
				j++
			}
			b.WriteString(esc(src[i:j]))
			i = j
		}
	}
	return template.HTML(b.String()) // every character of src went through HTMLEscapeString
}
