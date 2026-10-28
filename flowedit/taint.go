package flowedit

import (
	"strings"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/plan"
)

// The static taint view the page shows (docs/design/flow-taint.md, section 3). Everything is computed here
// from plan.ConfigReport.Taint, the analysis `bide check` prints; the page only draws it, and every state
// has words: none is carried by a color or a shape alone, and none says a node is safe.

// WithTaintPolicy gives a typed editor (WithRegistry) the taint policy of the flow its program runs, which a
// Registry does not hold (it is Builder.WithTaintPolicy). Without it the editor does not know the policy and
// says so: a node that reads untrusted data is "reads untrusted" and never "escalates". The zero
// agent.TaintPolicy states that the flow has none. It has no effect on a structural editor, and a
// description given by WithBlocks brings its own policy (RegistryDescription.TaintPolicy), which this option
// overrides.
//
// Experimental: may change in any 0.x release.
func WithTaintPolicy(p agent.TaintPolicy) Option {
	return func(o *options) { o.taintPolicy = &p }
}

// taintNode is a node's taint in the view: the analysis and its words.
type taintNode struct {
	plan.NodeTaint
	// Text is the node's taint in one sentence of words (what the outline, the inspector and the node's
	// accessible name say).
	Text string `json:"text"`
}

// taintSummary is the taint panel: the headline, the nodes to look at, and what the view cannot know.
type taintSummary struct {
	// Known says the declarations are known (typed, with the taint declarations) and the config has no
	// errors; when false there is one line and no verdict on any node.
	Known bool `json:"known"`
	// Line is the headline: counts of the nodes that may read untrusted data, or why it is not known.
	Line string `json:"line"`
	// Counts are the nodes of each verdict, in the order escalates, refused, exempt, reads untrusted.
	Counts []taintCount `json:"counts,omitempty"`
	// Exposed are the nodes that can have a side effect and may read untrusted data, in the flow's order.
	Exposed []string `json:"exposed,omitempty"`
	// Unknown are the exposed nodes whose escalation cannot be told because the flow's taint policy is not
	// given.
	Unknown []string `json:"unknown,omitempty"`
	// Sources are the nodes that declare a source, in the flow's order.
	Sources []string `json:"sources,omitempty"`
	// Caveat is what the verdict "no declared source before it" does not cover.
	Caveat string `json:"caveat,omitempty"`
	// Basis says what the verdicts rest on when it is not the Registry (a description file).
	Basis string `json:"basis,omitempty"`
}

type taintCount struct {
	Verdict string `json:"verdict"`
	N       int    `json:"n"`
}

// taintViews sets each node's taint and returns the summary, from the report r and the viewer's own
// summary tv (its headline and caveat). typed says the editor knows the blocks.
func taintViews(nodes []Node, r plan.ConfigReport, tv *runview.TaintView, basis string) *taintSummary {
	if len(r.Taint) == 0 || tv == nil {
		return nil
	}
	s := &taintSummary{Known: tv.Known, Line: tv.Line, Caveat: tv.Caveat}
	if !tv.Known {
		for i := range nodes {
			if t, ok := r.Taint[nodes[i].Name]; ok && len(t.Declares) > 0 {
				// The config states a source, whatever the blocks are.
				nodes[i].Taint = &taintNode{NodeTaint: t, Text: "unknown; the config declares source " + strings.Join(t.Declares, ", ")}
				s.Sources = append(s.Sources, nodes[i].Name)
			}
		}
		return s
	}
	s.Basis = basis
	counts := map[string]int{}
	for i := range nodes {
		t, ok := r.Taint[nodes[i].Name]
		if !ok {
			continue
		}
		if len(t.Declares) > 0 || len(t.Sources) > 0 {
			nodes[i].Taint = &taintNode{NodeTaint: t, Text: runview.TaintText(t)}
		}
		if len(t.Declares) > 0 {
			s.Sources = append(s.Sources, nodes[i].Name)
		}
		if len(t.Sources) == 0 {
			continue
		}
		counts[t.Verdict]++
		if r.Safety[nodes[i].Name] != plan.SafetyReadOnly {
			s.Exposed = append(s.Exposed, nodes[i].Name)
			if strings.HasPrefix(t.Reason, "taint policy not given") {
				s.Unknown = append(s.Unknown, nodes[i].Name)
			}
		}
	}
	for _, v := range []string{"escalates", "refused", "exempt", "reads untrusted"} {
		if counts[v] > 0 {
			s.Counts = append(s.Counts, taintCount{Verdict: v, N: counts[v]})
		}
	}
	return s
}
