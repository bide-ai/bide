package runview

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/bide-ai/bide/agent"
	mmd "github.com/bide-ai/bide/internal/mermaid"
	"github.com/bide-ai/bide/plan"
)

// Flow is a declared flow the viewer draws a run against. The journal records a flow run's name
// and topology digest, not its topology, so the topology comes from the caller: a program that
// holds the *plan.Flow passes its Topology, Digest and Conform; the bide view command reads a
// Topology exported as JSON (json.Marshal(flow.Topology())) and, optionally, the digest.
type Flow struct {
	Topology plan.Topology
	// Digest is flow.Digest(), compared with the digest the run journaled. Empty: not compared.
	Digest string
	// Conform is flow.Conform. Nil: the viewer runs only its structural check (see Conformance).
	// Source says where the topology came from, as the page shows it.
	Conform func(ctx context.Context, j *agent.Journal, runID string) (bool, []string, error)
	Source  string
}

// Conformance is the verdict on whether the run followed the declared topology.
type Conformance struct {
	OK     bool     `json:"ok"`
	Method string   `json:"method"` // "plan.Conform", or "structural" (see structural)
	Source string   `json:"source"` // where the topology came from
	Diffs  []string `json:"diffs,omitempty"`
	Digest string   `json:"digest"` // "match", "mismatch", or "not compared"
}

// Graph is the declared topology with the run's path overlaid.
type Graph struct {
	Nodes   []GraphNode `json:"nodes"`
	Edges   []GraphEdge `json:"edges"`
	SVG     string      `json:"-"`
	Mermaid string      `json:"mermaid"`
	Width   int         `json:"-"`
	Height  int         `json:"-"`
	nw, nh  int         // node size: taller when a node carries a gate or a crash line
	gy      int         // the gap between rows: smaller in a gated graph, so its taller nodes fit a screen
	// ranks is the number of rows, and backs how far right the loop-back arcs reach.
	ranks int
}

// GraphNode is one declared node and what the run did with it.
type GraphNode struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Status string `json:"status"` // done, halted, attempted, waiting (at its approval gate), not-reached
	// Gate is the node's declared approval gate ("2 of 3", "one decision"), empty for none, and
	// GateStatus where its latest evaluation stands (see Gate.Status).
	Gate       string `json:"gate,omitempty"`
	GateStatus string `json:"gate_status,omitempty"`
	// GateLine and SigLine are what the node's box says about its gate, short enough to read in
	// a screenshot: "approved 2 of 3" or "waiting, 1 of 2 approvals", and "signatures verified" (or
	// not, and why in short). GateClass styles them: good, wait or bad.
	GateLine  string `json:"gate_line,omitempty"`
	SigLine   string `json:"sig_line,omitempty"`
	GateClass string `json:"gate_class,omitempty"`
	Iters     int    `json:"iters,omitempty"`
	Last      bool   `json:"last,omitempty"`
	Note      string `json:"note,omitempty"`  // a line under the box
	Title     string `json:"title,omitempty"` // the tooltip, when not "name: status"
	// Risk is a config node's safety class ("side effect", "read-only", "idempotent", "safety
	// unknown") and Crash its crash behavior in a few words; empty on a run's graph. Cex marks a
	// node on a violated assertion's counterexample path.
	Risk  string `json:"risk,omitempty"`
	Crash string `json:"crash,omitempty"`
	Cex   bool   `json:"cex,omitempty"`
	x, y  int
	rank  int
}

// GraphEdge is one declared edge or Switch arm, and whether the run took it.
type GraphEdge struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Label   string `json:"label,omitempty"`
	Taken   bool   `json:"taken"`
	Back    bool   `json:"back,omitempty"`
	Problem string `json:"problem,omitempty"` // a config problem at this edge or arm
	Cex     bool   `json:"cex,omitempty"`     // on a violated assertion's counterexample path
	// Change is how a flow diff changed this edge or arm (added, removed or changed), which
	// styles it; empty outside a diff.
	Change string `json:"change,omitempty"`

	d      string // the path, set by layout
	lx, ly int    // the label's center, set by layout
	arc    bool   // drawn as a loop-back arc down the right side, at x ax
	ax     int
}

