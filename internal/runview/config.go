package runview

import (
	"fmt"
	"io"
	"slices"
	"strings"

	safetext "github.com/bide-ai/bide/internal/printable"
	"github.com/bide-ai/bide/internal/term"
	"github.com/bide-ai/bide/plan"
)

// Config is a flow definition to show instead of a run: its file name (for messages), its JSON
// bytes (a YAML file converted to JSON), and, when the program embeds the viewer, the
// plan.Registry its blocks, predicates and merges are registered in. With no Registry the check is
// structural (see plan.InspectConfig).
type Config struct {
	Name     string
	Data     []byte
	Registry *plan.Registry
	// Strict makes Check fail on a warning as well as on an error (bide view -check -strict).
	Strict bool
}

// ConfigView is what the config page shows.
type ConfigView struct {
	Name       string               `json:"name"`
	Flow       string               `json:"flow"`
	Typed      bool                 `json:"typed"`
	Digest     string               `json:"digest,omitempty"`
	Errors     int                  `json:"errors"`
	Warnings   int                  `json:"warnings"`
	Problems   []plan.ConfigProblem `json:"problems,omitempty"`
	Graph      *Graph               `json:"graph,omitempty"`
	Approvals  []string             `json:"approvals,omitempty"`
	CheckLabel string               `json:"check"`
	// Risk counts the nodes by crash behavior and gate, for the summary card.
	Risk RiskSummary `json:"risk"`
	// Assertions are the verdicts on the config's assert lines, in order.
	Assertions []plan.AssertionResult `json:"assertions,omitempty"`
	// Verdict is the answer the page opens with (see configVerdict); it is not in the JSON view.
	Verdict Verdict `json:"-"`
}

// RiskSummary counts a config's nodes by safety class: side effects (gated, when every path to one
// passes an approval gate, and ungated), read-only, idempotent, and unknown (the structural check
// cannot see a block's registered Safety).
type RiskSummary struct {
	SideEffects int `json:"side_effects"`
	Gated       int `json:"gated"`
	Ungated     int `json:"ungated"`
	ReadOnly    int `json:"read_only"`
	Idempotent  int `json:"idempotent"`
	Unknown     int `json:"unknown"`
	// Line is the headline, as "3 side-effect steps, 2 gated, 1 ungated" ("steps known" when a
	// class is unknown).
	Line string `json:"line"`
	// UnknownLine says why some classes are unknown, empty when none is.
	UnknownLine string `json:"unknown_line,omitempty"`
}

// BuildConfig checks c with plan.InspectConfig and lays out its declared graph, each problem
// marked on the node, edge or arm it belongs to.
func BuildConfig(c Config) *ConfigView {
	r := plan.InspectConfig(c.Data, c.Registry)
	v := &ConfigView{Name: c.Name, Flow: r.Topology.Flow, Typed: r.Typed, Digest: r.Digest, Problems: r.Problems}
	v.CheckLabel = "structural check: types and registered blocks are checked when you embed the viewer with your plan.Registry"
	if r.Typed {
		v.CheckLabel = "full check against your plan.Registry (Load's checks, with In and Out taken from the entry and terminal nodes)"
	}
	for _, p := range r.Problems {
		if p.Severity == "error" {
			v.Errors++
		} else {
			v.Warnings++
		}
	}
	// The gates of the final Topology: with a Registry, a gate a block declares in Go as well.
	for _, n := range r.Topology.Nodes {
		if n.Gate != nil {
			v.Approvals = append(v.Approvals, n.Name+": "+topoGateText(n.Gate))
		}
	}
	slices.Sort(v.Approvals)
	v.Risk = riskSummary(r)
	v.Assertions = r.Assertions
	if len(r.Topology.Nodes) > 0 {
		v.Graph = configGraph(r)
	}
	v.Verdict = configVerdict(v)
	return v
}

// gateText renders an approval gate as "2 of 3: ops, finance, risk".
func gateText(need int, approvers []string) string {
	return fmt.Sprintf("%d of %d: %s", need, len(approvers), strings.Join(approvers, ", "))
}

// topoGateText renders a declared gate: "one decision", or as gateText does, with " excluding the
// requester" for a gate that excludes the run's requester; either with ", times out after 24h" for a
// gate that has a deadline.
func topoGateText(g *plan.TopologyGate) string {
	if g.Single {
		return "one decision" + timeoutSuffix(g, false)
	}
	return gateText(g.Need, g.Approvers) + exclusionText(g) + timeoutSuffix(g, false)
}

