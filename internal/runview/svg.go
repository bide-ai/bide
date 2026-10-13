package runview

import (
	"fmt"
	"html"
	"strconv"
	"strings"
)

// riskCSS is each safety class's style on a config node, and riskIcon its glyph.
var (
	riskCSS  = map[string]string{"side effect": "risk-side", "read-only": "risk-read", "idempotent": "risk-idem", "safety unknown": "risk-unknown"}
	riskIcon = map[string]string{"side effect": "side", "read-only": "read", "idempotent": "idem", "safety unknown": "unknown"}
	// statusIcon is the glyph of a node's status, when it has no safety class. Status is never
	// carried by color alone: the glyph and the words say it too.
	statusIcon = map[string]string{
		"done": "ok", "halted": "halt", "attempted": "clock", "waiting": "wait", "not-reached": "idle",
		"added": "plus", "removed": "minus", "changed": "tilde", "unchanged": "node",
		"problem": "halt", "warning": "halt", "undeclared": "unknown", "declared": "node",
	}
)

// nodeIconName is the glyph a node wears.
func nodeIconName(n GraphNode) string {
	if i, ok := riskIcon[n.Risk]; ok {
		return i
	}
	if i, ok := statusIcon[n.Status]; ok {
		return i
	}
	return "node"
}

// statusWords phrases a node's status for its box.
func statusWords(s string) string {
	switch s {
	case "not-reached":
		return "not reached"
	case "undeclared":
		return "not declared"
	}
	return s
}