// flowGraph overlays the run's journal on flow's topology. With no flow it returns no graph, and
// notes that the journal does not carry the topology.
func flowGraph(ctx context.Context, runID string, recs []agent.Record, flow *Flow, v *View) (*Graph, *Conformance, string) {
	status := map[string]string{}
	iters := map[string]int{}
	chosen := map[string]string{} // switch over -> last chosen target
	var last string
	halted := map[string]bool{}
	for _, h := range v.Halts {
		if n, _, ok := nodeOf(h.ID); ok && h.Op == "step" {
			halted[n] = true
		}
	}
	for _, r := range recs {
		name := r.Name
		if key, ok := strings.CutPrefix(attemptBase(name), "attempt:step:"); ok && r.Kind == agent.StepAttempt {
			if n, _, ok := nodeOf(key); ok {
				if status[n] == "" {
					status[n] = "attempted"
				}
				last = n
			}
			continue
		}
		if over, ok := strings.CutPrefix(name, "switch:"); ok {
			o, _ := splitIter(over)
			var t string
			_ = json.Unmarshal(r.Result, &t)
			chosen[o] = t
			continue
		}
		if n, it, ok := nodeOf(name); ok && !strings.Contains(strings.TrimPrefix(name, "node:"), ":step:") {
			status[n] = "done"
			if it >= 0 {
				iters[n] = max(iters[n], it+1)
			}
			last = n
		}
	}
	for n := range halted {
		status[n] = "halted"
	}

	if flow == nil {
		v.Notes = append(v.Notes, "this run journaled no topology (it was started before flow:topology was journaled): pass -topology (a JSON export of flow.Topology()) to draw it")
		return nil, nil, last
	}
	t := flow.Topology
	// Each gated node's latest gate (the last iteration's, in a loop).
	gateOf := map[string]Gate{}
	for _, gt := range v.Gates {
		if n, _, ok := nodeOf(gt.ToolUseID); ok {
			gateOf[n] = gt
		}
	}
	g := &Graph{}
	for _, n := range t.Nodes {
		st := status[n.Name]
		gn := GraphNode{Name: n.Name, Kind: string(n.Kind), Iters: iters[n.Name], Last: n.Name == last}
		if gd := n.Gate; gd != nil {
			gn.Gate = "one decision"
			if !gd.Single {
				gn.Gate = fmt.Sprintf("%d of %d", gd.Need, len(gd.Approvers))
			}
			if gt, ok := gateOf[n.Name]; ok && gt.Waiting && st == "" {
				st, last = "waiting", n.Name // the run stopped here, at the gate
			}
		}
		if st == "" {
			st = "not-reached"
		}
		gn.Status = st
		g.Nodes = append(g.Nodes, gn)
	}
	for i := range g.Nodes {
		g.Nodes[i].Last = g.Nodes[i].Name == last
	}
	waiting := map[string]bool{}
	for _, n := range g.Nodes {
		waiting[n.Name] = n.Status == "waiting"
	}
	ran := func(n string) bool { s := status[n]; return s == "done" || s == "halted" || s == "attempted" }
	reached := func(n string) bool { return ran(n) || waiting[n] } // the path into a node paused at its gate was taken
	for _, e := range t.Edges {
		g.Edges = append(g.Edges, GraphEdge{From: e.From, To: e.To, Taken: ran(e.From) && reached(e.To)})
	}
	for _, b := range t.Branches {
		for _, a := range b.Arms {
			label := "when"
			switch {
			case a.LoopBack:
				label = "loop, at most " + strconv.Itoa(a.LoopMax)
			case a.Else:
				label = "else"
			}
			taken := ran(b.Over) && reached(a.Target) && (chosen[b.Over] == a.Target || a.LoopBack && iters[a.Target] > 1)
			g.Edges = append(g.Edges, GraphEdge{From: b.Over, To: a.Target, Label: label, Taken: taken, Back: a.LoopBack})
		}
	}
	layout(g, t.Entry)
	markGates(g, v.Gates)
	g.Mermaid = mermaid(g)

	c := &Conformance{Digest: "not compared", Source: flow.Source}
	if flow.Digest != "" {
		c.Digest = "mismatch"
		if v.Digest == flow.Digest {
			c.Digest = "match"
		}
	}
	if flow.Conform != nil {
		c.Method = "plan.Conform"
		j, err := agent.NewJournal(staticStore{recs: recs})
		if err == nil {
			c.OK, c.Diffs, err = flow.Conform(ctx, j, runID)
		}
		if err != nil {
			c.Diffs = append(c.Diffs, "conform: "+err.Error())
		}
	} else {
		c.Method = "structural"
		c.Diffs = structural(t, recs, v.Flow)
		c.OK = len(c.Diffs) == 0
	}
	if c.Digest == "mismatch" {
		c.OK = false
		c.Diffs = append(c.Diffs, "flow:digest: the run journaled "+v.Digest+", the declared flow's digest is "+flow.Digest)
	}
	return g, c, last
}

