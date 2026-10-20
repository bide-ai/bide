package themeaccess

import "testing"

func TestEscape(t *testing.T) {
	for in, want := range map[string]string{"plain é text": "plain é text", "a\nb": `a\nb`, "\x1b[31m": `\x1b[31m`, "\x00": `\x00`, "bad\xffbyte": `bad\xffbyte`, "\u009b": `\u009b`, "\u2028": `\u2028`, "\u2029": `\u2029`, "a\rb": `a\rb`, "\u0085": `\u0085`} {
		if got := Escape(in); got != want {
			t.Errorf("Escape(%q) = %q, want %q", in, got, want)
		}
	}
}
