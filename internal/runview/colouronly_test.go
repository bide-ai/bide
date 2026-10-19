package runview

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"strings"
	"testing"
)

// cssProps returns the declarations of every rule whose selector list holds exactly sel, merged in
// file order, across the viewer's stylesheets: what the stylesheets say about sel.
func cssProps(t *testing.T, sel string) map[string]string {
	t.Helper()
	comment := regexp.MustCompile(`(?s)/\*.*?\*/`)
	rule := regexp.MustCompile(`([^{}]+)\{([^{}]*)\}`)
	props := map[string]string{}
	for _, f := range []string{"style.css", "graph.css", "diff.css", "extras.css", "timeline.css", "live.css", "runs.css", "attention.css"} {
		b, err := os.ReadFile("assets/" + f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range rule.FindAllStringSubmatch(comment.ReplaceAllString(string(b), ""), -1) {
			for _, s := range strings.Split(m[1], ",") {
				if strings.Join(strings.Fields(s), " ") != sel {
					continue
				}
				for _, d := range strings.Split(m[2], ";") {
					if k, v, ok := strings.Cut(d, ":"); ok {
						props[strings.TrimSpace(k)] = strings.TrimSpace(v)
					}
				}
			}
		}
	}
	if len(props) == 0 {
		t.Fatalf("no rule for %q", sel)
	}
	return props
}

// line is how a stroke is drawn apart from its colour: its dash pattern, width and cap.
type line struct {
	dash  string // a CSS stroke-dasharray, computed ("13px, 3px, 2px, 3px") or written ("13 3 2 3"), or "none"
	width float64
}

// dashPattern reads a stroke-dasharray into its lengths, repeated once if odd as CSS does. A solid
// line is nil.
func dashPattern(v string) []float64 {
	v = strings.TrimSpace(strings.ToLower(v))
	if v == "" || v == "none" {
		return nil
	}
	var out []float64
	for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r == ' ' || r == ',' }) {
		var x float64
		if _, err := fmt.Sscanf(strings.TrimSuffix(f, "px"), "%g", &x); err != nil {
			return nil
		}
		out = append(out, x)
	}
	if len(out)%2 == 1 {
		out = append(out, out...)
	}
	return out
}

// distinct reports whether two strokes differ in a way the eye sees without color: one solid and
// the other dashed, a different number of segments, a period or a share of dash that differs by 30%
// or by 0.2, or a width ratio of 1.5 or more. A dash of 6 4 and one of 6 5, or a width of 3 and 3.5,
// are the same line to the eye.
func distinct(a, b line) bool {
	pa, pb := dashPattern(a.dash), dashPattern(b.dash)
	if (pa == nil) != (pb == nil) {
		return true
	}
	if a.width > 0 && b.width > 0 && max(a.width, b.width)/min(a.width, b.width) >= 1.5 {
		return true
	}
	if pa == nil {
		return false
	}
	if len(pa) != len(pb) {
		return true
	}
	stats := func(p []float64) (period, duty float64) {
		var on float64
		for i, x := range p {
			period += x
			if i%2 == 0 {
				on += x
			}
		}
		return period, on / period
	}
	ta, da := stats(pa)
	tb, db := stats(pb)
	return math.Abs(ta-tb)/max(ta, tb) >= 0.3 || math.Abs(da-db) >= 0.2
}

func TestDistinct(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b line
		want bool
	}{
		{"the old taken and counterexample edges", line{"10 6", 3}, line{"10 6", 3.5}, false},
		{"the old problem and removed edges", line{"6 4", 2.5}, line{"6 5", 2.2}, false},
		{"the old added and changed edges", line{"none", 2.8}, line{"none", 2.8}, false},
		{"the old problem and warning boxes", line{"none", 2.8}, line{"none", 2.2}, false},
		{"whitespace and units do not make a difference", line{"10  6", 3}, line{"10px, 6px", 3}, false},
		{"an odd pattern repeats", line{"5", 2}, line{"5 5", 2}, false},
		{"solid and dashed", line{"none", 2.8}, line{"13 3 2 3", 2.8}, true},
		{"two and four segments", line{"13 3 2 3", 2.8}, line{"9 5", 2.2}, true},
		{"a short dash and a long one", line{"3 4", 2.5}, line{"9 5", 2.2}, true},
		{"the same period, another duty", line{"2 8", 2}, line{"8 2", 2}, true},
		{"a heavy line and a thin one of one pattern", line{"1 5", 1.6}, line{"1 7", 4.5}, true},
		{"two solid lines of one width", line{"none", 2}, line{"", 2.2}, false},
		{"solid lines of 2x width", line{"none", 1.4}, line{"none", 2.8}, true},
	} {
		if got := distinct(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: distinct(%v, %v) = %v, want %v", tc.name, tc.a, tc.b, got, tc.want)
		}
	}
}