// gateBoxLine is a declared gate in a node's box: "gated: 2 of 3", then " · SoD" for a gate that
// excludes the requester and " · 24h" for one with a deadline (the tooltip spells both out).
func gateBoxLine(g *plan.TopologyGate) string {
	s := "gated: " + topoGateText(g)
	if !g.Single {
		s = fmt.Sprintf("gated: %d of %d", g.Need, len(g.Approvers))
	}
	if g.ExcludePrincipal {
		s += " · SoD"
	}
	if t := g.TimeoutText(); t != "" {
		s += " · " + t
	}
	return s
}

// riskSummary counts r's nodes by safety class and gate.
func riskSummary(r plan.ConfigReport) RiskSummary {
	var s RiskSummary
	for _, n := range r.Topology.Nodes {
		switch safetyOf(r, n.Name) {
		case plan.SafetySideEffect:
			s.SideEffects++
			if r.Gated[n.Name] {
				s.Gated++
			} else {
				s.Ungated++
			}
		case plan.SafetyReadOnly:
			s.ReadOnly++
		case plan.SafetyIdempotent:
			s.Idempotent++
		default:
			s.Unknown++
		}
	}
	known := ""
	if s.Unknown > 0 {
		known = " known"
	}
	s.Line = fmt.Sprintf("%d side-effect step%s%s, %d gated, %d ungated", s.SideEffects, plural(s.SideEffects), known, s.Gated, s.Ungated)
	switch {
	case s.Unknown > 0 && !r.Typed:
		s.UnknownLine = fmt.Sprintf("%d node%s: safety unknown without the Registry (the config states none)", s.Unknown, plural(s.Unknown))
	case s.Unknown > 0:
		s.UnknownLine = fmt.Sprintf("%d node%s: safety unknown (the config has errors, so its blocks were not resolved)", s.Unknown, plural(s.Unknown))
	}
	return s
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// safetyOf is node's class in r, unknown when r has none for it.
func safetyOf(r plan.ConfigReport, node string) plan.NodeSafety {
	if s, ok := r.Safety[node]; ok {
		return s
	}
	return plan.SafetyUnknown
}

// riskText is a safety class as a node's box and tooltip phrase it: the class, the crash behavior
// in a few words, and the detail.
func riskText(s plan.NodeSafety) (class, crash, detail string) {
	switch s {
	case plan.SafetySideEffect:
		return "side effect", "crash: halts, asks", "side effect: must not run twice. A crash mid-node halts the resume (*agent.OutcomeUnknown) until the outcome is recorded (agent.ResolveHalt)."
	case plan.SafetyReadOnly:
		return "read-only", "crash: re-runs", "read-only: no external effect. A resume after a crash re-runs it."
	case plan.SafetyIdempotent:
		return "idempotent", "crash: re-runs", "idempotent: safe to repeat. A resume after a crash re-runs it."
	}
	return "safety unknown", "crash: unknown", "safety unknown: the block is not registered here and the config states no safety. Check with the plan.Registry (plan/planview), or state it in the config."
}

// configGraph draws the declared graph with each problem on its node or edge. An edge or arm to a
// name no node declares gets a node of its own, marked undeclared, so the dangling end is visible.
func configGraph(r plan.ConfigReport) *Graph {
	t := r.Topology
	g := &Graph{}
	at := map[string][]string{} // node -> problem messages
	edgeAt := map[[2]string][]string{}
	for _, p := range r.Problems {
		switch {
		case p.Node != "":
			at[p.Node] = append(at[p.Node], p.Severity+": "+p.Message)
		case p.From != "" || p.To != "":
			edgeAt[[2]string{p.From, p.To}] = append(edgeAt[[2]string{p.From, p.To}], p.Severity+": "+p.Message)
		}
	}
	// The violated assertions' counterexamples, drawn in red: each path's nodes and its edges.
	cexNode := map[string]bool{}
	cexEdge := map[[2]string]bool{}
	for _, a := range r.Assertions {
		if a.Violated() {
			for i, n := range a.Path {
				cexNode[n] = true
				if i > 0 {
					cexEdge[[2]string{a.Path[i-1], n}] = true
				}
			}
		}
	}
	declared := map[string]bool{}
	for _, n := range t.Nodes {
		declared[n.Name] = true
		kind := string(n.Kind)
		if kind == "" {
			kind = "block " + n.Block
		}
		class, crash, detail := riskText(safetyOf(r, n.Name))
		gn := GraphNode{Name: n.Name, Kind: kind, Status: "declared", Risk: class, Crash: crash, Cex: cexNode[n.Name]}
		gn.Title = n.Name + " (" + kind + ")\n" + detail
		if safetyOf(r, n.Name) == plan.SafetySideEffect {
			if r.Gated[n.Name] {
				gn.Title += "\nGated: every path to it passes an approval gate."
			} else {
				gn.Title += "\nUngated: a path reaches it without an approval gate."
			}
		}
		if n.Gate != nil {
			gn.Gate = topoGateText(n.Gate)
			gn.GateLine, gn.GateClass = gateBoxLine(n.Gate), "gated"
		}
		if ps := at[n.Name]; len(ps) > 0 {
			gn.Status = "problem"
			if !slices.ContainsFunc(ps, func(s string) bool { return strings.HasPrefix(s, "error: ") }) {
				gn.Status = "warning"
			}
			gn.Title += "\n" + strings.Join(ps, "\n")
		}
		g.Nodes = append(g.Nodes, gn)
	}
	addEnd := func(name string) {
		if name != "" && !declared[name] {
			declared[name] = true
			g.Nodes = append(g.Nodes, GraphNode{Name: name, Kind: "named by an edge", Status: "undeclared"})
		}
	}
	edge := func(from, to, label string, back bool) {
		addEnd(from)
		addEnd(to)
		e := GraphEdge{From: from, To: to, Label: label, Back: back, Cex: cexEdge[[2]string{from, to}]}
		if ps := edgeAt[[2]string{from, to}]; len(ps) > 0 {
			e.Problem = strings.Join(ps, "\n")
		}
		g.Edges = append(g.Edges, e)
	}
	for _, e := range t.Edges {
		edge(e.From, e.To, "", false)
	}
	for _, b := range t.Branches {
		for _, a := range b.Arms {
			label := "when " + a.Pred
			switch {
			case a.LoopBack:
				label = fmt.Sprintf("loop %s, at most %d", a.Pred, a.LoopMax)
			case a.Else:
				label = "else"
			}
			edge(b.Over, a.Target, label, a.LoopBack)
		}
	}
	layout(g, t.Entry)
	g.Mermaid = mermaid(g)
	return g
}

// Check writes c's problems as "file:node: message" lines (the node, "from->to" for an edge or
// arm, or nothing for the config as a whole), warnings marked as such, then, when the config has
// assertions, a line counting their verdicts, and reports whether any problem is an error (or,
// with c.Strict, any problem at all). A violated, undecided or invalid assertion is an error.
func Check(w io.Writer, c Config) bool { return CheckStyled(w, c, term.Style{}) }

// CheckStyled is Check in st's colors (bide check on a terminal): a red cross on each error, a
// violated assertion's counterexample path in red with the node it reaches in bold, risk classes
// colored as the risk map colors them, and the verdict counts in green, red and amber. With color
// off it writes what Check writes.
func CheckStyled(w io.Writer, c Config, st term.Style) bool {
	ew := safetext.Writer(w) // a name that reached the output unrefused is written out, never drawn
	defer ew.Close()
	w = ew
	r := plan.InspectConfig(c.Data, c.Registry)
	name := st.Dim(c.Name)
	for _, p := range r.Problems {
		where := p.Node
		if where == "" && (p.From != "" || p.To != "") {
			where = p.From + "->" + p.To
		}
		if lines, ok := assertionLines(st, c.Name, p); ok {
			fmt.Fprint(w, lines)
			continue
		}
		mark, msg := st.Cross(), problemText(st, p)
		if p.Severity == "warning" {
			mark, msg = st.Bang(), st.Warn("warning:")+" "+colorRisk(st, p.Message)
		}
		if where == "" {
			fmt.Fprintf(w, "%s%s: %s\n", mark, name, msg)
		} else {
			fmt.Fprintf(w, "%s%s:%s: %s\n", mark, name, st.Bold(where), msg)
		}
	}
	if len(r.Assertions) > 0 {
		var hold, violated, other int
		for _, a := range r.Assertions {
			switch {
			case a.Holds:
				hold++
			case a.Violated():
				violated++
			default:
				other++
			}
		}
		mark := st.Pass()
		if violated+other > 0 {
			mark = st.Cross()
		}
		count := func(n int, what string, color func(string) string) string {
			if n == 0 {
				color = st.Dim
			}
			return color(fmt.Sprintf("%d %s", n, what))
		}
		fmt.Fprintf(w, "%s%s: %d assertion%s: %s, %s, %s\n", mark, name, len(r.Assertions), plural(len(r.Assertions)),
			count(hold, "hold", st.OK), count(violated, "violated", st.Fail), count(other, "undecided or invalid", st.Warn))
	}
	return r.Errors() || c.Strict && len(r.Problems) > 0
}

// holdVerb is "holds" for one assertion and "hold" for any other count.
func holdVerb(n int) string {
	if n == 1 {
		return "holds"
	}
	return "hold"
}

// isAre is "is" for one and "are" for any other count.
func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}
