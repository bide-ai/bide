package flowedit

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Assertion is one line of a config's assert list as the editor builds it: one of the six forms of
// the grammar plan reads (docs/guides/plan-flows.md, "Policy assertions"), with its parameters. The
// editor writes the line from these, so the page needs no grammar and a line it writes is one the
// check reads.
type Assertion struct {
	// Form is one of: "approval" (every path to Node passes approval >= K [of N] [excluding the
	// requester]), "passes" (every path to Node passes Via), "terminal" (every path to a terminal
	// passes Via), "no_side_effect" (no side effect after Node), "reachable" (Node reachable) and
	// "unreachable" (Node unreachable).
	Form string `json:"form"`
	Node string `json:"node,omitempty"`
	Via  string `json:"via,omitempty"`
	// K is the decisions the gate needs, N the most approvers it may name (0 for no limit), and
	// Excluding says the gate never counts the run's requester; all three are for "approval".
	K         int  `json:"k,omitempty"`
	N         int  `json:"n,omitempty"`
	Excluding bool `json:"excluding,omitempty"`
}

// The six forms, in the order the page lists them.
var assertionForms = []string{"approval", "passes", "terminal", "no_side_effect", "reachable", "unreachable"}

func isAssertionOp(op string) bool {
	return op == "add_assertion" || op == "set_assertion" || op == "remove_assertion"
}

// quoteNode writes a node name as the grammar reads one: bare when it has no space, in double quotes
// when it has a space, and not at all when it has a double quote (the grammar cannot hold one), a
// whitespace other than a single space inside the name, or a run of spaces (plan collapses runs of
// spaces in a line before it reads it, so the name would change).
func quoteNode(name string) (string, error) {
	switch {
	case name == "":
		return "", refuse("an assertion needs a node")
	case strings.Contains(name, `"`):
		return "", refuse("the node name %q has a double quote, which the assertion grammar cannot hold", name)
	case strings.IndexFunc(name, func(r rune) bool { return unicode.IsSpace(r) && r != ' ' }) >= 0,
		strings.Contains(name, "  "), strings.HasPrefix(name, " "), strings.HasSuffix(name, " "):
		return "", refuse("the node name %q has whitespace the assertion grammar cannot keep (only single spaces inside the name)", name)
	case strings.Contains(name, " "):
		return `"` + name + `"`, nil
	}
	return name, nil
}

// Text is the assertion as one line of the assert list, or a refusal when its parameters do not
// make one. It does not check that the nodes exist: AssertionOperation does, against the config.
func (a Assertion) Text() (string, error) {
	node := func(n string) (string, error) { return quoteNode(n) }
	switch a.Form {
	case "approval":
		n, err := node(a.Node)
		if err != nil {
			return "", err
		}
		if a.K < 1 || a.N < 0 || a.N != 0 && a.N < a.K {
			return "", refuse("an approval assertion needs 1 <= K <= N (N may be left out)")
		}
		s := "every path to " + n + " passes approval >= " + strconv.Itoa(a.K)
		if a.N > 0 {
			s += " of " + strconv.Itoa(a.N)
		}
		if a.Excluding {
			s += " excluding the requester"
		}
		return s, nil
	case "passes":
		n, err := node(a.Node)
		if err != nil {
			return "", err
		}
		v, err := node(a.Via)
		if err != nil {
			return "", err
		}
		return "every path to " + n + " passes " + v, nil
	case "terminal":
		v, err := node(a.Via)
		if err != nil {
			return "", err
		}
		return "every path to a terminal passes " + v, nil
	case "no_side_effect":
		n, err := node(a.Node)
		if err != nil {
			return "", err
		}
		return "no side effect after " + n, nil
	case "reachable", "unreachable":
		n, err := node(a.Node)
		if err != nil {
			return "", err
		}
		return n + " " + a.Form, nil
	}
	return "", refuse("%q is not one of the assertion forms %v", a.Form, assertionForms)
}

var (
	nodeRE         = `(\S+|"[^"]*")`
	reApproval     = regexp.MustCompile(`^every path to ` + nodeRE + ` passes approval >= (\d+)(?: of (\d+))?( excluding the requester)?$`)
	reTerminal     = regexp.MustCompile(`^every path to a terminal passes ` + nodeRE + `$`)
	rePasses       = regexp.MustCompile(`^every path to ` + nodeRE + ` passes ` + nodeRE + `$`)
	reNoSideEffect = regexp.MustCompile(`^no side effect after ` + nodeRE + `$`)
	reReachability = regexp.MustCompile(`^` + nodeRE + ` (reachable|unreachable)$`)
)

