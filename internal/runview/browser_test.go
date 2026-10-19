package runview

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent/agenttest"
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

type edgeOut struct {
	Classes []string `json:"classes"`
	Dash    string   `json:"dash"`
	Width   float64  `json:"width"`
	Cap     string   `json:"cap"`
}

type nodeOut struct {
	Classes []string `json:"classes"`
	Dash    string   `json:"dash"`
	Width   float64  `json:"width"`
	Mark    *struct {
		Shape string `json:"shape"`
		Text  string `json:"text"`
	} `json:"mark"`
}

type pageOut struct {
	Edges []edgeOut `json:"edges"`
	Nodes []nodeOut `json:"nodes"`
	// run page
	Stats map[string]string `json:"stats"`
	Fresh []struct {
		Index        int     `json:"index"`
		Fresh        bool    `json:"fresh"`
		Badge        *string `json:"badge"`
		BadgeVisible bool    `json:"badgeVisible"`
	} `json:"fresh"`
	// run list
	Cur         bool   `json:"cur"`
	AriaCurrent string `json:"ariaCurrent"`
	Glyph       string `json:"glyph"`
	Others      int    `json:"others"`
}

type chromeOut struct {
	OK      bool               `json:"ok"`
	Error   string             `json:"error"`
	Pages   map[string]pageOut `json:"pages"`
	Console []string           `json:"console"`
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
	serve("diff", "/", Options{Diff: &Diff{Old: Config{Name: "old.json", Data: []byte(oldC)}, New: Config{Name: "new.json", Data: []byte(newC)}}})
	src := Options{Source: JournalSource{J: j}}
	serve("run", "/run?id=f1", src)
	serve("runs", "/", src)
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

// TestColourOnlyInChrome is the control for the colour-only audit (docs/design/themes.md, A1 to
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

	edges := func(page string) map[string]line {
		m := map[string]line{}
		for _, e := range out.Pages[page].Edges {
			m[kindOf(e.Classes)] = line{e.Dash, e.Width}
		}
		return m
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
	for _, n := range out.Pages["config"].Nodes {
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
	pm, wm := nodes["problem"].Mark, nodes["warning"].Mark
	if pm == nil || wm == nil || pm.Shape == wm.Shape || pm.Text == "" || wm.Text == "" {
		t.Errorf("A4: the problem and warning marks are %+v and %+v: they must be different shapes, each with a glyph", pm, wm)
	}

	// A9: the stat glyphs.
	seen := map[string]string{}
	for c, g := range out.Pages["run"].Stats {
		if g == "" || g == "none" || g == "normal" {
			t.Errorf("A9: a %s stat's label has no glyph (computed content %q)", c, g)
		}
		if other, dup := seen[g]; dup {
			t.Errorf("A9: the %s and %s stats share the glyph %s", c, other, g)
		}
		seen[g] = c
	}
	if len(out.Pages["run"].Stats) != 3 {
		t.Errorf("A9: %d stat judgements measured, want 3", len(out.Pages["run"].Stats))
	}

	// A7: rows the live script marks new say so; the others do not.
	fresh := out.Pages["run"].Fresh
	if len(fresh) < 2 {
		t.Fatalf("A7: the run page has %d timeline rows, want at least 2", len(fresh))
	}
	for _, r := range fresh {
		switch {
		case r.Index == 0 && (r.Fresh || r.Badge != nil):
			t.Errorf("A7: the first row, not new, is marked: %+v", r)
		case r.Index > 0 && (!r.Fresh || r.Badge == nil || *r.Badge != "new" || !r.BadgeVisible):
			t.Errorf("A7: row %d is new and does not say so, visibly: %+v", r.Index, r)
		}
	}

	// A8: the run list's keyboard row.
	runs := out.Pages["runs"]
	if !runs.Cur || runs.AriaCurrent != "true" || !strings.Contains(runs.Glyph, "▶") || runs.Others != 1 {
		t.Errorf("A8: after j the run list's row is %+v: want one row, aria-current true, with the glyph", runs)
	}
}

func mapKeys[V any](m map[string]V) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}
