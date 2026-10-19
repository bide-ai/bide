package runview

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/plan"
)

// The viewer's pages in real Chrome. Run only where BIDE_VIEW_BROWSER=1 (the Viewer (Chrome) job
// sets it, after installing the driver from the lockfile); with CI also set, a missing Chrome or
// driver then fails the test instead of skipping it. Locally: npm ci --ignore-scripts in
// internal/runview/browser, CHROME_BIN set, BIDE_VIEW_BROWSER=1.

func viewTools(t *testing.T) (node, dir, chrome string) {
	t.Helper()
	if os.Getenv("BIDE_VIEW_BROWSER") != "1" {
		t.Skip("set BIDE_VIEW_BROWSER=1 to drive the viewer's pages in Chrome (the Viewer (Chrome) job does)")
	}
	fail := func(format string, args ...any) {
		t.Helper()
		if os.Getenv("CI") != "" {
			t.Fatalf(format, args...)
		}
		t.Skipf(format, args...)
	}
	chrome = os.Getenv("CHROME_BIN")
	if chrome == "" {
		chrome, _ = exec.LookPath("google-chrome-stable")
	}
	if chrome == "" {
		fail("Chrome is not installed (set CHROME_BIN, or install google-chrome-stable)")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		fail("node is not installed")
	}
	dir, err = filepath.Abs("browser")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "puppeteer-core")); err != nil {
		fail("puppeteer-core is not installed in %s (npm ci --ignore-scripts)", dir)
	}
	return node, dir, chrome
}

// shapeVis and textVis are the visibility records the script takes of each measured element: what
// the browser painted, not what the stylesheet says.
type shapeVis struct {
	Shown       bool       `json:"shown"`
	Opacity     float64    `json:"opacity"`
	StrokeAlpha float64    `json:"strokeAlpha"`
	StrokeWidth float64    `json:"strokeWidth"`
	FillAlpha   float64    `json:"fillAlpha"`
	W           float64    `json:"w"`
	H           float64    `json:"h"`
	Stroke      [3]float64 `json:"stroke"`
	Fill        [3]float64 `json:"fill"`
	BG          [3]float64 `json:"bg"`
}

type textVis struct {
	Shown    bool       `json:"shown"`
	Opacity  float64    `json:"opacity"`
	FontSize float64    `json:"fontSize"`
	Alpha    float64    `json:"alpha"`
	Content  string     `json:"content"`
	Color    [3]float64 `json:"color"`
	BG       [3]float64 `json:"bg"`
}

// strokeShown: an edge or a box's outline is drawn: shown, opaque enough, wide enough, with extent.
func (v shapeVis) strokeShown() bool {
	return v.Shown && v.Opacity >= 0.5 && v.StrokeAlpha >= 0.5 && v.StrokeWidth >= 1 && v.W+v.H >= 4 && contrastOf(v.Stroke, v.BG) >= 3
}

// shapeShown: a filled mark is drawn.
func (v shapeVis) shapeShown() bool {
	return v.Shown && v.Opacity >= 0.5 && (v.FillAlpha >= 0.5 && contrastOf(v.Fill, v.BG) >= 3 || v.StrokeAlpha >= 0.5 && contrastOf(v.Stroke, v.BG) >= 3) && v.W >= 4 && v.H >= 4
}

// glyphShown: text or a pseudo-element's glyph is drawn: shown, opaque, big enough, not transparent.
func (v textVis) glyphShown() bool {
	return v.Shown && v.Opacity >= 0.5 && v.FontSize >= 8 && v.Alpha >= 0.5 && contrastOf(v.Color, v.BG) >= 3
}

// textShown is glyphShown for text drawn on a shape the page's background does not describe (the
// "!" on a mark's circle): it is checked for alpha and size, not for contrast with the page.
func (v textVis) textShown() bool {
	return v.Shown && v.Opacity >= 0.5 && v.FontSize >= 8 && v.Alpha >= 0.5
}

