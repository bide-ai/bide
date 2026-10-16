package plan

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// NodeSafety is how a node behaves when a crash cuts it off mid-run, as a risk map shows it.
type NodeSafety string

// The safety classes InspectConfig reports.
const (
	// SafetyReadOnly: no external effect; a resume re-runs the node.
	SafetyReadOnly NodeSafety = "read-only"
	// SafetyIdempotent: an effect that is safe to repeat; a resume re-runs the node.
	SafetyIdempotent NodeSafety = "idempotent"
	// SafetySideEffect: an effect that must not fire twice; a resume after a crash mid-node halts
	// (*agent.OutcomeUnknown) and asks for the outcome.
	SafetySideEffect NodeSafety = "side effect"
	// SafetyUnknown: the structural check cannot tell (the block is not registered and the config
	// states no safety).
	SafetyUnknown NodeSafety = "unknown"
)

// RerunsOnCrash reports whether a resume re-runs a node of this class that a crash cut off
// (read-only and idempotent); a side effect halts and asks for the outcome instead, and an unknown
// class is not known to do either.
func (s NodeSafety) RerunsOnCrash() bool { return s == SafetyReadOnly || s == SafetyIdempotent }

// AssertionResult is the verdict on one line of a flow config's assert list. InspectConfig reports
// one per line, in order.
type AssertionResult struct {
	// Text is the assertion as written, with its spaces collapsed.
	Text string `json:"text"`
	// Holds reports the assertion holds on the declared graph.
	Holds bool `json:"holds"`
	// Invalid reports the line is not an assertion this version reads, or names a node the config
	// does not declare; Message says which. It is neither held nor violated.
	Invalid bool `json:"invalid,omitempty"`
	// Vacuous reports the assertion holds only because nothing it is about is reachable: for
	// "every path to T passes ...", no path from the entry reaches T (or, for the terminal form, a
	// terminal); Message says so. A flow that turned cyclic, or lost the edge to T, satisfies such a
	// line without the line checking anything.
	Vacuous bool `json:"vacuous,omitempty"`
	// Undecided reports the graph alone cannot decide it (a node's Safety is not known).
	Undecided bool `json:"undecided,omitempty"`
	// Path is a counterexample when the assertion is violated: a path of node names through the
	// declared graph (empty when none applies, as for "X reachable" when X is not).
	Path []string `json:"path,omitempty"`
	// Message says why it does not hold, what it could not decide, why it is invalid, or why it
	// holds only vacuously.
	Message string `json:"message,omitempty"`
}

// Violated reports the assertion was decided and does not hold.
func (a AssertionResult) Violated() bool { return !a.Holds && !a.Invalid && !a.Undecided }

// The assertion grammar: one assertion per line of the config's "assert" list, its words separated
// by single spaces (runs of spaces are collapsed). A node is named bare (no spaces) or in double
// quotes. Each is decided by breadth-first reachability on the declared graph (every edge, a join's
// inputs, and every Switch arm, loop-back arms included), so a loop is visited once and never
// enumerated, and a counterexample is a shortest path.
var (
	assertGate     = regexp.MustCompile(`^every path to (\S+|"[^"]*") passes approval >= (\d+)(?: of (\d+))?( excluding the requester)?$`)
	assertTerminal = regexp.MustCompile(`^every path to a terminal passes (\S+|"[^"]*")$`)
	assertPasses   = regexp.MustCompile(`^every path to (\S+|"[^"]*") passes (\S+|"[^"]*")$`)
	assertNoSide   = regexp.MustCompile(`^no side effect after (\S+|"[^"]*")$`)
	assertReach    = regexp.MustCompile(`^(\S+|"[^"]*") (reachable|unreachable)$`)
)

// graphOf is the declared graph as adjacency: every edge (a join's inputs included) and every
// Switch arm (loop-back arms included), and the entry.
func graphOf(t Topology) (map[string][]string, string) {
	adj := map[string][]string{}
	for _, e := range t.Edges {
		adj[e.From] = append(adj[e.From], e.To)
	}
	for _, b := range t.Branches {
		for _, a := range b.Arms {
			adj[b.Over] = append(adj[b.Over], a.Target)
		}
	}
	return adj, t.Entry
}

