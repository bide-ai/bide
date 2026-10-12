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

// AssertionResult is the verdict on one assert: line of a flow config.
type AssertionResult struct {
	// Text is the assertion as written.
	Text string `json:"text"`
	// Holds reports the assertion holds on the declared graph.
	Holds bool `json:"holds"`
	// Undecided reports the graph alone cannot decide it (a node's Safety is not known).
	Undecided bool `json:"undecided,omitempty"`
	// Path is a counterexample when the assertion does not hold: a path of node names through the
	// declared graph (empty when none applies, as for "X reachable" when X is not).
	Path []string `json:"path,omitempty"`
	// Message says why it does not hold, or what it could not decide.
	Message string `json:"message,omitempty"`
}

// The assertion grammar: one assertion per line of the config's "assert" list. A node is named
// bare (no spaces) or in double quotes.
var (
	assertGate   = regexp.MustCompile(`^every path to (\S+|"[^"]*") passes approval >= (\d+)(?: of (\d+))?$`)
	assertPasses = regexp.MustCompile(`^every path to (\S+|"[^"]*") passes (\S+|"[^"]*")$`)
	assertNoSide = regexp.MustCompile(`^no side effect after (\S+|"[^"]*")$`)
	assertReach  = regexp.MustCompile(`^(\S+|"[^"]*") (reachable|unreachable)$`)
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

// shortestPath is a shortest path from start to a node goal accepts, never entering a node skip
// rejects (start excepted), by breadth-first search, so a loop is visited once. nil when none.
func shortestPath(adj map[string][]string, start string, goal, skip func(string) bool) []string {
	parent := map[string]string{start: ""}
	queue := []string{start}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if goal(n) {
			path := []string{n}
			for p := parent[n]; p != ""; p = parent[p] {
				path = append(path, p)
			}
			slices.Reverse(path)
			return path
		}
		for _, m := range adj[n] {
			if _, seen := parent[m]; seen || skip(m) {
				continue
			}
			parent[m] = n
			queue = append(queue, m)
		}
	}
	return nil
}

// assertions decides each assertion on r's declared graph, and adds a problem for each that is
// ill-formed, does not hold, or cannot be decided.
func (r *ConfigReport) assertions(lines []string) {
	adj, entry := graphOf(r.Topology)
	declared := map[string]bool{}
	for _, n := range r.Topology.Nodes {
		declared[n.Name] = true
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
		bad := func(format string, args ...any) {
			r.Problems = append(r.Problems, ConfigProblem{Severity: "error", Message: fmt.Sprintf("assert[%d] %q: ", i, text) + fmt.Sprintf(format, args...)})
		}
		var nodes []string
		var decide func() AssertionResult
		switch m := (struct{ g, p, s, r []string }{assertGate.FindStringSubmatch(text), assertPasses.FindStringSubmatch(text), assertNoSide.FindStringSubmatch(text), assertReach.FindStringSubmatch(text)}); {
		case m.g != nil:
			target := name(m.g[1])
			k, _ := strconv.Atoi(m.g[2])
			n := -1
			if m.g[3] != "" {
				n, _ = strconv.Atoi(m.g[3])
			}
			if k < 1 || n != -1 && n < k {
				bad("want approval >= K of N with 1 <= K <= N")
				continue
			}
			nodes = []string{target}
			decide = func() AssertionResult {
				strict := func(x string) bool {
					a, ok := r.Approvals[x]
					return ok && a.Need >= k && (n == -1 || len(a.Approvers) <= n)
				}
				return r.passes(res, adj, entry, target, strict, fmt.Sprintf("a gate of at least %s", gateSpec(k, n)))
			}
		case m.p != nil:
			target, via := name(m.p[1]), name(m.p[2])
			nodes = []string{target, via}
			decide = func() AssertionResult {
				return r.passes(res, adj, entry, target, func(x string) bool { return x == via }, strconv.Quote(via))
			}
		case m.s != nil:
			from := name(m.s[1])
			nodes = []string{from}
			decide = func() AssertionResult {
				var unknown []string
				p := shortestPath(adj, from, func(x string) bool {
					if x == from {
						return false
					}
					switch r.Safety[x] {
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
					res.Path, res.Message = p, fmt.Sprintf("%q is a side effect after %q", p[len(p)-1], from)
				case len(unknown) > 0:
					res.Undecided = true
					res.Message = fmt.Sprintf("the safety of %s, after %q, is not known: register the blocks (plan/planview) or state their safety in the config", strings.Join(quoteAll(unknown), ", "), from)
				default:
					res.Holds = true
				}
				return res
			}
		case m.r != nil:
			target, want := name(m.r[1]), m.r[2] == "reachable"
			nodes = []string{target}
			decide = func() AssertionResult {
				p := shortestPath(adj, entry, func(x string) bool { return x == target }, func(string) bool { return false })
				switch {
				case want && p == nil:
					res.Message = fmt.Sprintf("no path from the entry %q reaches %q", entry, target)
				case !want && p != nil:
					res.Path, res.Message = p, fmt.Sprintf("%q is reachable from the entry", target)
				default:
					res.Holds = true
				}
				return res
			}
		default:
			bad("not an assertion this version reads (see the run viewer guide for the grammar)")
			continue
		}
		missing := ""
		for _, n := range nodes {
			if !declared[n] {
				missing = n
				break
			}
		}
		if missing != "" {
			bad("%q names no declared node", missing)
			continue
		}
		res = decide()
		r.Assertions = append(r.Assertions, res)
		switch {
		case res.Undecided:
			r.Problems = append(r.Problems, ConfigProblem{Severity: "error", Message: fmt.Sprintf("assert[%d] %q cannot be decided: %s", i, text, res.Message)})
		case !res.Holds:
			p := ConfigProblem{Severity: "error", Message: fmt.Sprintf("assert[%d] %q does not hold: %s", i, text, res.Message)}
			if len(res.Path) > 0 {
				p.Message += " (counterexample: " + strings.Join(res.Path, " -> ") + ")"
				p.Node = res.Path[len(res.Path)-1]
			} else {
				p.Node = nodes[0]
			}
			r.Problems = append(r.Problems, p)
		}
	}
}

// passes decides "every path to target passes a node good accepts": it holds when target is not
// reachable from the entry in the graph with those nodes removed. A counterexample is the shortest
// path to target that avoids them. The entry itself is on every path, so it counts when good.
func (r *ConfigReport) passes(res AssertionResult, adj map[string][]string, entry, target string, good func(string) bool, what string) AssertionResult {
	if good(entry) {
		res.Holds = true
		return res
	}
	p := shortestPath(adj, entry, func(x string) bool { return x == target }, good)
	if p == nil {
		res.Holds = true
		return res
	}
	res.Path = p
	res.Message = fmt.Sprintf("a path reaches %q without passing %s", target, what)
	return res
}

func gateSpec(k, n int) string {
	if n == -1 {
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