// unquoteNode is the name a token holds, as plan reads it: between the quotes exactly, with no
// escapes.
func unquoteNode(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// ParseAssertion reads a line of the assert list as one of the six forms, in the order plan tries
// them (a line that is two of them is the first); ok is false for a line that is none, which the
// check reports and the page shows without a form.
func ParseAssertion(line string) (a Assertion, ok bool) {
	text := strings.Join(strings.Fields(line), " ")
	if m := reApproval.FindStringSubmatch(text); m != nil {
		k, errK := strconv.Atoi(m[2])
		n := 0
		var errN error
		if m[3] != "" {
			n, errN = strconv.Atoi(m[3])
		}
		if errK != nil || errN != nil {
			return a, false
		}
		return Assertion{Form: "approval", Node: unquoteNode(m[1]), K: k, N: n, Excluding: m[4] != ""}, true
	}
	if m := reTerminal.FindStringSubmatch(text); m != nil {
		return Assertion{Form: "terminal", Via: unquoteNode(m[1])}, true
	}
	if m := rePasses.FindStringSubmatch(text); m != nil {
		return Assertion{Form: "passes", Node: unquoteNode(m[1]), Via: unquoteNode(m[2])}, true
	}
	if m := reNoSideEffect.FindStringSubmatch(text); m != nil {
		return Assertion{Form: "no_side_effect", Node: unquoteNode(m[1])}, true
	}
	if m := reReachability.FindStringSubmatch(text); m != nil {
		return Assertion{Form: m[2], Node: unquoteNode(m[1])}, true
	}
	return a, false
}

// applyAssertion applies an assertion operation: add appends a line to the assert list (creating the
// list when the config has none), set replaces the line at Index, remove removes it. The nodes the
// assertion names must be declared. data is src as JSON.
func applyAssertion(src, data []byte, op Operation) (*Applied, error) {
	var cfg struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
		Assert json.RawMessage `json:"assert"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, refuse("the config cannot be read: %v", err)
	}
	var lines []string
	hasKey := len(cfg.Assert) > 0                     // "assert:" is in the config, with or without items
	hasList := hasKey && string(cfg.Assert) != "null" // and it holds a list
	if hasList {
		if err := json.Unmarshal(cfg.Assert, &lines); err != nil {
			return nil, refuse("the config's assert is not a list of strings; edit it in the source")
		}
	}
	var text string
	if op.Op != "remove_assertion" {
		if op.Assertion == nil {
			return nil, refuse("%s needs an assertion", op.Op)
		}
		var err error
		if text, err = op.Assertion.Text(); err != nil {
			return nil, err
		}
		declared := map[string]bool{}
		for _, n := range cfg.Nodes {
			declared[n.Name] = true
		}
		for _, name := range []string{op.Assertion.Node, op.Assertion.Via} {
			if name != "" && !declared[name] {
				return nil, refuse("no node is named %q", name)
			}
		}
		for i, l := range lines {
			if strings.Join(strings.Fields(l), " ") == text && !(op.Op == "set_assertion" && i == op.Index) {
				return nil, refuse("the config already has the assertion %q", text)
			}
		}
	}
	if op.Op != "add_assertion" && (op.Index < 0 || op.Index >= len(lines)) {
		return nil, refuse("the config has no assertion at position %d (it has %d)", op.Index, len(lines))
	}
	var steps []Op
	switch op.Op {
	case "add_assertion":
		switch {
		case hasList:
			steps = []Op{{Kind: Insert, Path: []any{"assert"}, Index: len(lines), Value: text}}
		case hasKey: // "assert:" with nothing under it: replace the key
			steps = []Op{{Kind: Delete, Path: []any{"assert"}}, {Kind: Insert, Path: nil, Key: "assert", Value: []any{text}}}
		default:
			steps = []Op{{Kind: Insert, Path: nil, Key: "assert", Value: []any{text}}}
		}
	case "set_assertion":
		if strings.Join(strings.Fields(lines[op.Index]), " ") == text {
			return &Applied{Text: src}, nil
		}
		steps = []Op{{Kind: Set, Path: []any{"assert", op.Index}, Value: text}}
	case "remove_assertion":
		if len(lines) == 1 { // the last line: the list goes too, rather than leaving "assert:" with nothing under it
			steps = []Op{{Kind: Delete, Path: []any{"assert"}}}
		} else {
			steps = []Op{{Kind: Delete, Path: []any{"assert", op.Index}}}
		}
	}
	out := src
	for _, st := range steps {
		r, err := Apply(out, st)
		if err != nil {
			return nil, err
		}
		out = r.Text
	}
	return &Applied{Text: out, Changed: string(out) != string(src)}, nil
}

// assertionFormsOf returns each assert line as a form, in order, nil for a line that is not one.
func assertionFormsOf(lines []string) []*Assertion {
	out := make([]*Assertion, len(lines))
	for i, l := range lines {
		if a, ok := ParseAssertion(l); ok {
			a := a
			out[i] = &a
		}
	}
	return out
}
