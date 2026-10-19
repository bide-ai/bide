package printable

import (
	"bytes"
	"strings"
	"testing"
	"unicode"
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

func TestBadRefusesEveryDefaultIgnorable(t *testing.T) {
	n := 0
	for _, tab := range []*unicode.RangeTable{unicode.Other_Default_Ignorable_Code_Point, unicode.Variation_Selector} {
		for r := rune(0); r <= unicode.MaxRune; r++ {
			if unicode.Is(tab, r) {
				n++
				if _, bad := Bad("a" + string(r) + "b"); !bad {
					t.Errorf("U+%04X passed", r)
				}
			}
		}
	}
	for _, r := range []rune{0x034f, 0x115f, 0x1160, 0x17b4, 0x17b5, 0x180b, 0x180f, 0x200c, 0x200d, 0x2800, 0x3164, 0xfe00, 0xfe0f, 0xffa0, 0xe0100} {
		if _, bad := Bad("a" + string(r) + "b"); !bad {
			t.Errorf("U+%04X passed", r)
		}
	}
	if n < 100 {
		t.Errorf("only %d ignorable code points came out of the tables of Unicode %s", n, UnicodeVersion)
	}
	if _, bad := Bad("\u3000\u3164"); !bad {
		t.Error("a name of only blanks passed")
	}
}

func TestWriterCloseWritesWhatIsHeld(t *testing.T) {
	var b bytes.Buffer
	w := Writer(&b)
	w.Write([]byte("a\xe2\x80")) // the start of a three-byte character
	if b.String() != "a" {
		t.Fatalf("%q", b.String())
	}
	if err := w.Close(); err != nil || b.String() != "a\\x{E2}\\x{80}" {
		t.Errorf("%q %v", b.String(), err)
	}
	b.Reset()
	w = Writer(&b)
	w.Write([]byte("x\x1b[1;"))
	w.Close()
	if b.String() != "x\\u{1B}\\x{5B}\\x{31}\\x{3B}" {
		t.Errorf("%q", b.String())
	}
}

func TestBadRefusesBlankCharacters(t *testing.T) {
	for name, s := range map[string]string{
		"hangul filler": "\u3164", "halfwidth hangul filler": "ab\uffa0", "choseong filler": "\u115f", "jungseong filler": "a\u1160",
		"braille blank": "\u2800", "khmer inherent vowel": "a\u17b4", "khmer inherent aa": "\u17b5", "grapheme joiner": "a\u034f",
		"mongolian selector": "a\u180b", "mongolian vowel separator": "a\u180e", "bare variation selector": "\ufe0f", "variation selector after a space": "a \ufe0f",
		"variation selector after a variation selector": "a\ufe0f\ufe0e", "supplement variation selector alone": "\U000e0100",
		"leading combining mark": "\u0301a", "only marks": "\u0301\u0302",
	} {
		if _, bad := Bad(s); !bad {
			t.Errorf("%s: %q passed", name, s)
		}
	}
	for _, s := range []string{"e\u0301", "\u0661\u0662", "\u05e9\u05dc\u05d5\u05dd", "\u0645\u0631\u062d\u0628\u0627", "\ud55c\uae00", "\u4e2d\u6587", "\U0001F44D\U0001F3FD"} {
		if r, bad := Bad(s); bad {
			t.Errorf("%q refused for %U", s, r)
		}
	}
}

func TestWriterHoldsASplitColorSequence(t *testing.T) {
	var b bytes.Buffer
	w := Writer(&b)
	for _, p := range []string{"a \x1b[1;3", "1mred\x1b", "[0m"} {
		if _, err := w.Write([]byte(p)); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := b.String(), "a \x1b[1;31mred\x1b[0m"; got != want {
		t.Errorf("%q, want %q", got, want)
	}
}

func TestEscapeInvalidUTF8(t *testing.T) {
	if got, want := Escape("a\xffb"), `a\x{FF}b`; got != want {
		t.Errorf("%q, want %q", got, want)
	}
}