// contrastOf is the WCAG contrast ratio of two sRGB colours (0 to 255 a channel).
func contrastOf(a, b [3]float64) float64 {
	lum := func(c [3]float64) float64 {
		var l [3]float64
		for i, v := range c {
			v /= 255
			if v <= 0.04045 {
				l[i] = v / 12.92
			} else {
				l[i] = math.Pow((v+0.055)/1.055, 2.4)
			}
		}
		return 0.2126*l[0] + 0.7152*l[1] + 0.0722*l[2]
	}
	la, lb := lum(a), lum(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// inkMin is how many pixels (of a box a few hundred wide) must change, in greyscale, when an element
// is hidden for it to count as painted.
const inkMin = 6

type edgeOut struct {
	Classes []string `json:"classes"`
	Dash    string   `json:"dash"`
	Width   float64  `json:"width"`
	Cap     string   `json:"cap"`
	Vis     shapeVis `json:"vis"`
	Len     float64  `json:"len"`
	Paints  *int     `json:"paints"` // measured for the first edge of each kind
}

type nodeOut struct {
	Classes        []string `json:"classes"`
	Dash           string   `json:"dash"`
	Width          float64  `json:"width"`
	BoxVis         shapeVis `json:"boxVis"`
	BoxPaints      *int     `json:"boxPaints"`
	MarkPaints     *int     `json:"markPaints"`
	MarkTextPaints *int     `json:"markTextPaints"`
	Mark           *struct {
		Inside   bool     `json:"inside"` // the mark's centre lies on the node it describes
		Shape    string   `json:"shape"`
		Text     string   `json:"text"`
		ShapeVis shapeVis `json:"shapeVis"`
		TextVis  textVis  `json:"textVis"`
	} `json:"mark"`
}

type statOut struct {
	Classes []string `json:"classes"`
	Label   string   `json:"label"`
	Glyph   *textVis `json:"glyph"`
	Paints  *int     `json:"paints"` // pixels the label's ::before paints, for a judged stat
}

type controlOut struct {
	Tag         string     `json:"tag"`
	ID          string     `json:"id"`
	Class       string     `json:"cls"`
	BorderWidth float64    `json:"borderWidth"`
	BorderStyle string     `json:"borderStyle"`
	Border      [3]float64 `json:"border"`
	BorderAlpha float64    `json:"borderAlpha"`
	Fill        [3]float64 `json:"fill"`
	FillAlpha   float64    `json:"fillAlpha"`
	ParentBG    [3]float64 `json:"parentBG"`
	ParentClass string     `json:"parentClass"`
}

// edgeShown: a control can be told from what it sits on: its border (a line of a pixel or more, in
// a style that draws, opaque enough) or its fill reaches 3:1 against the surface under it
// (WCAG 1.4.11).
func (c controlOut) edgeShown() bool {
	return c.BorderWidth >= 1 && c.BorderStyle != "none" && c.BorderStyle != "hidden" && c.BorderAlpha >= 0.5 && contrastOf(c.Border, c.ParentBG) >= 3 ||
		c.FillAlpha >= 0.5 && contrastOf(c.Fill, c.ParentBG) >= 3
}

type pageOut struct {
	AttemptedBox *struct {
		Dash  string  `json:"dash"`
		Width float64 `json:"width"`
	} `json:"attemptedBox"`
	Controls []controlOut `json:"controls"`
	Body     struct {
		BG      string `json:"bg"`
		FG      string `json:"fg"`
		BGToken string `json:"bgToken"`
	} `json:"body"`
	Edges []edgeOut `json:"edges"`
	Nodes []nodeOut `json:"nodes"`
	Stats []statOut `json:"stats"`
	// run page
	Fresh []struct {
		Index int      `json:"index"`
		Fresh bool     `json:"fresh"`
		Badge *string  `json:"badge"`
		Vis   *textVis `json:"vis"`
	} `json:"fresh"`
	BadgePaints int `json:"badgePaints"`
	// run list
	Cur         bool    `json:"cur"`
	AriaCurrent string  `json:"ariaCurrent"`
	Glyph       textVis `json:"glyph"`
	Paints      int     `json:"paints"`
	Others      int     `json:"others"`
}

type chromeOut struct {
	OK     bool                          `json:"ok"`
	Error  string                        `json:"error"`
	Themes map[string]map[string]pageOut `json:"themes"`
	Combos map[string]struct {
		BG      string `json:"bg"`
		FG      string `json:"fg"`
		BGToken string `json:"bgToken"`
		Current string `json:"current"`
	} `json:"combos"`
	Console []string `json:"console"`
}

// everyStateFixtures serves a page for each state the audit names: a config with a counterexample,
// a problem edge, a problem node and a warning node; a diff with an added, a changed and a removed
// edge; a run that took a path; and the run list.
func everyStateFixtures(t *testing.T) map[string]string {
	t.Helper()
	nodes := `, {"name": "audit", "block": "audit", "safety": "readonly"}, {"name": "router", "block": "router", "safety": "readonly"}, {"name": "extra", "block": "extra", "safety": "readonly"}, {"name": "bad", "block": "bad", "safety": "sideways"}`
	config := gapCfg(nodes, `, {"edge": ["validate", "audit"]}, {"edge": ["validate", "bad"]}, {"edge": ["validate", "ghost"]}, {"edge": ["validate", "router"]}, {"switch": "router", "when": [{"pred": "p", "to": "receipt"}]}`, "every path to a terminal passes charge")
	oldC := gapCfg(nodes, `, {"edge": ["validate", "audit"]}, {"edge": ["validate", "router"]}, {"switch": "router", "else": "charge", "when": [{"pred": "p", "to": "receipt"}]}`)
	newC := gapCfg(nodes, `, {"edge": ["validate", "extra"]}, {"edge": ["validate", "router"]}, {"switch": "router", "else": "charge", "when": [{"pred": "q", "to": "receipt"}]}`)
	charges := 0
	f := refundFlow(t, &charges)
	j := agenttest.MemJournal()
	if _, err := f.Run(context.Background(), j, "f1", order{ID: "o-1", Amount: 250}); err != nil {
		t.Fatal(err)
	}
	urls := map[string]string{}
	serve := func(name, path string, o Options) {
		ts := httptest.NewServer(Handler(o))
		t.Cleanup(ts.Close)
		urls[name] = ts.URL + path
	}
	serve("config", "/", Options{Config: &Config{Name: "c.json", Data: []byte(config)}})
	serve("configHeld", "/", Options{Config: &Config{Name: "h.json", Data: []byte(gapCfg("", "", "every path to a terminal passes charge"))}})
	serve("diff", "/", Options{Diff: &Diff{Old: Config{Name: "old.json", Data: []byte(oldC)}, New: Config{Name: "new.json", Data: []byte(newC)}}})
	src := Options{Source: JournalSource{J: j}}
	serve("run", "/run?id=f1", src)
	serve("runs", "/", src)
	// A run paused at an approval gate (waiting) and one that crashed mid-step (halted).
	policy := agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}}
	gb := plan.New[int, string]("refunds").WithApproverVerifiers(approverKeys(nil))
	check := gb.Step("check", func(_ context.Context, n int) (int, error) { return n, nil }, plan.ReadOnly())
	pay := gb.Step("refund", func(_ context.Context, n int) (string, error) { return "refunded", nil }, plan.Approval(&policy))
	gb.Edge(check, pay)
	gf, err := gb.Build()
	if err != nil {
		t.Fatal(err)
	}
	gj := agenttest.MemJournal()
	if _, err := gf.Run(context.Background(), gj, "g1", 120); err == nil {
		t.Fatal("the gated run did not pause")
	}
	serve("runWaiting", "/run?id=g1", Options{Source: JournalSource{J: gj}})
	serve("runDefault", "/run?id=f1", Options{Source: JournalSource{J: j}, DefaultTheme: "dark"})
	cs := &crashStore{Store: agent.NewMemStore(), lose: "node:charge", armed: true}
	hf := refundFlow(t, &charges)
	if _, err := hf.Run(context.Background(), agenttest.MustJournal(cs), "h1", order{ID: "o-1", Amount: 250}); !errors.Is(err, errCrash) {
		t.Fatalf("the crashing run = %v", err)
	}
	cs.armed = false
	hj := agenttest.MustJournal(cs)
	_, _ = hf.Run(context.Background(), hj, "h1", order{ID: "o-1", Amount: 250})
	serve("runHalted", "/run?id=h1", Options{Source: JournalSource{J: hj}, Flows: map[string]*Flow{"refund": {Topology: hf.Topology(), Digest: hf.Digest(), Conform: hf.Conform}}})
	return urls
}

