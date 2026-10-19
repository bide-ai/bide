package printable

import (
	"bytes"
	"strings"
	"testing"
)

func TestBad(t *testing.T) {
	for name, s := range map[string]string{
		"right-to-left override": "ch\u202egra", "zero-width space": "\u200b", "zero-width joiner": "a\u200db",
		"soft hyphen": "a\u00adb", "no-break space": "a\u00a0b", "line separator": "a\u2028b", "byte order mark": "a\ufeffb",
		"private use": "a\ue000b", "unassigned": "a\U000e0fffb", "tab": "a\tb", "newline": "a\nb", "NUL": "a\x00b", "DEL": "a\x7fb",
		"leading space": " a", "trailing space": "a ", "only a space": " ",
	} {
		if _, bad := Bad(s); !bad {
			t.Errorf("%s: %q passed", name, s)
		}
	}
	for _, s := range []string{"", "charge", "manual review", "é", "ノード", "🚀 rocket", "a-b_c.d", "x:y", `"q"`, "1e3"} {
		if r, bad := Bad(s); bad {
			t.Errorf("%q refused for %U", s, r)
		}
	}
}

func TestEscape(t *testing.T) {
	for in, want := range map[string]string{
		"plain":             "plain",
		"ch\u202egra":       `ch\u{202E}gra`,
		"a\u200b":           `a\u{200B}`,
		"a\nb\tc\r":         "a\nb\tc\r",
		"a\u00a0b":          `a\u{A0}b`,
		"a\x1bb":            `a\u{1B}b`,
		"\U0001F680 rocket": "\U0001F680 rocket",
	} {
		if got := Escape(in); got != want {
			t.Errorf("Escape(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWriterKeepsColorAndEscapesTheRest(t *testing.T) {
	var b bytes.Buffer
	w := Writer(&b)
	n, err := w.Write([]byte("\x1b[1;31mch\u202egra\x1b[0m ok\n"))
	if err != nil || n != len("\x1b[1;31mch\u202egra\x1b[0m ok\n") {
		t.Fatalf("n %d err %v", n, err)
	}
	if got, want := b.String(), "\x1b[1;31mch\\u{202E}gra\x1b[0m ok\n"; got != want {
		t.Errorf("%q, want %q", got, want)
	}
	if strings.ContainsRune(b.String(), '\u202e') {
		t.Error("a raw override got through")
	}
}

func TestEscapeName(t *testing.T) {
	if got, want := EscapeName("a\tb\nc\u202e"), `a\u{9}b\u{A}c\u{202E}`; got != want {
		t.Errorf("%q, want %q", got, want)
	}
}

// A character split across two writes is one character, not two bad ones.
func TestWriterJoinsASplitCharacter(t *testing.T) {
	var b bytes.Buffer
	w := Writer(&b)
	raw := []byte("a\u00e9\u202eb") // e-acute (2 bytes), then the override (3 bytes)
	for i := range raw {
		if _, err := w.Write(raw[i : i+1]); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := b.String(), "a\u00e9\\u{202E}b"; got != want {
		t.Errorf("%q, want %q", got, want)
	}
}
