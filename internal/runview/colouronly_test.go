package runview

import (
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
	for _, f := range []string{"style.css", "graph.css", "diff.css", "extras.css", "timeline.css", "live.css"} {
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

// TestColourOnlyPairs is the audit of docs/design/themes.md (A1 to A5, A7 to A9) as a test: states
// that were told apart by colour alone differ in a shape property as well, so they still differ in
// greyscale, in forced colours and for a reader who cannot tell the hues apart.
func TestColourOnlyPairs(t *testing.T) {
	for _, tc := range []struct {
		id, a, b string
		props    []string
	}{
		{"A1 added and changed edges", ".edge.added", ".edge.changed", []string{"stroke-dasharray"}},
		{"A2 the path taken and a counterexample", ".edge.taken", ".edge.cex", []string{"stroke-dasharray", "stroke-width", "stroke-linecap"}},
		{"A3 a problem and a removed edge", ".edge.problem", ".edge.removed", []string{"stroke-dasharray"}},
		{"A1 and A3 changed and removed edges", ".edge.changed", ".edge.removed", []string{"stroke-dasharray"}},
		{"A5 a problem node and a warning node", ".node.problem .box", ".node.warning .box", []string{"stroke-dasharray"}},
	} {
		a, b := cssProps(t, tc.a), cssProps(t, tc.b)
		differ := false
		for _, p := range tc.props {
			if a[p] != b[p] {
				differ = true
			}
		}
		if !differ {
			t.Errorf("%s: %s and %s have the same %v (%v and %v): they differ by colour alone", tc.id, tc.a, tc.b, tc.props, pick(a, tc.props), pick(b, tc.props))
		}
	}
	// The pairs on one page differ from each other, not only in pairs: the diff page's three edge
	// states and the config page's two path states.
	seen := map[string]string{}
	for _, sel := range []string{".edge.added", ".edge.changed", ".edge.removed"} {
		d := cssProps(t, sel)["stroke-dasharray"]
		if other, dup := seen[d]; dup {
			t.Errorf("%s and %s share the dash %q", sel, other, d)
		}
		seen[d] = sel
	}
}

func pick(m map[string]string, keys []string) []string {
	var out []string
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
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