func runColourScript(t *testing.T, urls map[string]string) chromeOut {
	t.Helper()
	node, dir, chrome := viewTools(t)
	pages, _ := json.Marshal(urls)
	cmd := exec.Command(node, "colouronly.mjs")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CHROME_BIN="+chrome, "PAGES="+string(pages))
	raw, err := cmd.Output()
	var out chromeOut
	if jerr := json.Unmarshal(raw, &out); jerr != nil {
		t.Fatalf("the script printed no result (%v): %q", err, raw)
	}
	if !out.OK {
		t.Fatalf("the script failed: %s", out.Error)
	}
	return out
}

// kindOf names an edge or node by the state classes it carries, in order.
func kindOf(classes []string, drop ...string) string {
	var keep []string
	for _, c := range classes {
		if !slices.Contains(drop, c) {
			keep = append(keep, c)
		}
	}
	return strings.Join(keep, " ")
}

// TestColourOnlyInChrome (under each shipped theme) is the control for the colour-only audit (docs/design/themes.md, A1 to
// A5, A7 to A9): the states are drawn by the browser from the real pages, and what it computed is
// compared. Cascade, specificity and whitespace are Chrome's, so a later rule that puts two states
// back to one line fails here though the stylesheet text still looks fine. Each page's states are
// pairwise distinct strokes, and every state the audit names must appear, so the test cannot pass
// for want of one.
func TestColourOnlyInChrome(t *testing.T) {
	out := runColourScript(t, everyStateFixtures(t))
	for _, c := range out.Console {
		t.Errorf("console: %s", c)
	}
	checkCombos(t, out)
	for _, theme := range []string{"light", "dark", "high-contrast", "prefers-contrast", "forced-colors"} {
		pages, ok := out.Themes[theme]
		if !ok {
			t.Errorf("no measurements under the %s theme", theme)
			continue
		}
		t.Run(theme, func(t *testing.T) {
			checkColourOnly(t, pages)
			checkMode(t, theme, pages)
		})
	}
}

