package runview

import (
	"testing"

	"github.com/bide-ai/bide/internal/term"
	"github.com/bide-ai/bide/plan"
)

// TestServingLine: bide view's address line is the plain text it was, and with color the address
// is in bold cyan.
func TestServingLine(t *testing.T) {
	const link = "http://127.0.0.1:8484/"
	if got := servingLine(term.New(false), link); got != "bide view: serving http://127.0.0.1:8484/ (read-only; Ctrl-C to stop)\n" {
		t.Errorf("plain: %q", got)
	}
	if got := servingLine(term.New(true), link); got != "bide view: serving \x1b[1;36mhttp://127.0.0.1:8484/\x1b[0m \x1b[2m(read-only; Ctrl-C to stop)\x1b[0m\n" {
		t.Errorf("color: %q", got)
	}
}

// TestStyledTextPlain: with color off, every helper returns the text it was given.
func TestStyledTextPlain(t *testing.T) {
	off := term.New(false)
	p := plan.ConfigProblem{Node: "charge", Message: `assert[0] "no side effect after charge" is violated: x (counterexample: a -> charge)`}
	if got := problemText(off, p); got != p.Message {
		t.Errorf("problemText: %q", got)
	}
	c := Change{Change: "changed", What: "node", Node: "charge", Gate: "weakened", Details: []string{"safety readonly -> side_effect", "approval 2 of 3 -> 1 of 3 (weakened)"}}
	if got := changeText(off, c); got != c.String() || changeMark(off, c) != "" {
		t.Errorf("changeText: %q, mark %q", got, changeMark(off, c))
	}
	if got := colorRisk(off, "a side effect"); got != "a side effect" {
		t.Errorf("colorRisk: %q", got)
	}
}

// TestColorRisk: risk classes are colored as the risk map colors them, whole words only.
func TestColorRisk(t *testing.T) {
	on := term.New(true)
	for in, want := range map[string]string{
		"safety readonly -> side_effect": "safety \x1b[32mreadonly\x1b[0m -> \x1b[31mside_effect\x1b[0m",
		"a side effect, can run":         "a \x1b[31mside effect\x1b[0m, can run",
		"read-only or idempotent":        "\x1b[32mread-only\x1b[0m or \x1b[32midempotent\x1b[0m",
		"an approval gate of 2":          "an \x1b[33mapproval gate\x1b[0m of 2",
		"approval 2 of 3: ops":           "\x1b[33mapproval 2 of 3\x1b[0m: ops",
		"node readonly_cache":            "node readonly_cache",
	} {
		if got := colorRisk(on, in); got != want {
			t.Errorf("colorRisk(%q) = %q, want %q", in, got, want)
		}
	}
}