// structural checks the journal against a topology the viewer holds only as data: the run is of
// this flow, every node record names a declared node, and every Switch choice is a declared arm.
// It is a subset of plan.Conform (which also replays routing, loops and completion), and is used
// only when the caller has no Flow to call Conform on.
func structural(t plan.Topology, recs []agent.Record, flowName string) []string {
	var diffs []string
	if flowName != "" && flowName != t.Flow {
		diffs = append(diffs, fmt.Sprintf("run:start records flow %q, the topology is of %q", flowName, t.Flow))
	}
	declared := map[string]bool{}
	for _, n := range t.Nodes {
		declared[n.Name] = true
	}
	arms := map[string]map[string]bool{}
	for _, b := range t.Branches {
		arms[b.Over] = map[string]bool{}
		for _, a := range b.Arms {
			arms[b.Over][a.Target] = true
		}
	}
	for _, r := range recs {
		name := r.Name
		if key, ok := strings.CutPrefix(attemptBase(name), "attempt:step:"); ok {
			name = key
		}
		if over, ok := strings.CutPrefix(name, "switch:"); ok {
			o, _ := splitIter(over)
			var target string
			_ = json.Unmarshal(r.Result, &target)
			if arms[o] == nil {
				diffs = append(diffs, r.Name+" (switch over undeclared node)")
			} else if target != "" && !arms[o][target] {
				diffs = append(diffs, r.Name+" -> "+target+" (unreachable arm taken)")
			}
			continue
		}
		if n, _, ok := nodeOf(name); ok && !declared[n] {
			diffs = append(diffs, r.Name+" (undeclared node)")
		}
	}
	return diffs
}

// nodeOf maps a plan node key ("node:<N>", "node:iter:<i>:<N>", or either followed by
// ":step:<S>") to N and i (-1 outside a loop iteration).
func nodeOf(key string) (string, int, bool) {
	rest, ok := strings.CutPrefix(key, "node:")
	if !ok {
		return "", -1, false
	}
	n, it := splitIter(rest)
	n, _, _ = strings.Cut(n, ":")
	return n, it, n != ""
}

// splitIter removes a leading "iter:<n>:" from a key segment.
func splitIter(s string) (string, int) {
	rest, ok := strings.CutPrefix(s, "iter:")
	if !ok {
		return s, -1
	}
	digits, node, ok := strings.Cut(rest, ":")
	n, err := strconv.Atoi(digits)
	if !ok || err != nil {
		return s, -1
	}
	return node, n
}

// markGates sets each gated node's gate lines from gates (after their signatures are checked, when
// they are) and draws the graph again.
func markGates(g *Graph, gates []Gate) {
	if g == nil {
		return
	}
	gateOf := map[string]Gate{}
	for _, gt := range gates {
		if n, _, ok := nodeOf(gt.ToolUseID); ok {
			gateOf[n] = gt
		}
	}
	for i := range g.Nodes {
		n := &g.Nodes[i]
		if n.Gate == "" {
			continue
		}
		gt, ok := gateOf[n.Name]
		if !ok {
			n.GateLine, n.GateClass = "approval "+n.Gate, "wait"
			continue
		}
		n.GateStatus = gt.Status
		if gt.Due != "" {
			n.GateStatus += "; " + gt.Due
		}
		n.GateLine, n.GateClass, n.SigLine = gateLines(gt)
	}
	g.SVG = svg(g)
}