// checkColourOnly judges one theme's measurements.
func checkColourOnly(t *testing.T, pages map[string]pageOut) {
	t.Helper()

	edges := func(page string) map[string]line {
		m := map[string]line{}
		for _, e := range pages[page].Edges {
			m[kindOf(e.Classes)] = line{e.Dash, e.Width}
		}
		return m
	}
	// Every edge state is drawn: shown, opaque, wide, with extent, and it paints pixels (in
	// greyscale) that are not there when it is hidden.
	for page, p := range pages {
		for _, e := range p.Edges {
			if !e.Vis.strokeShown() {
				t.Errorf("%s page: the %q edge is not drawn (%+v): a state that is hidden is no state", page, kindOf(e.Classes), e.Vis)
			}
			if e.Paints != nil && *e.Paints < inkMin {
				t.Errorf("%s page: the %q edge changes %d greyscale pixels when hidden, want at least %d", page, kindOf(e.Classes), *e.Paints, inkMin)
			}
			if !dashDrawn(e.Dash) {
				t.Errorf("%s page: the %q edge's dash %q draws almost nothing: a dash needs 1px or more and a tenth of the line", page, kindOf(e.Classes), e.Dash)
			}
		}
	}
	for page, want := range map[string][]string{
		"config": {"edge", "edge cex", "edge problem"},
		"diff":   {"edge", "edge added", "edge changed", "edge removed"},
		"run":    {"edge", "edge taken"},
	} {
		got := edges(page)
		var kinds []string
		for _, k := range want {
			if _, ok := got[k]; !ok {
				t.Errorf("%s page: no %q edge drawn (drew %v): the test would pass for want of it", page, k, mapKeys(got))
				continue
			}
			kinds = append(kinds, k)
		}
		for i, a := range kinds {
			for _, b := range kinds[i+1:] {
				if !distinct(got[a], got[b]) {
					t.Errorf("%s page: %q and %q edges are one line to the eye (computed %v and %v)", page, a, b, got[a], got[b])
				}
			}
		}
	}

	// A1 to A3: the audit's pairs, whichever page each is drawn on (the path taken is the run
	// page's, a counterexample the config page's, a removed edge the diff page's).
	byKind := map[string]line{}
	for _, page := range []string{"config", "diff", "run"} {
		for k, l := range edges(page) {
			if _, dup := byKind[k]; !dup {
				byKind[k] = l
			}
		}
	}
	for _, p := range [][2]string{{"edge added", "edge changed"}, {"edge taken", "edge cex"}, {"edge problem", "edge removed"}, {"edge changed", "edge removed"}} {
		a, aok := byKind[p[0]]
		b, bok := byKind[p[1]]
		if !aok || !bok {
			t.Errorf("A1 to A3: no %q or %q edge drawn on any page", p[0], p[1])
		} else if !distinct(a, b) {
			t.Errorf("A1 to A3: %q and %q edges are one line to the eye (computed %v and %v)", p[0], p[1], a, b)
		}
	}

	// A4, A5: the node states of the config page.
	nodes := map[string]nodeOut{}
	for _, n := range pages["config"].Nodes {
		for _, k := range []string{"problem", "warning", "undeclared"} {
			// A problem node on the counterexample path is drawn heavier for that: the plain one is the
			// state to tell from a warning.
			if slices.Contains(n.Classes, k) && !slices.Contains(n.Classes, "cex") {
				nodes[k] = n
			}
		}
	}
	for _, k := range []string{"problem", "warning", "undeclared"} {
		if _, ok := nodes[k]; !ok {
			t.Fatalf("config page: no %s node drawn", k)
		}
	}
	for _, p := range [][2]string{{"problem", "warning"}, {"problem", "undeclared"}, {"warning", "undeclared"}} {
		a, b := nodes[p[0]], nodes[p[1]]
		if !distinct(line{a.Dash, a.Width}, line{b.Dash, b.Width}) {
			t.Errorf("A5: the %s and %s boxes are one line to the eye (computed %v and %v)", p[0], p[1], line{a.Dash, a.Width}, line{b.Dash, b.Width})
		}
	}
	for _, n := range pages["config"].Nodes {
		if !n.BoxVis.strokeShown() {
			t.Errorf("a %v node's box is not drawn (%+v)", n.Classes, n.BoxVis)
		}
		if n.BoxPaints != nil && *n.BoxPaints < inkMin {
			t.Errorf("a %v node's box changes %d greyscale pixels when hidden, want at least %d", n.Classes, *n.BoxPaints, inkMin)
		}
		if !dashDrawn(n.Dash) {
			t.Errorf("a %v node's box dash %q draws almost nothing", n.Classes, n.Dash)
		}
		if n.MarkTextPaints != nil && *n.MarkTextPaints < 3 {
			t.Errorf("a %v node's mark \"!\" changes %d greyscale pixels when hidden: it is the shape's own colour or gone", n.Classes, *n.MarkTextPaints)
		}
		if n.MarkPaints != nil && *n.MarkPaints < inkMin {
			t.Errorf("a %v node's mark changes %d greyscale pixels when hidden, want at least %d", n.Classes, *n.MarkPaints, inkMin)
		}
		if m := n.Mark; m != nil && !m.Inside {
			t.Errorf("a %v node's mark is not on the node it describes", n.Classes)
		}
		if m := n.Mark; m != nil && (!m.ShapeVis.shapeShown() || !m.TextVis.textShown()) {
			t.Errorf("a %v node's mark is not drawn (shape %+v, text %+v)", n.Classes, m.ShapeVis, m.TextVis)
		}
	}
	pm, wm := nodes["problem"].Mark, nodes["warning"].Mark
	if pm == nil || wm == nil || pm.Shape == wm.Shape || pm.Text == "" || wm.Text == "" {
		t.Errorf("A4: the problem and warning marks are %+v and %+v: they must be different shapes, each with a glyph", pm, wm)
	}

	// A9: the stat glyphs, from the real pages' own stats: a config with an ungated side effect and a
	// violated assertion (warn, bad) and one whose assertions hold (good).
	glyphs := map[string]string{}
	judged := 0
	for _, page := range []string{"config", "configHeld"} {
		for _, st := range pages[page].Stats {
			for _, c := range []string{"s-good", "s-bad", "s-warn"} {
				if !slices.Contains(st.Classes, c) {
					continue
				}
				judged++
				if st.Paints == nil || *st.Paints < 3 {
					t.Errorf("A9: a %s stat's glyph paints %v greyscale pixels when its ::before is hidden: it is blank or off screen", c, st.Paints)
				}
				if st.Label == "" || st.Glyph == nil || !st.Glyph.glyphShown() {
					t.Errorf("A9: a %s stat (%q) has no drawn glyph on its label (%+v)", c, st.Label, st.Glyph)
					continue
				}
				if other, dup := glyphs[st.Glyph.Content]; dup && other != c {
					t.Errorf("A9: the %s and %s stats share the glyph %s", c, other, st.Glyph.Content)
				}
				glyphs[st.Glyph.Content] = c
			}
		}
	}
	for _, c := range []string{"s-good", "s-bad", "s-warn"} {
		if !slices.Contains(mapValues(glyphs), c) {
			t.Errorf("A9: no real page stat with class %s was measured (measured %d judged stats)", c, judged)
		}
	}

	// A7: rows the live script marks new say so, visibly and in pixels; the others do not.
	fresh := pages["run"].Fresh
	if len(fresh) < 2 {
		t.Fatalf("A7: the run page has %d timeline rows, want at least 2", len(fresh))
	}
	for _, r := range fresh {
		switch {
		case r.Index == 0 && (r.Fresh || r.Badge != nil):
			t.Errorf("A7: the first row, not new, is marked: %+v", r)
		case r.Index > 0 && (!r.Fresh || r.Badge == nil || *r.Badge != "new" || r.Vis == nil || !r.Vis.glyphShown()):
			t.Errorf("A7: row %d is new and does not say so, visibly: %+v", r.Index, r)
		}
	}
	if pages["run"].BadgePaints < inkMin {
		t.Errorf("A7: the new badge changes %d greyscale pixels when hidden, want at least %d", pages["run"].BadgePaints, inkMin)
	}

	// A8: the run list's keyboard row.
	runs := pages["runs"]
	if !runs.Cur || runs.AriaCurrent != "true" || !strings.Contains(runs.Glyph.Content, "\u25b6") || !runs.Glyph.glyphShown() || runs.Others != 1 || runs.Paints < 3 {
		t.Errorf("A8: after j the run list's row is %+v: want one row, aria-current true, with the glyph drawn", runs)
	}
}