// edgeLine is what the stylesheets say about an edge state's stroke: the base .edge rule with the
// state's own rule over it. The browser test is the authority; this is a fast guard that cannot see
// the cascade (a later, more specific rule) the way Chrome does.
func edgeLine(t *testing.T, state string) line {
	t.Helper()
	p := cssProps(t, ".edge")
	if state != "" {
		for k, v := range cssProps(t, ".edge."+state) {
			p[k] = v
		}
	}
	var w float64
	fmt.Sscanf(p["stroke-width"], "%g", &w)
	return line{p["stroke-dasharray"], w}
}

// TestColourOnlyPairs is the audit of docs/design/themes.md (A1 to A5) as a fast test: the edge
// states that appear together on a page are pairwise distinct strokes (distinct above), so they
// still differ in greyscale, in forced colors and for a reader who cannot tell the hues apart.
func TestColourOnlyPairs(t *testing.T) {
	for page, states := range map[string][]string{
		"the config page":   {"", "cex", "problem"},
		"the diff page":     {"", "added", "changed", "removed"},
		"the run page":      {"", "taken"},
		"the editor's page": {"", "cex", "problem"},
	} {
		for i, a := range states {
			for _, b := range states[i+1:] {
				if distinct(edgeLine(t, a), edgeLine(t, b)) {
					continue
				}
				t.Errorf("%s: the %q and %q edges are one line to the eye: %v and %v", page, a, b, edgeLine(t, a), edgeLine(t, b))
			}
		}
	}
	// The audit's pairs, whichever page each is drawn on.
	for _, p := range [][2]string{{"added", "changed"}, {"taken", "cex"}, {"problem", "removed"}, {"changed", "removed"}} {
		if a, b := edgeLine(t, p[0]), edgeLine(t, p[1]); !distinct(a, b) {
			t.Errorf("the %q and %q edges are one line to the eye: %v and %v", p[0], p[1], a, b)
		}
	}
	// A5: a problem node's box and a warning node's.
	box := func(sel string) line {
		p := cssProps(t, sel)
		var w float64
		fmt.Sscanf(p["stroke-width"], "%g", &w)
		return line{p["stroke-dasharray"], w}
	}
	if a, b := box(".node.problem .box"), box(".node.warning .box"); !distinct(a, b) {
		t.Errorf("A5: the problem and warning boxes are one line to the eye: %v and %v", a, b)
	}
}

// TestNodeMarkShapes (A4): a problem is a circle and a warning a diamond, so the marks differ
// without color; both carry the same "!" and the box says which in words in its tooltip.
func TestNodeMarkShapes(t *testing.T) {
	out := svg(&Graph{Nodes: []GraphNode{{Name: "p", Status: "problem"}, {Name: "w", Status: "warning"}}})
	problem := regexp.MustCompile(`<g class="nmark problem"><circle [^>]*/><text [^>]*>!</text></g>`)
	warning := regexp.MustCompile(`<g class="nmark warning"><path d="M[^"]+z"/><text [^>]*>!</text></g>`)
	if !problem.MatchString(out) {
		t.Errorf("a problem node's mark is not a circle with a bang: %s", out)
	}
	if !warning.MatchString(out) {
		t.Errorf("a warning node's mark is not a diamond with a bang: %s", out)
	}
	if strings.Contains(out, `class="nmark warning"><circle`) || strings.Contains(out, `class="nmark problem"><path`) {
		t.Error("a mark has the other kind's shape")
	}
}