// gateLines phrases a gate for its node's box: where it stands, its style, and its signatures.
func gateLines(gt Gate) (line, class, sig string) {
	state, _, _ := strings.Cut(gt.Status, " (")
	switch {
	case gt.Single:
		line = "approval: " + state
	case state == "approved":
		line = fmt.Sprintf("approved %d of %d", len(gt.ApprovedBy), len(gt.Approvers))
	default:
		line = fmt.Sprintf("%s, %d of %d approvals", state, len(gt.ApprovedBy), gt.Need)
	}
	if gt.Waiting {
		line = fmt.Sprintf("waiting, %d of %d approvals", len(gt.ApprovedBy), gt.Need)
	}
	class = "wait"
	switch state {
	case "approved":
		class = "good"
	case "denied", "closed, not approved", "expired":
		class = "bad"
	}
	// An approved gate with a certificate-bound approver is drawn good only when an audit re-checked its
	// identity; otherwise it says what the identity is as recorded.
	if gi := gt.Identity; gi != nil && gi.Bound && state == "approved" && gi.Class != identAudited {
		switch gi.Class {
		case identFinal:
			class, line = "wait", line+", identity final as recorded"
		case identBad:
			class, line = "bad", line+", identity NOT VERIFIED"
		default:
			class, line = "wait", line+", identity not final"
		}
	}
	switch {
	case gt.Identity != nil && gt.Identity.Bound:
		// The identity block says what the count rests on; the signature line of a bare-key gate would
		// claim more than this page checked.
		sig = "identity: " + gt.Identity.Line
	case gt.Single:
		sig = "one unsigned decision"
	case gt.SignaturesOK:
		sig = "signatures verified"
	case strings.HasPrefix(gt.Signatures, "signatures NOT"):
		sig, class = "signatures NOT verified", "bad"
	case gt.Tallied:
		sig = "signatures not checked"
	case gt.Waiting && len(gt.Pending) > 0:
		sig = "pending: " + strings.Join(gt.Pending, ", ")
	}
	return line, class, sig
}

// Headline is the gate's state in a few words, as the page's gate card and the node's box show it.
func (gt Gate) Headline() string { l, _, _ := gateLines(gt); return l }

// Class is the gate's style: good, wait or bad.
func (gt Gate) Class() string { _, c, _ := gateLines(gt); return c }

// SigLine is the gate's signature verdict in a few words.
func (gt Gate) SigLine() string { _, _, s := gateLines(gt); return s }

// mermaid renders the overlaid graph as Mermaid text, a fallback for any viewer that renders it.
func mermaid(g *Graph) string {
	var b strings.Builder
	b.WriteString("flowchart TD\n")
	id := func(n string) string {
		return "n" + strconv.Itoa(slices.IndexFunc(g.Nodes, func(x GraphNode) bool { return x.Name == n }))
	}
	for _, n := range g.Nodes {
		label := n.Name
		if n.Status == "declared" && n.Gate != "" {
			label += " | approval: " + n.Gate // the config page's Mermaid says what each gate requires
		}
		fmt.Fprintf(&b, "  %s[%s]:::%s\n", id(n.Name), mmd.Label(label), strings.ReplaceAll(n.Status, "-", ""))
	}
	for _, e := range g.Edges {
		arrow := "-->"
		if !e.Taken {
			arrow = "-.->"
		}
		if e.Label != "" {
			fmt.Fprintf(&b, "  %s %s|%s| %s\n", id(e.From), arrow, e.Label, id(e.To))
		} else {
			fmt.Fprintf(&b, "  %s %s %s\n", id(e.From), arrow, id(e.To))
		}
	}
	b.WriteString("  classDef done fill:#d8f0dd,stroke:#2f7d43\n  classDef halted fill:#fbe0dc,stroke:#b3261e\n  classDef attempted fill:#fff2cc,stroke:#9a6b00\n  classDef waiting fill:#e3ecfb,stroke:#2b5fb4\n  classDef notreached fill:#eee,stroke:#999,stroke-dasharray:4\n")
	return b.String()
}
