package approvecmd

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/approveline"
)

// What bide approve shows is read from the journal, and the journal holds text a model wrote: a
// tool name, the arguments of the call. Shown raw, it could carry a terminal escape sequence that
// repaints the screen, or a right-to-left override that reorders what the approver reads, and so
// make the approver sign something other than what they saw. Everything that came from the journal
// goes through safe before it is printed.

// safe returns s with every character that could change how a terminal shows the text written as
// an escape: control characters other than a newline, the Unicode format characters (bidirectional
// overrides, zero-width joiners, the byte order mark, the soft hyphen), line and paragraph
// separators, the fillers that render as blank (Hangul and braille), and bytes that are not valid
// UTF-8 (approveline.Unsafe is the one rule, shared with the command lines bide approve prints). An escape is \uXXXX (or \UXXXXXXXX, or \xXX for a byte), so the
// reader sees that something is there.
func safe(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n <= 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case r == '\n':
			b.WriteByte('\n')
		case approveline.Unsafe(r):
			if r > 0xFFFF {
				fmt.Fprintf(&b, `\U%08x`, r)
			} else {
				fmt.Fprintf(&b, `\u%04x`, r)
			}
		default:
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

// Limits on the arguments shown. They bound what a call with huge arguments puts on a terminal; the
// signature binds all of the arguments whether or not they are shown, and -full shows them all.
const (
	maxArgLines    = 40
	maxArgLineRune = 200
)

// prettyArgs returns the call's arguments as indented JSON, read from the journal's bytes (object
// keys sorted, numbers kept as written), and safe to print. Bytes that are not one JSON value are
// shown as a quoted string, since the signature binds them as they are.
func prettyArgs(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "{}"
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err == nil && !dec.More() {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if enc.Encode(v) == nil {
			return safe(strings.TrimSuffix(buf.String(), "\n"))
		}
	}
	return safe("not JSON: " + fmt.Sprintf("%q", trimmed))
}

// clip limits text to maxArgLines lines of at most maxArgLineRune characters each and returns it
// with the number of bytes it left out (0 when it shows all of it).
func clip(text string) (string, int) {
	lines := strings.Split(text, "\n")
	var out []string
	hidden := 0
	for i, l := range lines {
		if i >= maxArgLines {
			hidden += len(l) + 1
			continue
		}
		if utf8.RuneCountInString(l) > maxArgLineRune {
			cut := 0
			for n := 0; n < maxArgLineRune; n++ {
				_, w := utf8.DecodeRuneInString(l[cut:])
				cut += w
			}
			hidden += len(l) - cut
			l = l[:cut] + " [...]"
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n"), hidden
}

// argsDigest returns the SHA-256 of the call's canonical arguments as the signature binds it. It
// is read out of the very bytes an approver signs (agent.ApprovalDecisionBytes), at the offset the
// layout fixes (after the domain tag and the three length-prefixed ids), so the digest shown is
// the digest signed, not a second computation that could drift from it.
func argsDigest(s agent.ApprovalSubject) string {
	b := agent.ApprovalDecisionBytes(s, "x", true, time.Unix(0, 0))
	off := len(agent.DecisionFormat) + 1 + 3*4 + len(s.RunID) + len(s.ToolUseID) + len(s.ToolName)
	return hex.EncodeToString(b[off : off+32])
}

// digestAt reports whether msg, decision bytes, carry digest at the offset argsDigest reads it
// from. Run before signing, it checks the layout this file assumes is the layout in use.
func digestAt(msg []byte, s agent.ApprovalSubject, digest string) bool {
	off := len(agent.DecisionFormat) + 1 + 3*4 + len(s.RunID) + len(s.ToolUseID) + len(s.ToolName)
	if len(msg) < off+32 {
		return false
	}
	// The three length prefixes must read back as the three ids' lengths.
	p := len(agent.DecisionFormat) + 1
	for _, id := range []string{s.RunID, s.ToolUseID, s.ToolName} {
		if int(binary.BigEndian.Uint32(msg[p:])) != len(id) || string(msg[p+4:p+4+len(id)]) != id {
			return false
		}
		p += 4 + len(id)
	}
	return hex.EncodeToString(msg[off:off+32]) == digest
}