// TestColourOnlyWords (A7 to A9): a new row says "new", the keyboard's row is marked by a glyph and
// aria-current, and a stat's colour is backed by a glyph that differs per judgement.
func TestColourOnlyWords(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile("assets/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	live := read("live.js")
	if !strings.Contains(live, `classList.add("fresh")`) || !strings.Contains(live, `className = "badge newbadge"`) || !strings.Contains(live, `textContent = "new"`) {
		t.Error("A7: live.js tints a new row without the word new")
	}
	if !strings.Contains(read("live.css"), ".newbadge") {
		t.Error("A7: no style for the new badge")
	}
	view := read("view.js")
	if !strings.Contains(view, `setAttribute("aria-current", "true")`) || !strings.Contains(view, `removeAttribute("aria-current")`) {
		t.Error("A8: view.js does not mark the current row with aria-current")
	}
	if c := cssProps(t, "table.runlist tbody tr.cur td:first-child::before")["content"]; !strings.Contains(c, `\25B6`) {
		t.Errorf("A8: the current run-list row has no glyph (content %q)", c)
	}
	glyphs := map[string]string{}
	for _, s := range []string{".s-good .lbl::before", ".s-bad .lbl::before", ".s-warn .lbl::before"} {
		c := cssProps(t, s)["content"]
		if c == "" {
			t.Errorf("A9: no glyph for %s", s)
		}
		if other, dup := glyphs[c]; dup {
			t.Errorf("A9: %s and %s have the same glyph %s", s, other, c)
		}
		glyphs[c] = s
	}
}

// auditStatus says where each row of the colour-only audit in docs/design/themes.md is handled.
// Every row must be here, and every entry must be a row: a new colour-only use is added to the
// audit table first, and the table's row then has to be given a status, so one cannot be added
// silently.
var auditStatus = map[string]string{
	"A1":  "tested: TestColourOnlyPairs (added and changed edges)",
	"A2":  "tested: TestColourOnlyPairs (the path taken and a counterexample)",
	"A3":  "tested: TestColourOnlyPairs (a problem and a removed edge)",
	"A4":  "tested: TestNodeMarkShapes (problem circle, warning diamond)",
	"A5":  "tested: TestColourOnlyPairs (problem and warning node boxes)",
	"A6":  "contrast, not state: the border_strong token and its use on every control (PR 4 for the viewer's CSS, PR 5 for the approve page), with a contrast test there",
	"A7":  "tested: TestColourOnlyWords (the new badge)",
	"A8":  "tested: TestColourOnlyWords (the run list's marker and aria-current)",
	"A9":  "tested: TestColourOnlyWords (stat glyphs)",
	"A10": "allowed: redundant colour, the badge text carries it",
	"A11": "allowed: redundant colour, the result sentence has words",
	"A12": "PR 5: the approve page's disabled button",
	"A13": "allowed: syntax colour is decoration, the text is the content",
	"A14": "allowed: the tab icon holds the verdict's glyph",
}

// TestAuditIsComplete: the audit table in the design note and auditStatus name the same rows.
func TestAuditIsComplete(t *testing.T) {
	b, err := os.ReadFile("../../docs/design/themes.md")
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile(`(?m)^\| (A\d+) \|`)
	inDoc := map[string]bool{}
	for _, m := range row.FindAllStringSubmatch(string(b), -1) {
		inDoc[m[1]] = true
	}
	if len(inDoc) == 0 {
		t.Fatal("no audit rows found in docs/design/themes.md")
	}
	for id := range inDoc {
		if auditStatus[id] == "" {
			t.Errorf("audit row %s has no status in auditStatus: say where it is tested or why it is allowed", id)
		}
	}
	for id := range auditStatus {
		if !inDoc[id] {
			t.Errorf("auditStatus has %s, which is not a row of the audit table", id)
		}
	}
}

// TestStateColoursUseInfo: what a page says with the info colour (waiting for a person, the path
// taken, the last node, live) is drawn with --info, not with --accent, which is what a link, the
// focus ring and the selected row use. The two have one value in every shipped theme; a custom theme
// may set them apart.
func TestStateColoursUseInfo(t *testing.T) {
	for _, sel := range []string{
		".edge.taken", ".arrowhead.taken", ".node.waiting .box", ".node.waiting .disc", ".node.last .box", ".node.last",
		".lg.waiting::before", ".lg.lastk::before", ".lg.taken::before", ".v-wait", ".pill.live", ".ev.fresh", ".newbadge", ".ev.fresh .seq",
		".chip-waiting .count", "table.runlist .pill.state-waiting", ".kind-approval",
	} {
		joined := ""
		for k, v := range cssProps(t, sel) {
			joined += k + ":" + v + ";"
		}
		if strings.Contains(joined, "var(--accent") || !strings.Contains(joined, "var(--info") {
			t.Errorf("%s is a state drawn with the info color but reads %s", sel, joined)
		}
	}
	// Links, the focus ring and the selected row stay on accent.
	for _, sel := range []string{"a", "table.runlist tbody tr.cur"} {
		joined := ""
		for k, v := range cssProps(t, sel) {
			joined += k + ":" + v + ";"
		}
		if !strings.Contains(joined, "var(--accent") {
			t.Errorf("%s is interactive and should read --accent: %s", sel, joined)
		}
	}
}