func mapValues[V any](m map[string]V) []V {
	var vs []V
	for _, v := range m {
		vs = append(vs, v)
	}
	return vs
}

func mapKeys[V any](m map[string]V) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

// dashDrawn: a dash pattern draws something the eye sees: each dash at least a pixel long and the
// dashes a tenth of the line. A solid line is drawn.
func dashDrawn(v string) bool {
	p := dashPattern(v)
	if p == nil {
		return true
	}
	var period, on float64
	for i, x := range p {
		period += x
		if i%2 == 0 {
			on += x
			if x < 1 {
				return false
			}
		}
	}
	return on/period >= 0.1
}

// checkMode judges what is particular to one mode: that a reader's request for more contrast gets the
// high-contrast theme's colours with no theme chosen in the page, that the system's forced colors
// replace the page's, and that the run states, one color under forced colors, differ in shape.
func checkMode(t *testing.T, mode string, pages map[string]pageOut) {
	t.Helper()
	// A6: every control's edge is 3:1 against what it sits on, on the pages that have controls (the
	// run list's filters, the run page's header buttons and copy buttons, the graph's tools).
	measured := 0
	for _, page := range []string{"runs", "run", "config", "diff"} {
		for _, c := range pages[page].Controls {
			measured++
			if !c.edgeShown() {
				t.Errorf("A6: the %s page's %s#%s.%s has no 3:1 edge (border %vpx %s %v on %v, fill %v alpha %v)", page, c.Tag, c.ID, c.Class, c.BorderWidth, c.BorderStyle, c.Border, c.ParentBG, c.Fill, c.FillAlpha)
			}
		}
	}
	// The controls the fix is about are among those measured, so a rule that moves one back to the
	// quiet border cannot hide in an unmeasured corner.
	seen := map[string]bool{}
	for _, page := range []string{"runs", "run", "config", "diff"} {
		for _, c := range pages[page].Controls {
			for _, k := range []string{"hbtn", "chip", "copy"} {
				if strings.Contains(c.Class, k) {
					seen[k] = true
				}
			}
			if c.Tag == "input" || c.Tag == "select" {
				seen["field"] = true
			}
			if c.Tag == "button" && c.ParentClass == "gtools" {
				seen["gtools"] = true
			}
		}
	}
	for _, k := range []string{"hbtn", "chip", "copy", "field", "gtools"} {
		if !seen[k] {
			t.Errorf("A6: no %s control was measured", k)
		}
	}
	if measured < 6 {
		t.Errorf("A6: only %d controls were measured; the pages' controls moved or are hidden", measured)
	}
	body := pages["run"].Body
	// The root's --bg custom property: a value Chrome does not force, so it shows which block won.
	wantToken := map[string]string{"light": "#f6f6f2", "dark": "#121214", "high-contrast": "#ffffff", "prefers-contrast": "#ffffff", "forced-colors": "Canvas"}[mode]
	if body.BGToken != wantToken {
		t.Errorf("%s: the root's --bg is %q, want %q (the wrong theme block won)", mode, body.BGToken, wantToken)
	}
	switch mode {
	case "light", "dark", "high-contrast", "prefers-contrast":
		// light and high-contrast both have a white ground; their text differs: #1a1a17 and black.
		wantFG := map[string]string{"light": "rgb(26, 26, 23)", "dark": "rgb(236, 236, 231)", "high-contrast": "rgb(0, 0, 0)", "prefers-contrast": "rgb(0, 0, 0)"}[mode]
		if body.FG != wantFG {
			t.Errorf("%s: the page's text is %s, want %s", mode, body.FG, wantFG)
		}
	case "forced-colors":
		// The theme chosen in the page was dark; forced colors win: the ground is the system's Canvas
		// (white in the emulation), not dark's #121214.
		if body.BG == "rgb(18, 18, 20)" || body.FG == "rgb(236, 236, 231)" {
			t.Errorf("forced colors: the page is still drawn in the dark theme (%s on %s)", body.FG, body.BG)
		}
	}
	// The run states, by the shape of the node's box: one page each has a done node and a node not
	// reached (the completed run), a waiting one (the gated run) and a halted one (the crashed run).
	if mode != "forced-colors" {
		return
	}
	states := map[string]line{}
	for _, page := range []string{"run", "runWaiting", "runHalted"} {
		for _, n := range pages[page].Nodes {
			for _, k := range []string{"done", "attempted", "waiting", "halted", "not-reached"} {
				if slices.Contains(n.Classes, k) {
					states[k] = line{n.Dash, n.Width}
				}
			}
		}
	}
	// An attempted step needs a journal with a retried attempt in progress, which no fixture here
	// builds; the script draws one on the run page by giving a node the state's class, and the
	// browser computes it with the same stylesheet.
	if a := pages["run"].AttemptedBox; a != nil {
		states["attempted"] = line{a.Dash, a.Width}
	}
	for _, k := range []string{"done", "attempted", "waiting", "halted", "not-reached"} {
		if _, ok := states[k]; !ok {
			t.Errorf("forced colors: no %s node drawn on the run pages", k)
		}
	}
	var kinds []string
	for k := range states {
		kinds = append(kinds, k)
	}
	slices.Sort(kinds)
	for i, a := range kinds {
		for _, b := range kinds[i+1:] {
			if !distinct(states[a], states[b]) {
				t.Errorf("forced colors: the %s and %s boxes are one line to the eye (computed %v and %v)", a, b, states[a], states[b])
			}
		}
	}
}

// checkCombos: a request beside a choice. The reader's own theme beats a request for more contrast;
// a request beats a host's default; the default alone is in force.
func checkCombos(t *testing.T, out chromeOut) {
	t.Helper()
	for key, want := range map[string]struct{ token, fg, current string }{
		"prefers-contrast+stored":  {"#121214", "rgb(236, 236, 231)", "dark"},
		"prefers-contrast+default": {"#ffffff", "rgb(0, 0, 0)", "high-contrast"},
		"default":                  {"#121214", "rgb(236, 236, 231)", "dark"},
	} {
		got, ok := out.Combos[key]
		if !ok {
			t.Errorf("no measurement for %s", key)
			continue
		}
		if got.BGToken != want.token || got.FG != want.fg || got.Current != want.current {
			t.Errorf("%s: --bg %q, text %s, current theme %q; want %q, %s, %q", key, got.BGToken, got.FG, got.Current, want.token, want.fg, want.current)
		}
	}
}