// shortestPath is a shortest path from one of starts to a node goal accepts, never entering a node
// skip rejects (starts excepted), by breadth-first search, so a node in a loop is visited once. nil
// when none.
func shortestPath(adj map[string][]string, starts []string, goal, skip func(string) bool) []string {
	parent := map[string]string{}
	seen := map[string]bool{}
	var queue []string
	for _, s := range starts {
		if !seen[s] {
			seen[s] = true
			queue = append(queue, s)
		}
	}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if goal(n) {
			path := []string{n}
			for p, ok := parent[n]; ok; p, ok = parent[p] {
				path = append(path, p)
			}
			slices.Reverse(path)
			return path
		}
		for _, m := range adj[n] {
			if seen[m] || skip(m) {
				continue
			}
			seen[m] = true
			parent[m] = n
			queue = append(queue, m)
		}
	}
	return nil
}

// assertions decides each assertion on r's declared graph, and adds a problem for each that is
// invalid, violated, or cannot be decided. It runs on r's final Topology, so with a Registry a gate
// a block declares in Go counts as the config's own gates do.
func (r *ConfigReport) assertions(lines []string) {
	adj, entry := graphOf(r.Topology)
	declared := map[string]bool{}
	gates := map[string]*TopologyGate{}
	hasOut := map[string]bool{}
	for _, n := range r.Topology.Nodes {
		declared[n.Name] = true
		gates[n.Name] = n.Gate
	}
	for from := range adj {
		hasOut[from] = true
	}
	name := func(s string) string {
		if u, err := strconv.Unquote(s); err == nil {
			return u
		}
		return s
	}
	for i, line := range lines {
		text := strings.Join(strings.Fields(line), " ")
		res := AssertionResult{Text: text}
		invalid := func(format string, args ...any) {
			res.Invalid, res.Message = true, fmt.Sprintf(format, args...)
			r.Assertions = append(r.Assertions, res)
			r.Problems = append(r.Problems, ConfigProblem{Severity: "error", Message: fmt.Sprintf("assert[%d] %q: %s", i, text, res.Message)})
		}
		var nodes []string
		var decide func() AssertionResult
		if m := assertGate.FindStringSubmatch(text); m != nil {
			target := name(m[1])
			k, errK := strconv.Atoi(m[2])
			n := -1
			var errN error
			if m[3] != "" {
				n, errN = strconv.Atoi(m[3])
			}
			if errK != nil || errN != nil || k < 1 || n != -1 && n < k {
				invalid("want approval >= K of N with 1 <= K <= N")
				continue
			}
			excl := m[4] != ""
			nodes = []string{target}
			decide = func() AssertionResult {
				// A gate counts when it is at least as strict as K of N: it needs K or more decisions,
				// and (with "of N") from a named set of at most N approvers, and (with "excluding the
				// requester") it never counts the run's requester (agent.ApprovalPolicy.ExcludePrincipal).
				// A one-decision gate (agent.SingleApproval) needs one decision from no named set and
				// cannot exclude the requester, so it counts only for "approval >= 1".
				strict := func(x string) bool {
					g := gates[x]
					switch {
					case g == nil:
						return false
					case g.Single:
						return k == 1 && n == -1 && !excl
					}
					return g.Need >= k && (n == -1 || len(g.Approvers) <= n) && (!excl || g.ExcludePrincipal)
				}
				what := "an approval gate of at least " + gateSpec(k, n)
				if excl {
					what += " excluding the requester"
				}
				return passes(res, adj, entry, func(x string) bool { return x == target }, strict, what)
			}
		} else if m := assertTerminal.FindStringSubmatch(text); m != nil {
			via := name(m[1])
			nodes = []string{via}
			decide = func() AssertionResult {
				return passes(res, adj, entry, func(x string) bool { return !hasOut[x] }, func(x string) bool { return x == via }, strconv.Quote(via))
			}
		} else if m := assertPasses.FindStringSubmatch(text); m != nil {
			target, via := name(m[1]), name(m[2])
			nodes = []string{target, via}
			decide = func() AssertionResult {
				return passes(res, adj, entry, func(x string) bool { return x == target }, func(x string) bool { return x == via }, strconv.Quote(via))
			}
		} else if m := assertNoSide.FindStringSubmatch(text); m != nil {
			from := name(m[1])
			nodes = []string{from}
			decide = func() AssertionResult {
				// From each node "from" leads to, so "from" itself counts when a loop returns to it.
				var unknown []string
				p := shortestPath(adj, adj[from], func(x string) bool {
					switch r.safetyOf(x) {
					case SafetySideEffect:
						return true
					case SafetyUnknown:
						if !slices.Contains(unknown, x) {
							unknown = append(unknown, x)
						}
					}
					return false
				}, func(string) bool { return false })
				switch {
				case p != nil:
					res.Path = append([]string{from}, p...)
					res.Message = fmt.Sprintf("%q, a side effect, can run after %q", p[len(p)-1], from)
				case len(unknown) > 0:
					res.Undecided = true
					res.Message = fmt.Sprintf("the safety of %s, after %q, is not known: check with the plan.Registry (plan/planview) or state it in the config", strings.Join(quoteAll(unknown), ", "), from)
				default:
					res.Holds = true
				}
				return res
			}
		} else if m := assertReach.FindStringSubmatch(text); m != nil {
			target, want := name(m[1]), m[2] == "reachable"
			nodes = []string{target}
			decide = func() AssertionResult {
				p := shortestPath(adj, []string{entry}, func(x string) bool { return x == target }, func(string) bool { return false })
				switch {
				case want && p == nil:
					res.Message = fmt.Sprintf("no path from the entry %q reaches %q", entry, target)
				case !want && p != nil:
					res.Path, res.Message = p, fmt.Sprintf("a path from the entry reaches %q", target)
				default:
					res.Holds = true
				}
				return res
			}
		} else {
			invalid("not an assertion this version reads (see the plan flows guide for the grammar)")
			continue
		}
		if i := slices.IndexFunc(nodes, func(n string) bool { return !declared[n] }); i >= 0 {
			invalid("%q names no declared node", nodes[i])
			continue
		}
		res = decide()
		r.Assertions = append(r.Assertions, res)
		switch {
		case res.Undecided:
			r.Problems = append(r.Problems, ConfigProblem{Node: nodes[0], Severity: "error", Message: fmt.Sprintf("assert[%d] %q cannot be decided: %s", i, text, res.Message)})
		case !res.Holds:
			p := ConfigProblem{Node: nodes[0], Severity: "error", Message: fmt.Sprintf("assert[%d] %q is violated: %s", i, text, res.Message)}
			if len(res.Path) > 0 {
				p.Message += " (counterexample: " + strings.Join(res.Path, " -> ") + ")"
				p.Node = res.Path[len(res.Path)-1]
			}
			r.Problems = append(r.Problems, p)
		}
	}
}

