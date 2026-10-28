package runview

import (
	"fmt"
	"io"
	"strings"

	"github.com/bide-ai/bide/internal/taintwords"
	"github.com/bide-ai/bide/internal/term"
	"github.com/bide-ai/bide/plan"
)

// The static taint verdicts of a config (plan.ConfigReport.Taint) as bide check prints them and the config
// page serves them. The words are plan's: this file chooses which nodes to list and how to say the rest.

// TaintView is the taint section of a config: what a declared source can do to the flow, from the same
// analysis the engine's rule is checked against. It is never a statement that a node is safe.
type TaintView struct {
	// Known says the declarations are known (a Registry, or a description that carries them) and the config
	// has no errors. When false there is one line and no per-node verdict.
	Known bool `json:"known"`
	// Line is the headline: "taint: 2 nodes may read untrusted data (1 escalates, 1 refused)", or why the
	// taint is not known.
	Line string `json:"line"`
	// Nodes are the nodes that declare a source or may read one, in the flow's order.
	Nodes []TaintNodeView `json:"nodes,omitempty"`
	// Caveat is what the verdict "no declared source before it" does not cover, present when Known.
	Caveat string `json:"caveat,omitempty"`
}

// TaintNodeView is one node of a TaintView.
type TaintNodeView struct {
	Node string `json:"node"`
	plan.NodeTaint
	// Text is the node as one line of words.
	Text string `json:"text"`
}

const taintCaveat = `a node with "no declared source before it" can still read untrusted data when the run declares its input untrusted, or when a block runs an agent or a nested flow, whose taint this view cannot see; a block that reads outside data without declaring it is not a source here`

// taintView builds the section for r, nil when r has none (a typed config with errors).
func taintView(r plan.ConfigReport) *TaintView {
	if len(r.Taint) == 0 {
		return nil
	}
	known := false
	reason := ""
	for _, t := range r.Taint {
		if t.Verdict != "unknown" {
			known = true
		}
		if reason == "" {
			reason = t.Reason
		}
	}
	if !known {
		if r.Typed {
			return &TaintView{Line: "taint: not known (" + reason + ")"}
		}
		return &TaintView{Line: "taint: not known without -blocks"}
	}
	v := &TaintView{Known: true, Caveat: taintCaveat}
	counts := map[string]int{}
	reading := 0
	for _, n := range r.Topology.Nodes {
		t, ok := r.Taint[n.Name]
		if !ok || len(t.Declares) == 0 && len(t.Sources) == 0 {
			continue
		}
		if len(t.Sources) > 0 {
			reading++
			counts[t.Verdict]++
		}
		v.Nodes = append(v.Nodes, TaintNodeView{Node: n.Name, NodeTaint: t, Text: taintNodeText(n.Name, t)})
	}
	var parts []string
	for _, w := range []string{"escalates", "refused", "exempt"} {
		if counts[w] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[w], w))
		}
	}
	switch {
	case reading == 0 && len(v.Nodes) == 0:
		v.Line = "taint: no node declares an untrusted source (a source that is not declared is not shown)"
		v.Caveat = ""
	case reading == 0:
		v.Line = "taint: no node has a declared source before it"
	case len(parts) == 0:
		v.Line = fmt.Sprintf("taint: %d node%s may read untrusted data", reading, plural(reading))
	default:
		v.Line = fmt.Sprintf("taint: %d node%s may read untrusted data (%s)", reading, plural(reading), strings.Join(parts, ", "))
	}
	return v
}

// taintNodeText is a node's taint as one line: the verdict, what it reads and how, what it declares, why.
func taintNodeText(name string, t plan.NodeTaint) string {
	s := name + ": " + t.Verdict
	if len(t.Sources) > 0 {
		s += ", reads " + strings.Join(t.Sources, ", ")
		if len(t.Path) > 1 {
			s += " via " + strings.Join(t.Path, " > ")
		} else {
			s += " (earlier in the flow's order)"
		}
	}
	if t.Exempt != "" {
		s += " (exempt: " + t.Exempt + ")"
	}
	if len(t.Declares) > 0 {
		s += "; source: " + strings.Join(t.Declares, ", ")
	}
	if r := strings.TrimSuffix(t.Reason, taintwords.Caveat); r != "" {
		s += "; " + strings.TrimSuffix(r, "; ")
	}
	return s
}

// writeTaint prints the taint section of a check.
func writeTaint(w io.Writer, st term.Style, name string, v *TaintView) {
	if v == nil {
		return
	}
	fmt.Fprintf(w, "%s: %s\n", name, st.Dim(v.Line))
	for _, n := range v.Nodes {
		fmt.Fprintf(w, "  %s\n", n.Text)
	}
	if v.Caveat != "" {
		fmt.Fprintf(w, "  %s\n", st.Dim(v.Caveat))
	}
}