// gateBadge is a gate in two numbers for the node's corner: "2/3", or "1/1" for one decision.
func gateBadge(gate string) string {
	if gate == "one decision" {
		return "1/1"
	}
	var need, of int
	if _, err := fmt.Sscanf(gate, "%d of %d", &need, &of); err == nil {
		return fmt.Sprintf("%d/%d", need, of)
	}
	return "gate"
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// svg draws the graph: rounded nodes with a glyph and a gate badge, edges on spread ports, the
// path taken (or a counterexample) as a flowing dash. Every label is escaped. The drawing is
// complete without script: the hover, click and pan of graph.js only add to it.
func svg(g *Graph) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="graph" viewBox="0 0 %d %d" width="%d" height="%d" role="group" aria-label="flow graph: %d nodes, %d edges">`,
		g.Width, g.Height, g.Width, g.Height, len(g.Nodes), len(g.Edges))
	b.WriteString(`<defs>`)
	for _, m := range []struct{ id, cls string }{{"arrow", "arrowhead"}, {"arrow-taken", "arrowhead taken"}, {"arrow-problem", "arrowhead problem"},
		{"arrow-added", "arrowhead added"}, {"arrow-removed", "arrowhead removed"}, {"arrow-changed", "arrowhead changed"}} {
		fmt.Fprintf(&b, `<marker id="%s" viewBox="0 0 10 10" refX="9" refY="5" markerUnits="userSpaceOnUse" markerWidth="11" markerHeight="11" orient="auto-start-reverse"><path d="M0,0 L10,5 L0,10 z" class="%s"/></marker>`, m.id, m.cls)
	}
	used := map[string]bool{"gate": true}
	for _, n := range g.Nodes {
		used[nodeIconName(n)] = true
	}
	var names []string
	for _, n := range iconNames() {
		if used[n] {
			names = append(names, n)
		}
	}
	b.WriteString(iconDefs("gi-", names...))
	b.WriteString(`</defs>`)

	b.WriteString(`<g class="edges">`)
	for _, e := range g.Edges {
		if e.d == "" {
			continue
		}
		cls, mk := "edge", "arrow"
		if e.Taken {
			cls, mk = "edge taken", "arrow-taken"
		}
		if e.Problem != "" {
			cls, mk = "edge problem", "arrow-problem"
		}
		if e.Cex {
			cls, mk = "edge cex", "arrow-problem"
		}
		if e.Change != "" {
			cls, mk = "edge "+e.Change, "arrow-"+e.Change
		}
		if e.arc {
			cls += " back"
		}
		title := e.From + " → " + e.To
		if e.Label != "" {
			title += " (" + e.Label + ")"
		}
		switch {
		case e.Taken:
			title += ": taken"
		case e.Cex:
			title += ": on the counterexample path"
		case e.Change != "":
			title += ": " + e.Change
		}
		fmt.Fprintf(&b, `<g class="edge-g" data-from="%s" data-to="%s"><path class="%s" d="%s" marker-end="url(#%s)"/><title>%s</title>`,
			html.EscapeString(e.From), html.EscapeString(e.To), cls, e.d, mk, html.EscapeString(title))
		if e.Label != "" {
			w, h := labelSize(e.Label)
			fmt.Fprintf(&b, `<g class="elabel-g"><rect x="%d" y="%d" width="%d" height="%d" rx="%d"/><text class="elabel" x="%d" y="%d">%s</text></g>`,
				e.lx-w/2, e.ly-h/2, w, h, h/2, e.lx, e.ly+4, html.EscapeString(e.Label))
		}
		if e.Problem != "" {
			w, _ := labelSize(e.Label)
			mx := e.lx - w/2 - 12
			if e.Label == "" {
				mx = e.lx
			}
			fmt.Fprintf(&b, `<g class="edge-mark"><circle cx="%d" cy="%d" r="8"/><text x="%d" y="%d">!</text><title>%s</title></g>`, mx, e.ly, mx, e.ly+4, html.EscapeString(e.Problem))
		}
		b.WriteString(`</g>`)
	}
	b.WriteString(`</g><g class="nodes">`)
	nodeW, nodeH := g.nw, g.nh
	for _, n := range g.Nodes {
		cls := "node " + n.Status
		if n.Last {
			cls += " last"
		}
		if n.Risk != "" {
			cls += " " + riskCSS[n.Risk]
		}
		if n.Gate != "" && n.Risk != "" {
			cls += " gated"
		}
		if n.Cex {
			cls += " cex"
		}
		title := n.Name + ": " + statusWords(n.Status)
		if n.Title != "" {
			title = n.Title
		}
		sub := n.Kind + " · " + statusWords(n.Status)
		if n.Risk != "" {
			sub = n.Kind + " · " + n.Risk
		}
		if n.Crash != "" {
			if n.Gate != "" {
				title += "\napproval gate " + n.Gate
			}
		} else if n.Gate != "" {
			title += "\napproval gate " + n.Gate
			if n.GateStatus != "" {
				title += ": " + n.GateStatus
			}
		}
		label := n.Name
		if n.Iters > 1 {
			label += " ×" + strconv.Itoa(n.Iters)
		}
		fmt.Fprintf(&b, `<g class="%s" data-id="%s" tabindex="0" role="img" aria-label="%s" data-detail="%s">`,
			cls, html.EscapeString(n.Name), html.EscapeString(strings.ReplaceAll(title, "\n", ". ")), html.EscapeString(title))
		fmt.Fprintf(&b, `<rect class="box" x="%d" y="%d" width="%d" height="%d" rx="12"/>`, n.x, n.y, nodeW, nodeH)
		fmt.Fprintf(&b, `<circle class="disc" cx="%d" cy="%d" r="14"/><use class="nicon" href="#gi-%s" x="%d" y="%d" width="16" height="16"/>`,
			n.x+26, n.y+28, nodeIconName(n), n.x+18, n.y+20)
		fmt.Fprintf(&b, `<text class="nlabel" x="%d" y="%d">%s</text>`, n.x+50, n.y+24, html.EscapeString(clip(label, 21)))
		fmt.Fprintf(&b, `<text class="nkind" x="%d" y="%d">%s</text>`, n.x+50, n.y+43, html.EscapeString(clip(sub, 27)))
		line := func(y int, class, text string) {
			fmt.Fprintf(&b, `<text x="%d" y="%d" class="%s">%s</text>`, n.x+16, n.y+y, class, html.EscapeString(clip(text, 32)))
		}
		switch {
		case n.Crash != "":
			line(64, "ncrash", n.Crash)
			if n.GateLine != "" {
				line(82, "ngate "+n.GateClass, n.GateLine)
			}
		default:
			if n.GateLine != "" {
				line(64, "ngate "+n.GateClass, n.GateLine)
			}
			if n.SigLine != "" {
				line(82, "ngate "+n.GateClass, n.SigLine)
			}
		}
		if n.Status == "problem" || n.Status == "warning" {
			// The border says it in color; the mark and the tooltip say it in words.
			fmt.Fprintf(&b, `<g class="nmark %s"><circle cx="%d" cy="%d" r="10"/><text x="%d" y="%d">!</text></g>`, n.Status, n.x+4, n.y+2, n.x+4, n.y+7)
		}
		if n.Gate != "" {
			bx, by := n.x+nodeW-8, n.y
			fmt.Fprintf(&b, `<g class="gbadge %s"><rect x="%d" y="%d" width="%d" height="22" rx="11"/><use href="#gi-gate" x="%d" y="%d" width="12" height="12"/><text x="%d" y="%d">%s</text></g>`,
				n.GateClass, bx-badgeW/2, by-11, badgeW, bx-badgeW/2+8, by-6, bx-badgeW/2+24, by+4, html.EscapeString(gateBadge(n.Gate)))
		}
		if n.Note != "" {
			fmt.Fprintf(&b, `<text class="nnote" x="%d" y="%d">%s</text>`, n.x+nodeW/2, n.y+nodeH+16, html.EscapeString(n.Note))
		}
		fmt.Fprintf(&b, `<title>%s</title></g>`, html.EscapeString(title))
	}
	b.WriteString(`</g></svg>`)
	return b.String()
}