// gated sets r.Gated from r's final Topology: a node is gated when the entry cannot reach it with
// the gated nodes removed (and every node is, when the entry is gated).
func (r *ConfigReport) gated() {
	adj, entry := graphOf(r.Topology)
	gate := map[string]bool{}
	for _, n := range r.Topology.Nodes {
		gate[n.Name] = n.Gate != nil
	}
	open := map[string]bool{}
	if !gate[entry] {
		shortestPath(adj, []string{entry}, func(x string) bool { open[x] = true; return false }, func(x string) bool { return gate[x] })
	}
	r.Gated = map[string]bool{}
	for _, n := range r.Topology.Nodes {
		r.Gated[n.Name] = !open[n.Name]
	}
}

// safetyOf is node's class, unknown when r has none for it.
func (r *ConfigReport) safetyOf(node string) NodeSafety {
	if s, ok := r.Safety[node]; ok {
		return s
	}
	return SafetyUnknown
}

// passes decides "every path to a node target accepts passes a node good accepts": it holds when
// no such node is reachable from the entry in the graph with the good nodes removed. A
// counterexample is a shortest path that avoids them. The entry is on every path, so it counts
// when good; a target that is itself good is guarded by itself.
func passes(res AssertionResult, adj map[string][]string, entry string, target, good func(string) bool, whatGood string) AssertionResult {
	if shortestPath(adj, []string{entry}, target, func(string) bool { return false }) == nil {
		res.Holds, res.Vacuous = true, true
		res.Message = "holds vacuously: no path from the entry reaches a node this assertion is about"
		return res
	}
	if good(entry) {
		res.Holds = true
		return res
	}
	p := shortestPath(adj, []string{entry}, target, good)
	if p == nil {
		res.Holds = true
		return res
	}
	res.Path = p
	res.Message = fmt.Sprintf("a path reaches %q without passing %s", p[len(p)-1], whatGood)
	return res
}

