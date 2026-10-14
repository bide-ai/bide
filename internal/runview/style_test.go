package runview

import (
	"strings"
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

// TestAssertionLines: a violated assertion on a terminal is the verdict and the assertion, the
// reason on its own line, and the counterexample path with the node it reaches in bold; an
// undecided one is amber with no path; anything else, or no color, keeps plan's one line.
func TestAssertionLines(t *testing.T) {
	on := term.New(true)
	p := plan.ConfigProblem{Node: "charge", Message: `assert[2] "no side effect after charge" is violated: charge runs twice (counterexample: validate -> charge -> charge)`}
	got, ok := assertionLines(on, "refund.json", p)
	if !ok {
		t.Fatal("a violated assertion is not rendered")
	}
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %q, want verdict, reason, counterexample", lines)
	}
	if !strings.Contains(lines[0], "refund.json") || !strings.Contains(lines[0], on.Bold("charge")) || !strings.Contains(lines[0], "assert[2] "+on.Fail("violated")+`: "no side effect after charge"`) {
		t.Errorf("verdict line = %q", lines[0])
	}
	if lines[1] != indent+on.Dim("charge runs twice") {
		t.Errorf("reason line = %q", lines[1])
	}
	if lines[2] != indent+on.Fail("counterexample:")+" "+on.Fail("validate")+on.Fail(" -> ")+on.BoldFail("charge")+on.Fail(" -> ")+on.BoldFail("charge") {
		t.Errorf("counterexample line = %q", lines[2])
	}

	undecided := plan.ConfigProblem{Message: `assert[0] "x" cannot be decided: loop`}
	got, ok = assertionLines(on, "f.json", undecided)
	if !ok || !strings.Contains(got, on.Warn("cannot be decided")) || strings.Contains(got, "counterexample") || strings.Contains(got, ":\x1b[1m") {
		t.Errorf("undecided = %q, %v: want amber, no path, no node", got, ok)
	}
	if _, ok := assertionLines(on, "f.json", plan.ConfigProblem{Message: "node charge has no edge"}); ok {
		t.Error("an ordinary problem was rendered as an assertion")
	}
	if _, ok := assertionLines(term.New(false), "f.json", p); ok {
		t.Error("an assertion was rendered with color off")
	}
}

// TestProblemTextColor: with color a violated assertion's verdict and counterexample path are red
// and the node the path reaches is bold; risk words in the message are colored; an undecided
// assertion is amber; a message with no path is returned without one.
func TestProblemTextColor(t *testing.T) {
	on := term.New(true)
	p := plan.ConfigProblem{Node: "charge", Message: `assert[0] "x" is violated: a side effect (counterexample: a -> charge)`}
	want := `assert[0] "x" ` + on.Fail("is violated") + ": a " + on.Fail("side effect") + " (" + on.Fail("counterexample:") + " " + on.Fail("a") + on.Fail(" -> ") + on.BoldFail("charge") + ")"
	if got := problemText(on, p); got != want {
		t.Errorf("violated:\n got %q\nwant %q", got, want)
	}
	if got := problemText(on, plan.ConfigProblem{Message: `assert[1] "y" cannot be decided: loop`}); got != `assert[1] "y" `+on.Warn("cannot be decided")+": loop" {
		t.Errorf("undecided: %q", got)
	}
	if got := problemText(on, plan.ConfigProblem{Message: "needs an approval gate"}); got != "needs an "+on.Warn("approval gate") {
		t.Errorf("no path: %q", got)
	}
}

// TestChangeTextColor: each kind of change has its mark and verb color; a weakened gate is a cross
// and bold red, a strengthened one green; nodes and edges are named in bold.
func TestChangeTextColor(t *testing.T) {
	on := term.New(true)
	for _, c := range []struct {
		c    Change
		mark string
	}{
		{Change{Change: "added"}, on.OK("+") + " "},
		{Change{Change: "removed"}, on.Fail("-") + " "},
		{Change{Change: "changed"}, on.Warn("~") + " "},
		{Change{Change: "changed", Gate: "weakened"}, on.Cross()},
	} {
		if got := changeMark(on, c.c); got != c.mark {
			t.Errorf("changeMark(%+v) = %q, want %q", c.c, got, c.mark)
		}
	}
	if got := changeText(on, Change{Change: "added", What: "node", Node: "review"}); got != on.OK("added")+" node "+on.Bold("review") {
		t.Errorf("added node: %q", got)
	}
	if got := changeText(on, Change{Change: "removed", What: "edge", From: "a", To: "b"}); got != on.Fail("removed")+" edge "+on.Bold("a")+" -> "+on.Bold("b") {
		t.Errorf("removed edge: %q", got)
	}
	c := Change{Change: "changed", What: "node", Node: "charge", Details: []string{"approval 2 of 3 -> 1 of 3 (weakened)", "approval 1 of 3 -> 2 of 3 (strengthened)"}}
	got := changeText(on, c)
	if !strings.HasPrefix(got, on.Warn("changed")+" node "+on.Bold("charge")+": ") || !strings.Contains(got, on.BoldFail("(weakened)")) || !strings.Contains(got, on.OK("(strengthened)")) {
		t.Errorf("changed node: %q", got)
	}
	if got := changeText(on, Change{Change: "changed", What: "name"}); got != on.Warn("changed")+" name" {
		t.Errorf("other what: %q", got)
	}
}
