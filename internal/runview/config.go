package runview

import (
	"fmt"
	"io"
	"slices"
	"strings"

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
	names := make([]string, 0, len(r.Approvals))
	for n := range r.Approvals {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		v.Approvals = append(v.Approvals, n+": "+gateText(r.Approvals[n].Need, r.Approvals[n].Approvers))
	}
	if len(r.Topology.Nodes) > 0 {
		v.Graph = configGraph(r)
	}
	return v
}

// gateText renders an approval gate as "2 of 3: ops, finance, risk".
func gateText(need int, approvers []string) string {
	return fmt.Sprintf("%d of %d: %s", need, len(approvers), strings.Join(approvers, ", "))
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
	declared := map[string]bool{}
	for _, n := range t.Nodes {
		declared[n.Name] = true
		kind := string(n.Kind)
		if kind == "" {
			kind = "block " + n.Block
		}
		gn := GraphNode{Name: n.Name, Kind: kind, Status: "declared"}
		if a, ok := r.Approvals[n.Name]; ok {
			gn.Note = "approval " + gateText(a.Need, a.Approvers)
		}
		if ps := at[n.Name]; len(ps) > 0 {
			gn.Status = "problem"
			if !slices.ContainsFunc(ps, func(s string) bool { return strings.HasPrefix(s, "error: ") }) {
				gn.Status = "warning"
			}
			gn.Title = n.Name + "\n" + strings.Join(ps, "\n")
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
		e := GraphEdge{From: from, To: to, Label: label, Back: back}
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
// arm, or nothing for the config as a whole), warnings marked as such, and reports whether any
// is an error.
func Check(w io.Writer, c Config) bool {
	r := plan.InspectConfig(c.Data, c.Registry)
	for _, p := range r.Problems {
		where := p.Node
		if where == "" && (p.From != "" || p.To != "") {
			where = p.From + "->" + p.To
		}
		msg := p.Message
		if p.Severity == "warning" {
			msg = "warning: " + msg
		}
		if where == "" {
			fmt.Fprintf(w, "%s: %s\n", c.Name, msg)
		} else {
			fmt.Fprintf(w, "%s:%s: %s\n", c.Name, where, msg)
		}
	}
	return r.Errors()
}