func gateSpec(k, n int) string {
	if n == -1 {
		if k == 1 {
			return "1 approval"
		}
		return strconv.Itoa(k) + " approvals"
	}
	return fmt.Sprintf("%d of %d", k, n)
}

func quoteAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = strconv.Quote(s)
	}
	return out
}

// gateAssertion is an assertion of the form "every path to T passes approval >= K [of N]
// [excluding the requester]".
type gateAssertion struct {
	target string
	k, n   int // n is -1 without "of N"
	excl   bool
}

func parseGateAssertion(text string) (gateAssertion, bool) {
	m := assertGate.FindStringSubmatch(strings.Join(strings.Fields(text), " "))
	if m == nil {
		return gateAssertion{}, false
	}
	ga := gateAssertion{k: -1, n: -1, excl: m[4] != ""}
	ga.target = m[1]
	if u, err := strconv.Unquote(m[1]); err == nil {
		ga.target = u
	}
	ga.k, _ = strconv.Atoi(m[2])
	if m[3] != "" {
		ga.n, _ = strconv.Atoi(m[3])
	}
	return ga, true
}

// assertForms are the assertion forms in the order InspectConfig tries them, each with the capture
// groups of its node names.
var assertForms = []struct {
	re    *regexp.Regexp
	nodes []int
}{{assertGate, []int{1}}, {assertTerminal, []int{1}}, {assertPasses, []int{1, 2}}, {assertNoSide, []int{1}}, {assertReach, []int{1}}}

// RenameAssertion is the assertion line with each node name it holds replaced by rename(name) and
// every name written in double quotes, so two spellings of one assertion (a bare name or a quoted
// one, extra spaces) are the same string. A line that is not an assertion this version reads comes
// back with only its spaces collapsed. A nil rename keeps the names.
func RenameAssertion(text string, rename func(string) string) string {
	text = strings.Join(strings.Fields(text), " ")
	for _, f := range assertForms {
		m := f.re.FindStringSubmatchIndex(text)
		if m == nil {
			continue
		}
		var b strings.Builder
		prev := 0
		for _, g := range f.nodes {
			start, end := m[2*g], m[2*g+1]
			name := text[start:end]
			if u, err := strconv.Unquote(name); err == nil {
				name = u
			}
			if rename != nil {
				name = rename(name)
			}
			b.WriteString(text[prev:start])
			b.WriteString(strconv.Quote(name))
			prev = end
		}
		b.WriteString(text[prev:])
		return b.String()
	}
	return text
}

// CompareAssertions relates two assertion lines of a config, for comparing two versions of it. It
// returns "equal" for the same assertion (spacing and quoting of a node name aside, in every form); "stronger" when
// b is a different assertion that holds only where a holds, which only the approval form decides
// ("every path to T passes approval >= K [of N] [excluding the requester]": b is stronger when it
// names the same T and needs at least a's K, from a set no larger than a's N, and excludes the
// requester wherever a does); "weaker" when both are of that form for the same T and b is not
// stronger or equal, so a config could satisfy b and violate a; and "" otherwise (different forms or
// targets, which the comparison cannot relate).
func CompareAssertions(a, b string) string {
	if RenameAssertion(a, nil) == RenameAssertion(b, nil) {
		return "equal"
	}
	ga, okA := parseGateAssertion(a)
	gb, okB := parseGateAssertion(b)
	if !okA || !okB || ga.target != gb.target {
		return ""
	}
	if ga == gb {
		return "equal"
	}
	if gb.k >= ga.k && (ga.n == -1 || gb.n != -1 && gb.n <= ga.n) && (!ga.excl || gb.excl) {
		return "stronger"
	}
	return "weaker"
}

// UngatedPath is a shortest path of node names from the entry to node that passes no approval gate,
// or nil when every path to node passes one (or the entry cannot reach it): the evidence behind
// Gated[node] being false.
func (r ConfigReport) UngatedPath(node string) []string {
	adj, entry := graphOf(r.Topology)
	gate := map[string]bool{}
	for _, n := range r.Topology.Nodes {
		gate[n.Name] = n.Gate != nil
	}
	if gate[entry] {
		return nil
	}
	return shortestPath(adj, []string{entry}, func(x string) bool { return x == node }, func(x string) bool { return gate[x] })
}
