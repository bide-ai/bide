package runview

import (
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
