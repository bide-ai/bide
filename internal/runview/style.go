package runview

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/bide-ai/bide/internal/term"
	"github.com/bide-ai/bide/plan"
)

// The terminal text of bide check and bide check -diff -check: each helper returns its text
// unchanged when the style is off, so the plain output is the same with or without them.

// riskWords are the risk classes and gates in a problem or a change, colored as the risk map
// colors them: a side effect red, read-only and idempotent green, an approval gate amber.
var riskWords = regexp.MustCompile(`\b(side_effect|side effect|read-only|readonly|idempotent|approval gate|approval \d+ of \d+|approval >= \d+(?: of \d+)?)\b`)

func colorRisk(st term.Style, s string) string {
	if !st.On() {
		return s
	}
	return riskWords.ReplaceAllStringFunc(s, func(w string) string {
		switch {
		case strings.HasPrefix(w, "side"):
			return st.Fail(w)
		case strings.HasPrefix(w, "approval"):
			return st.Warn(w)
		}
		return st.OK(w)
	})
}

// indent starts a line that continues the one above it, on a terminal.
const indent = "    "

// assertionMessage is plan's message for an assertion that is violated or cannot be decided:
// assert[i] "<text>" is violated: <why> (counterexample: a -> b -> c).
var assertionMessage = regexp.MustCompile(`^(assert\[\d+\]) ("(?:[^"\\]|\\.)*") (is violated|cannot be decided): (.*?)(?: \(counterexample: (.*)\))?$`)

// assertionLines is a violated or undecided assertion on a terminal, over lines that fit its
// width: the verdict and the assertion, then the reason, then the counterexample path in red with
// the node it reaches in bold. ok is false when color is off or p is not such an assertion; the
// plain output keeps plan's one line.
func assertionLines(st term.Style, file string, p plan.ConfigProblem) (string, bool) {
	m := assertionMessage.FindStringSubmatch(p.Message)
	if !st.On() || m == nil {
		return "", false
	}
	verdict := st.Fail("violated")
	if m[3] == "cannot be decided" {
		verdict = st.Warn("cannot be decided")
	}
	where := st.Dim(file)
	if p.Node != "" {
		where += ":" + st.Bold(p.Node)
	}
	s := fmt.Sprintf("%s%s: %s %s: %s\n%s%s\n", st.Cross(), where, m[1], verdict, m[2], indent, st.Dim(m[4]))
	if m[5] != "" {
		nodes := strings.Split(m[5], " -> ")
		for i, n := range nodes {
			if n == p.Node {
				nodes[i] = st.BoldFail(n)
			} else {
				nodes[i] = st.Fail(n)
			}
		}
		s += indent + st.Fail("counterexample:") + " " + strings.Join(nodes, st.Fail(" -> ")) + "\n"
	}
	return s, true
}

// cexMark opens the counterexample path plan appends to a violated assertion's message.
const cexMark = " (counterexample: "

// problemText is a problem's message: a violated assertion's verdict and counterexample path in
// red, with the node the path reaches (the problem's node) in bold.
func problemText(st term.Style, p plan.ConfigProblem) string {
	msg := p.Message
	if !st.On() {
		return msg
	}
	var path string
	if i := strings.LastIndex(msg, cexMark); i >= 0 && strings.HasSuffix(msg, ")") {
		msg, path = msg[:i], msg[i+len(cexMark):len(msg)-1]
	}
	msg = colorRisk(st, msg)
	msg = strings.Replace(msg, " is violated:", " "+st.Fail("is violated")+":", 1)
	msg = strings.Replace(msg, " cannot be decided:", " "+st.Warn("cannot be decided")+":", 1)
	if path == "" {
		return msg
	}
	nodes := strings.Split(path, " -> ")
	for i, n := range nodes {
		if n == p.Node {
			nodes[i] = st.BoldFail(n)
		} else {
			nodes[i] = st.Fail(n)
		}
	}
	return msg + " (" + st.Fail("counterexample:") + " " + strings.Join(nodes, st.Fail(" -> ")) + ")"
}

// changeMark marks a diff line: + added, - removed, ~ changed, and a cross for a weakened gate.
func changeMark(st term.Style, c Change) string {
	if !st.On() {
		return ""
	}
	switch {
	case c.Gate == "weakened":
		return st.Cross()
	case c.Change == "added":
		return st.OK("+") + " "
	case c.Change == "removed":
		return st.Fail("-") + " "
	}
	return st.Warn("~") + " "
}

// changeText is c.String() in st's colors: the change green, red or amber, a weakened gate in
// bold red, a strengthened one green, and the risk classes in the details colored.
func changeText(st term.Style, c Change) string {
	if !st.On() {
		return c.String()
	}
	verb := st.Warn(c.Change)
	switch c.Change {
	case "added":
		verb = st.OK(c.Change)
	case "removed":
		verb = st.Fail(c.Change)
	}
	var where string
	switch c.What {
	case "node":
		where = "node " + st.Bold(c.Node)
	case "edge":
		where = "edge " + st.Bold(c.From) + " -> " + st.Bold(c.To)
	case "switch":
		where = "switch " + st.Bold(c.Node)
	default:
		where = c.What
	}
	s := verb + " " + where
	if len(c.Details) > 0 {
		d := colorRisk(st, strings.Join(c.Details, "; "))
		d = strings.ReplaceAll(d, "(weakened)", st.BoldFail("(weakened)"))
		d = strings.ReplaceAll(d, "(strengthened)", st.OK("(strengthened)"))
		s += ": " + d
	}
	return s
}
