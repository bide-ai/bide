package runview

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bide-ai/bide/internal/term"
	"github.com/bide-ai/bide/plan"
)

// Diff is a flow definition compared with an earlier version of it: bide check -diff. Each side is
// read as BuildConfig reads a config (plan.InspectConfig), so with a Registry on both sides the
// page also shows the digest each side's runs journal.
type Diff struct {
	Old, New Config
}

// Change is one difference between two flow definitions: a node, an edge or Switch arm, the entry,
// or the flow's name, added, removed or changed.
type Change struct {
	Change string `json:"change"` // added, removed or changed
	What   string `json:"what"`   // node, edge, entry or flow
	Node   string `json:"node,omitempty"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	// Details say what changed, one item each ("block charge -> refund", "approval 1 of 3 -> 2 of
	// 3"); for an added or removed node, what it was.
	Details []string `json:"details,omitempty"`
	// Gate is how the node's approval gate changed: weakened (a set of decisions the new gate
	// accepts would not have passed the old one: the gate was removed, its need lowered, or an
	// approver added without raising the need to match), strengthened (the reverse, and not
	// weakened), or reordered (the same approvers and need in another policy order). Removing or
	// lengthening a gate's timeout is a weakening (a run waits longer, and late decisions count), and
	// adding or shortening one a strengthening (the gate fails closed sooner). Empty when the
	// gate did not change. A removed node's gate counts as removed.
	Gate string `json:"gate,omitempty"`
	// Weakening says what a weakened change weakened: "gate" (an approval policy: the gate was
	// removed, loosened, or lost its exclusion of the requester or its deadline), "path" (an
	// effect node newly reachable without passing an approval gate) or "assertion" (an assert line
	// removed or loosened). Empty for a change that weakened nothing.
	Weakening string `json:"weakening,omitempty"`
}

// String is the change as one line: "changed node charge: approval 1 of 3 -> 2 of 3 (strengthened)".
func (c Change) String() string {
	var where string
	switch c.What {
	case "node":
		where = "node " + c.Node
	case "edge":
		where = "edge " + c.From + " -> " + c.To
	case "switch":
		where = "switch " + c.Node
	default:
		where = c.What
	}
	s := c.Change + " " + where
	if len(c.Details) > 0 {
		s += ": " + strings.Join(c.Details, "; ")
	}
	return s
}

// DiffView is what the diff page shows.
type DiffView struct {
	Name    string `json:"name"`
	OldName string `json:"old_name"`
	Flow    string `json:"flow"`
	// Typed reports both sides were checked against a plan.Registry, so their digests are known
	// (unless a side has errors).
	Typed     bool     `json:"typed"`
	OldDigest string   `json:"old_digest,omitempty"`
	NewDigest string   `json:"new_digest,omitempty"`
	Changes   []Change `json:"changes"`
	// Weakened and Strengthened name each change that made the flow's approval checks weaker or
	// stronger, with how: "charge: approval 2 of 3 -> 1 of 3" for a gate, "receipt: reachable
	// without an approval gate (validate -> receipt), not before" for a new ungated path to an
	// effect, "assertion removed: every path to charge passes approval >= 2 of 3" for an assert line.
	Weakened     []string `json:"weakened,omitempty"`
	Strengthened []string `json:"strengthened,omitempty"`
	Counts       Counts   `json:"counts"`
	// OldErrors and NewErrors count each side's config errors (bide check lists them): a side
	// with errors is compared as far as it reads.
	OldErrors int    `json:"old_errors"`
	NewErrors int    `json:"new_errors"`
	Graph     *Graph `json:"graph,omitempty"`
	// Verdict is the answer the page opens with (see diffVerdict); it is not in the JSON view.
	Verdict Verdict `json:"-"`
	// weakKinds is the Weakening of each Weakened entry, in order.
	weakKinds []string
}

// Counts are a diff's changes by kind.
type Counts struct {
	NodesAdded   int `json:"nodes_added"`
	NodesRemoved int `json:"nodes_removed"`
	NodesChanged int `json:"nodes_changed"`
	EdgesAdded   int `json:"edges_added"`
	EdgesRemoved int `json:"edges_removed"`
	EdgesChanged int `json:"edges_changed"`
}

// DigestLine says how the digest changed, in words, for the text summary and the page.
func (v *DiffView) DigestLine() string {
	switch {
	case v.OldDigest != "" && v.NewDigest != "" && v.OldDigest == v.NewDigest:
		return "digest unchanged: " + v.NewDigest
	case v.OldDigest != "" && v.NewDigest != "":
		return "digest " + v.OldDigest + " -> " + v.NewDigest
	case v.Typed:
		return "digest not computed: a side has errors"
	}
	return "digest not computed: the check is structural (the digest commits to the blocks' Go types; see plan/planview)"
}

// BuildDiff compares d.New with d.Old and draws the new flow with the old one's removed nodes and
// edges as ghosts. It fails only when a side cannot be read as a flow config at all.
func BuildDiff(d Diff) (*DiffView, error) {
	o := plan.InspectConfig(d.Old.Data, d.Old.Registry)
	n := plan.InspectConfig(d.New.Data, d.New.Registry)
	for _, s := range []struct {
		name string
		r    plan.ConfigReport
	}{{d.Old.Name, o}, {d.New.Name, n}} {
		if len(s.r.Topology.Nodes) == 0 && s.r.Errors() {
			return nil, fmt.Errorf("%s: %s", s.name, s.r.Problems[0].Message)
		}
	}
	v := &DiffView{Name: d.New.Name, OldName: d.Old.Name, Flow: n.Topology.Flow, Typed: o.Typed && n.Typed,
		OldDigest: o.Digest, NewDigest: n.Digest, OldErrors: errorCount(o), NewErrors: errorCount(n)}
	v.Changes = diffChanges(o, n, safeties(d.Old.Data), safeties(d.New.Data))
	for _, c := range v.Changes {
		switch {
		case c.What == "node" && c.Change == "added":
			v.Counts.NodesAdded++
		case c.What == "node" && c.Change == "removed":
			v.Counts.NodesRemoved++
		case c.What == "node":
			v.Counts.NodesChanged++
		case c.What == "edge" && c.Change == "added":
			v.Counts.EdgesAdded++
		case c.What == "edge" && c.Change == "removed":
			v.Counts.EdgesRemoved++
		case c.What == "edge":
			v.Counts.EdgesChanged++
		}
		switch c.Gate {
		case "weakened":
			v.Weakened = append(v.Weakened, changeSummary(c, "loosened"))
			v.weakKinds = append(v.weakKinds, c.Weakening)
		case "strengthened":
			v.Strengthened = append(v.Strengthened, changeSummary(c, "tightened"))
		}
	}
	v.Graph = diffGraph(o.Topology, n.Topology, v.Changes)
	v.Verdict = diffVerdict(v)
	return v, nil
}

// CheckDiff writes the diff as text, for CI and a pull request comment: one line per change, a
// line per weakened approval gate, and a summary line with the counts and the digest, each
// prefixed with the new config's name. It reports whether an approval policy was weakened.
func CheckDiff(w io.Writer, d Diff) (weakened bool, err error) {
	return CheckDiffStyled(w, d, term.Style{})
}

// CheckDiffStyled is CheckDiff in st's colors (bide check -diff -check on a terminal): added
// green, removed red, changed amber, and each weakened approval gate in bold red, called out
// before the changes rather than after them. With color off it writes what CheckDiff writes.
func CheckDiffStyled(w io.Writer, d Diff, st term.Style) (weakened bool, err error) {
	v, err := BuildDiff(d)
	if err != nil {
		return false, err
	}
	name := st.Dim(v.Name)
	weak := func() {
		for i, s := range v.Weakened {
			what := "weakened: "
			if i < len(v.weakKinds) && v.weakKinds[i] == "gate" {
				what = "approval policy weakened: "
			}
			fmt.Fprintf(w, "%s%s: %s\n", st.Cross(), name, st.BoldFail(what+s))
		}
	}
	if st.On() {
		weak()
	}
	for _, c := range v.Changes {
		fmt.Fprintf(w, "%s%s: %s\n", changeMark(st, c), name, changeText(st, c))
	}
	if !st.On() {
		weak()
	}
	for _, s := range []struct {
		which, name string
		n           int
	}{{"old", v.OldName, v.OldErrors}, {"new", v.Name, v.NewErrors}} {
		if s.n > 0 {
			fmt.Fprintf(w, "%s%s: %s the %s config has %d error(s), compared as far as it reads; bide check %s lists them\n", st.Bang(), name, st.Warn("note:"), s.which, s.n, st.Path(s.name))
		}
	}
	if len(v.Changes) == 0 {
		fmt.Fprintf(w, "%s%s: no changes from %s; %s\n", st.Pass(), name, v.OldName, st.Dim(v.DigestLine()))
	} else {
		c := v.Counts
		changes := "changes"
		if len(v.Changes) == 1 {
			changes = "change"
		}
		counts := fmt.Sprintf("nodes: %d added, %d removed, %d changed; edges: %d added, %d removed, %d changed", c.NodesAdded, c.NodesRemoved, c.NodesChanged, c.EdgesAdded, c.EdgesRemoved, c.EdgesChanged)
		if st.On() { // on a terminal the counts and the digest go on lines of their own, to fit its width
			fmt.Fprintf(w, "%s: %s from %s\n%s%s\n%s%s\n", name, st.Bold(fmt.Sprintf("%d %s", len(v.Changes), changes)), v.OldName,
				indent, st.Dim(counts), indent, st.Dim(v.DigestLine()))
		} else {
			fmt.Fprintf(w, "%s: %d %s from %s (%s); %s\n", name, len(v.Changes), changes, v.OldName, counts, v.DigestLine())
		}
	}
	return len(v.Weakened) > 0, nil
}

// diffRoutes serves the diff page at / and its view at /diff.json.
func diffRoutes(mux *http.ServeMux, d Diff) {
	serve := func(asJSON bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			v, err := BuildDiff(d)
			switch {
			case err != nil:
				http.Error(w, err.Error(), http.StatusInternalServerError)
			case asJSON:
				writeJSON(w, v)
			default:
				render(w, "diff.html", v)
			}
		}
	}
	mux.HandleFunc("GET /{$}", serve(false))
	mux.HandleFunc("GET /diff.json", serve(true))
}

func errorCount(r plan.ConfigReport) int {
	n := 0
	for _, p := range r.Problems {
		if p.Severity == "error" {
			n++
		}
	}
	return n
}

// safeties reads each node's safety as the config writes it ("" keeps the registered block's),
// which the Topology does not carry. The config has been read strictly by plan.InspectConfig;
// a config it could not read has no nodes here either.
func safeties(data []byte) map[string]string {
	var c struct {
		Nodes []struct {
			Name   string `json:"name"`
			Safety string `json:"safety"`
		} `json:"nodes"`
	}
	m := map[string]string{}
	if json.Unmarshal(data, &c) == nil {
		for _, n := range c.Nodes {
			m[n.Name] = n.Safety
		}
	}
	return m
}

// diffChanges lists the changes from o to n: the flow's name and entry, then the nodes (in the new
// config's order, then the removed ones in the old config's), then the edges and arms likewise.
func diffChanges(o, n plan.ConfigReport, oldSafety, newSafety map[string]string) []Change {
	var cs []Change
	ot, nt := o.Topology, n.Topology
	if ot.Flow != nt.Flow {
		cs = append(cs, Change{Change: "changed", What: "flow", Details: []string{ot.Flow + " -> " + nt.Flow}})
	}
	if ot.Entry != nt.Entry {
		cs = append(cs, Change{Change: "changed", What: "entry", Details: []string{ot.Entry + " -> " + nt.Entry}})
	}
	oldNode := map[string]plan.TopologyNode{}
	for _, x := range ot.Nodes {
		oldNode[x.Name] = x
	}
	ren := renamedNodes(o, n)
	renRev := map[string]string{}
	for from, to := range ren {
		renRev[to] = from
	}
	uOld, uNew := ungatedEffects(o), ungatedEffects(n)
	newNode := map[string]bool{}
	for _, x := range nt.Nodes {
		newNode[x.Name] = true
		was, ok := oldNode[x.Name]
		if from, renamed := renRev[x.Name]; renamed {
			c := Change{Change: "renamed", What: "node", Node: x.Name, Details: []string{from + " -> " + x.Name + ": the same block, safety, gate and edges"}}
			// The node is the same, but a gate upstream of it may be gone.
			if uNew[x.Name] && !uOld[from] {
				c.Gate, c.Weakening = "weakened", "path"
				c.Details = append(c.Details, ungatedWhy(n, x.Name)+" (weakened)")
			}
			cs = append(cs, c)
			continue
		}
		if !ok {
			c := Change{Change: "added", What: "node", Node: x.Name, Details: []string{nodeKind(x)}}
			if s := newSafety[x.Name]; s != "" {
				c.Details = append(c.Details, "safety "+s)
			}
			if x.Gate != nil {
				c.Details = append(c.Details, "approval "+gateFull(x.Gate))
			}
			if uNew[x.Name] {
				c.Gate, c.Weakening = "weakened", "path"
				c.Details = append(c.Details, "new node, "+ungatedWhy(n, x.Name)+" (weakened)")
			}
			cs = append(cs, c)
			continue
		}
		c := Change{Change: "changed", What: "node", Node: x.Name}
		if was.Kind != x.Kind && was.Kind != "" && x.Kind != "" {
			c.Details = append(c.Details, fmt.Sprintf("kind %s -> %s", was.Kind, x.Kind))
		}
		if was.Block != x.Block {
			c.Details = append(c.Details, fmt.Sprintf("block %s -> %s", was.Block, x.Block))
		}
		if a, b := oldSafety[x.Name], newSafety[x.Name]; a != b {
			c.Details = append(c.Details, fmt.Sprintf("safety %s -> %s", safetyText(a), safetyText(b)))
		}
		if was.In != x.In && was.In != "" && x.In != "" {
			c.Details = append(c.Details, fmt.Sprintf("input type %s -> %s", was.In, x.In))
		}
		if was.Out != x.Out && was.Out != "" && x.Out != "" {
			c.Details = append(c.Details, fmt.Sprintf("output type %s -> %s", was.Out, x.Out))
		}
		if g := gateChange(was.Gate, x.Gate); g != "" {
			c.Gate = g
			if g == "weakened" {
				c.Weakening = "gate"
			}
			c.Details = append(c.Details, gateMove(was.Gate, x.Gate)+" ("+g+")")
		}
		switch {
		case c.Gate == "weakened":
		case uNew[x.Name] && !uOld[x.Name]:
			c.Gate, c.Weakening = "weakened", "path"
			c.Details = append(c.Details, ungatedWhy(n, x.Name)+" (weakened)")
		case uNew[x.Name] && was.Block != x.Block && !(o.Typed && n.Typed):
			// Without the Registry the block's own approval gate (ToolSpec.Approval) is not visible: a new
			// block on a node no config gate guards may have none.
			c.Gate, c.Weakening = "weakened", "block"
			c.Details = append(c.Details, "block changed on a node no declared gate guards: the new block's own approval gate, if any, is not visible without the plan.Registry (weakened)")
		}
		if len(c.Details) > 0 {
			cs = append(cs, c)
		}
	}
	for _, x := range ot.Nodes {
		if newNode[x.Name] || ren[x.Name] != "" {
			continue
		}
		c := Change{Change: "removed", What: "node", Node: x.Name, Details: []string{nodeKind(x)}}
		if x.Gate != nil {
			c.Gate, c.Weakening = "weakened", "gate"
			c.Details = append(c.Details, gateMove(x.Gate, nil)+", with the node (weakened)")
		}
		cs = append(cs, c)
	}
	oe, ne := edgesOf(ot), edgesOf(nt)
	for _, k := range ne.order {
		if _, ok := oe.label[k]; !ok {
			cs = append(cs, edgeChange("added", k, ne.label[k]))
		} else if oe.label[k] != ne.label[k] {
			cs = append(cs, edgeChange("changed", k, oe.label[k]+" -> "+ne.label[k]))
		}
	}
	for _, k := range oe.order {
		if _, ok := ne.label[k]; !ok {
			cs = append(cs, edgeChange("removed", k, oe.label[k]))
		}
	}
	cs = append(cs, switchChanges(o, n, uNew)...)
	return append(cs, assertionChanges(o, n)...)
}

// assertionChanges compares the config's assert lines. A line the new config lacks is removed
// (weakened), unless a line only the new config has is stronger than it (tightened, strengthened: a
// higher need, a smaller approver set, or the requester excluded), or weaker (loosened, weakened) for
// the same node; a line only the new config has, and relates to none removed, is added.
func assertionChanges(o, n plan.ConfigReport) []Change {
	var cs []Change
	text := func(r plan.ConfigReport) []string {
		var out []string
		for _, a := range r.Assertions {
			out = append(out, a.Text)
		}
		return out
	}
	oldT, newT := text(o), text(n)
	same := func(a, b string) bool { return plan.CompareAssertions(a, b) == "equal" }
	inOther := func(list []string, t string) bool {
		return slices.ContainsFunc(list, func(x string) bool { return same(x, t) })
	}
	var added []string
	for _, t := range newT {
		if !inOther(oldT, t) {
			added = append(added, t)
		}
	}
	used := map[string]bool{}
	for _, t := range oldT {
		if inOther(newT, t) {
			continue
		}
		rel, with := "", ""
		for _, a := range added {
			if used[a] {
				continue
			}
			switch r := plan.CompareAssertions(t, a); {
			case r == "stronger" && rel != "stronger":
				rel, with = r, a
			case r == "weaker" && rel == "":
				rel, with = r, a
			}
		}
		switch rel {
		case "stronger":
			used[with] = true
			cs = append(cs, Change{Change: "changed", What: "assertion", Gate: "strengthened", Details: []string{t + " -> " + with + " (strengthened)"}})
		case "weaker":
			used[with] = true
			cs = append(cs, Change{Change: "changed", What: "assertion", Gate: "weakened", Weakening: "assertion", Details: []string{t + " -> " + with + " (weakened)"}})
		default:
			cs = append(cs, Change{Change: "removed", What: "assertion", Gate: "weakened", Weakening: "assertion", Details: []string{t + " (weakened)"}})
		}
	}
	for _, a := range added {
		if !used[a] {
			cs = append(cs, Change{Change: "added", What: "assertion", Details: []string{a}})
		}
	}
	return cs
}

// changeSummary is c as an entry of DiffView.Weakened or Strengthened. loosened is the verb of an
// assertion that changed ("loosened", or "tightened" for a strengthening).
func changeSummary(c Change, loosened string) string {
	detail, _, _ := strings.Cut(strings.Join(c.Details, "; "), " (")
	if c.What == "assertion" {
		// The detail was cut at its first " (", which an assertion's text never holds.
		if c.Change == "removed" {
			return "assertion removed: " + detail
		}
		return "assertion " + loosened + ": " + detail
	}
	if c.Weakening == "path" || c.Weakening == "block" || c.Weakening == "condition" {
		d := c.Details[len(c.Details)-1]
		d = strings.TrimSuffix(d, " (weakened)")
		return c.Node + ": " + d
	}
	return c.Node + ": " + gateDetail(c)
}

func edgeChange(change string, k [2]string, label string) Change {
	c := Change{Change: change, What: "edge", From: k[0], To: k[1]}
	if label != "" {
		c.Details = []string{label}
	}
	return c
}

// edges are a topology's edges and Switch arms by (from, to), each with its label ("when large",
// "else", "loop retry, at most 3"; several arms to one target joined with " / ").
type edges struct {
	order [][2]string
	label map[[2]string]string
	back  map[[2]string]bool
}

func edgesOf(t plan.Topology) edges {
	e := edges{label: map[[2]string]string{}, back: map[[2]string]bool{}}
	add := func(from, to, label string, back bool) {
		k := [2]string{from, to}
		if l, ok := e.label[k]; ok {
			if label != "" {
				e.label[k] = strings.TrimPrefix(l+" / "+label, " / ")
			}
		} else {
			e.order = append(e.order, k)
			e.label[k] = label
		}
		e.back[k] = e.back[k] || back
	}
	for _, x := range t.Edges {
		add(x.From, x.To, "", false)
	}
	for _, b := range t.Branches {
		for _, a := range b.Arms {
			add(b.Over, a.Target, armLabel(a), a.LoopBack)
		}
	}
	return e
}

// armLabel is an arm's label, as the config page draws it.
func armLabel(a plan.TopologyBranchArm) string {
	switch {
	case a.LoopBack:
		return fmt.Sprintf("loop %s, at most %d", a.Pred, a.LoopMax)
	case a.Else:
		return "else"
	}
	return "when " + a.Pred
}

func nodeKind(x plan.TopologyNode) string {
	switch x.Kind {
	case plan.NodeKindJoin:
		return "join, merge " + x.Block
	case "":
		return "block " + x.Block
	}
	return string(x.Kind) + " " + x.Block
}

func safetyText(s string) string {
	if s == "" {
		return "(the block's)"
	}
	return s
}

// gateShort is a gate as "2 of 3", "2 of 3 excluding the requester", "one decision" or "none", with
// ", times out after 24h" for a gate that has a deadline.
func gateShort(g *plan.TopologyGate) string { return gateShortT(g, false) }

// gateShortT is gateShort; with showNone, a gate without a deadline says ", no timeout", so a move
// between a gate with one and a gate without reads as such.
func gateShortT(g *plan.TopologyGate, showNone bool) string {
	switch {
	case g == nil:
		return "none"
	case g.Single:
		return "one decision" + timeoutSuffix(g, showNone)
	}
	return fmt.Sprintf("%d of %d", g.Need, len(g.Approvers)) + exclusionText(g) + timeoutSuffix(g, showNone)
}

// gateFull is a gate with its approvers: "2 of 3: ops, finance, risk".
func gateFull(g *plan.TopologyGate) string { return gateFullT(g, false) }

func gateFullT(g *plan.TopologyGate, showNone bool) string {
	if g == nil || g.Single {
		return gateShortT(g, showNone)
	}
	return gateText(g.Need, g.Approvers) + exclusionText(g) + timeoutSuffix(g, showNone)
}

// exclusionText is " excluding the requester" for a gate that excludes the run's requester.
func exclusionText(g *plan.TopologyGate) string {
	if g.ExcludePrincipal {
		return " excluding the requester"
	}
	return ""
}

// timeoutSuffix is ", times out after 24h" for a gate with a deadline; for one without, ", no
// timeout" when showNone, else "".
func timeoutSuffix(g *plan.TopologyGate, showNone bool) string {
	switch t := g.TimeoutText(); {
	case t != "":
		return ", times out after " + t
	case showNone:
		return ", no timeout"
	}
	return ""
}

// gateMove is a gate's change, "approval 1 of 3 -> 2 of 3", with the approvers when they differ and
// a side's missing timeout spelled out when only the other has one.
func gateMove(a, b *plan.TopologyGate) string {
	showNone := a != nil && b != nil && (a.TimeoutMS > 0) != (b.TimeoutMS > 0)
	if a != nil && b != nil && !a.Single && !b.Single && slices.Equal(a.Approvers, b.Approvers) {
		return "approval " + gateShortT(a, showNone) + " -> " + gateShortT(b, showNone)
	}
	return "approval " + gateFullT(a, showNone) + " -> " + gateFullT(b, showNone)
}

// gateDetail is the gate item of c's details, without its verdict.
func gateDetail(c Change) string {
	for _, d := range c.Details {
		if strings.HasPrefix(d, "approval ") {
			d, _, _ = strings.Cut(d, " (")
			d, _, _ = strings.Cut(d, ", with the node")
			return d
		}
	}
	return ""
}

// gateChange classifies a gate's change from a to b: weakened, strengthened, reordered, or "" for
// none.
func gateChange(a, b *plan.TopologyGate) string {
	if gateEqual(a, b) {
		return ""
	}
	switch {
	case !accepts(a, b):
		return "weakened"
	case !accepts(b, a):
		return "strengthened"
	}
	return "reordered"
}

func gateEqual(a, b *plan.TopologyGate) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Single == b.Single && a.Need == b.Need && slices.Equal(a.Approvers, b.Approvers) && a.ExcludePrincipal == b.ExcludePrincipal && a.TimeoutMS == b.TimeoutMS
}

// accepts reports whether gate a accepts everything gate b accepts: no approval set passes b and
// fails a (acceptsDecisions), and no decision on time under b is late under a (acceptsTiming). No gate accepts everything; a gate accepts nothing that passes no gate. A
// single-decision gate accepts any gate's decisions (each needs at least one), and only another
// single-decision gate is accepted by it. For need k of approvers A and need k' of approvers B, the
// set that passes b with the fewest approvers from A takes every approver B adds outside A, so a
// accepts b exactly when k' - |B \ A| >= k. A gate that excludes the run's requester does not
// accept the decisions of one that does not: they may count the requester's approval.
func accepts(a, b *plan.TopologyGate) bool {
	return acceptsDecisions(a, b) && acceptsTiming(a, b)
}

// acceptsTiming reports whether gate a gives approvers at least as long as gate b: a has no
// deadline, or both do and a's is no shorter. A deadline makes a gate fail closed once it passes, so
// removing or lengthening one lets a run wait, and accept a decision, later than the old gate did.
func acceptsTiming(a, b *plan.TopologyGate) bool {
	switch {
	case a == nil || a.TimeoutMS <= 0:
		return true
	case b == nil || b.TimeoutMS <= 0:
		return false
	}
	return a.TimeoutMS >= b.TimeoutMS
}

func acceptsDecisions(a, b *plan.TopologyGate) bool {
	switch {
	case a == nil:
		return true
	case b == nil:
		return false
	case a.Single:
		return true
	case b.Single:
		return false
	case a.ExcludePrincipal && !b.ExcludePrincipal:
		return false
	}
	outside := 0
	for _, id := range b.Approvers {
		if !slices.Contains(a.Approvers, id) {
			outside++
		}
	}
	return b.Need-outside >= a.Need
}

// diffGraph draws the new topology with the old one's removed nodes and edges added as ghosts (each
// removed node placed after the node it followed in the old config), every node and edge styled by
// its change.
func diffGraph(ot, nt plan.Topology, cs []Change) *Graph {
	nodeCh := map[string]Change{}
	edgeCh := map[[2]string]Change{}
	for _, c := range cs {
		switch c.What {
		case "node":
			nodeCh[c.Node] = c
		case "edge":
			edgeCh[[2]string{c.From, c.To}] = c
		}
	}
	inNew := map[string]bool{}
	for _, x := range nt.Nodes {
		inNew[x.Name] = true
	}
	after := map[string][]plan.TopologyNode{} // a node in both -> the removed nodes that followed it
	prev := ""
	for _, x := range ot.Nodes {
		if inNew[x.Name] {
			prev = x.Name
			continue
		}
		after[prev] = append(after[prev], x)
	}
	g := &Graph{}
	declared := map[string]bool{}
	add := func(x plan.TopologyNode, old *plan.TopologyGate) {
		declared[x.Name] = true
		c, ok := nodeCh[x.Name]
		gn := GraphNode{Name: x.Name, Kind: nodeKind(x), Status: "unchanged", Title: x.Name + ": unchanged"}
		if ok {
			gn.Status, gn.Title = c.Change, x.Name+": "+c.Change+"\n"+strings.Join(c.Details, "\n")
		}
		gate := x.Gate
		if c.Change == "removed" {
			gate = nil
		}
		switch {
		case old == nil && gate == nil:
		case c.Gate == "":
			gn.Gate, gn.GateLine = gateShort(gate), "approval "+gateShort(gate)
		default:
			gn.Gate = gateShort(old)
			if gate != nil {
				gn.Gate = gateShort(gate)
			}
			gn.GateLine = "approval " + gateShort(old) + " -> " + gateShort(gate)
			if c.Change == "removed" {
				gn.GateLine = "approval " + gateShort(old) + " removed"
			}
			gn.SigLine, gn.GateClass = c.Gate, "wait"
			switch c.Gate {
			case "weakened":
				gn.SigLine, gn.GateClass = "WEAKENED", "bad"
			case "strengthened":
				gn.GateClass = "good"
			}
		}
		var notes []string
		for _, d := range c.Details {
			if c.Change == "changed" && !strings.HasPrefix(d, "approval ") {
				notes = append(notes, d)
			}
		}
		gn.Note = strings.Join(notes, "; ")
		g.Nodes = append(g.Nodes, gn)
	}
	oldGate := map[string]*plan.TopologyGate{}
	for _, x := range ot.Nodes {
		oldGate[x.Name] = x.Gate
	}
	for _, x := range after[""] {
		add(x, x.Gate)
	}
	for _, x := range nt.Nodes {
		add(x, oldGate[x.Name])
		for _, r := range after[x.Name] {
			add(r, r.Gate)
		}
	}
	addEnd := func(name string) {
		if name != "" && !declared[name] {
			declared[name] = true
			g.Nodes = append(g.Nodes, GraphNode{Name: name, Kind: "named by an edge", Status: "undeclared"})
		}
	}
	oe, ne := edgesOf(ot), edgesOf(nt)
	edge := func(k [2]string, label string, back bool) {
		addEnd(k[0])
		addEnd(k[1])
		e := GraphEdge{From: k[0], To: k[1], Label: label, Back: back}
		if c, ok := edgeCh[k]; ok {
			e.Change = c.Change
			if c.Change == "changed" {
				e.Label = c.Details[0]
			}
		}
		g.Edges = append(g.Edges, e)
	}
	for _, k := range ne.order {
		edge(k, ne.label[k], ne.back[k])
	}
	for _, k := range oe.order {
		if _, ok := ne.label[k]; !ok {
			edge(k, oe.label[k], oe.back[k])
		}
	}
	layout(g, nt.Entry)
	return g
}

// readOld reads -diff's argument, the earlier version of the config at newPath. A path to an
// existing file is read as that file. Otherwise the argument is a git revision, read with git show
// from the working directory: "<rev>:<path>" is git's own form (the path from the top of the
// repository, or from the working directory when it starts with ./), and a bare "<rev>" means
// newPath as it was at that revision. The YAML or JSON format follows the path's extension.
func readOld(arg, newPath string, toJSON func([]byte) ([]byte, error)) (Config, error) {
	if _, err := os.Stat(arg); err == nil {
		return readConfig(arg, toJSON)
	}
	if arg == "" || strings.HasPrefix(arg, "-") {
		return Config{}, fmt.Errorf("-diff %q: no such file, and not a git revision", arg)
	}
	spec, dir, path := arg, "", ""
	if _, p, ok := strings.Cut(arg, ":"); ok {
		path = p
	} else {
		// A bare revision: the new file's path at that revision, from the file's own directory.
		dir, path = filepath.Dir(newPath), filepath.Base(newPath)
		spec = arg + ":./" + filepath.ToSlash(path)
	}
	if _, err := exec.LookPath("git"); err != nil {
		return Config{}, fmt.Errorf("-diff %q: no such file, and git (to read it as a revision) is not available", arg)
	}
	cmd := exec.Command("git", "show", spec)
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return Config{}, fmt.Errorf("-diff %q: no such file, and git show %s failed: %s", arg, spec, cmp.Or(strings.TrimSpace(stderr.String()), err.Error()))
	}
	if len(out) > maxInput {
		return Config{}, fmt.Errorf("-diff %q: larger than %d bytes", arg, maxInput)
	}
	return configFrom(spec, path, out, toJSON)
}
