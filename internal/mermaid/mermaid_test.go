package mermaid

import "testing"

// Each character that could end a label, start a statement, or be read as markup is written as
// an entity code; everything else is written as is.
func TestLabel(t *testing.T) {
	for in, want := range map[string]string{
		"plain name -> int": `"plain name -> int"`,
		`a"b`:               `"a#34;b"`,
		"a#b":               `"a#35;b"`,
		"a&b":               `"a#38;b"`,
		"a<b":               `"a#60;b"`,
		"a`b":               `"a#96;b"`,
		"a\nb\tc":           `"a#10;b#9;c"`,
		"a\x7fb":            `"a#127;b"`,
		"a\u0085b":          `"a#133;b"`,
		"a\xffb":            `"a#65533;b"`,
		"a\uFFFDb":          `"a#65533;b"`,
		"é ✓ [x] {y} %% ;":  `"é ✓ [x] {y} %% ;"`,
		"a\u009fb c":        "\"a#159;b c\"",
	} {
		if got := Label(in); got != want {
			t.Errorf("Label(%q) = %s; want %s", in, got, want)
		}
	}
}
