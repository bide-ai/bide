package flowedit

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/bide-ai/bide/internal/runview"
)

type runPanel struct {
	Hidden   bool   `json:"hidden"`
	Heading  string `json:"heading"`
	Labelled string `json:"labelled"`
	Facts    string `json:"facts"`
	Word     string `json:"word"`
	Claim    string `json:"claim"`
	Icon     bool   `json:"icon"`
	Lists    []struct {
		Label string   `json:"label"`
		Items []string `json:"items"`
	} `json:"lists"`
	Read string `json:"read"`
}

// TestRunPanelInChrome drives the comparison with a run in real Chrome: the panel as opened says the
// digest is the same, a changed block makes it say it differs and list the change, Reload run is reached
// and pressed by keyboard alone and announced, no state is told by colour alone (a word and a glyph),
// and axe-core finds nothing in the panel's states in the three themes.
func TestRunPanelInChrome(t *testing.T) {
	j, digest := ranRun(t, runCfg)
	s := runServer(t, runCfg, runview.JournalSource{J: j}, "r1", true)
	var r struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Line  struct {
			Hidden         bool   `json:"hidden"`
			Word           string `json:"word"`
			Text           string `json:"text"`
			Link           string `json:"link"`
			AboveFlow      bool   `json:"aboveFlow"`
			PanelBelowFlow bool   `json:"panelBelowFlow"`
		} `json:"line"`
		AfterFailure struct {
			Word  string `json:"word"`
			Class string `json:"cls"`
			Claim string `json:"claim"`
		} `json:"afterFailure"`
		LinkReached    bool   `json:"linkReached"`
		FocusAfterLink string `json:"focusAfterLink"`
		Same           runPanel
		Differs        runPanel
		AfterReload    runPanel `json:"afterReload"`
		LiveEdit       string   `json:"liveAfterEdit"`
		LiveReload     string   `json:"liveAfterReload"`
		ReloadLabel    string   `json:"reloadLabel"`
		Results        map[string]struct {
			Violations []struct {
				ID     string   `json:"id"`
				Impact string   `json:"impact"`
				Help   string   `json:"help"`
				Nodes  []string `json:"nodes"`
			} `json:"violations"`
			Passes int `json:"passes"`
		} `json:"results"`
		Console   []string `json:"console"`
		Failed    []string `json:"failed"`
		OffOrigin []string `json:"offOrigin"`
	}
	runScript(t, "run.mjs", s, &r)
	if !r.OK {
		t.Fatalf("the script failed: %s", r.Error)
	}
	if l := r.Line; l.Hidden || l.Word != "SAME DIGEST" || l.Text != "Run r1" || l.Link != "Details of the comparison with the run" || !l.AboveFlow || !l.PanelBelowFlow {
		t.Errorf("the headline line: %+v", l)
	}
	if f := r.AfterFailure; f.Word != "NOT COMPARED" || !strings.Contains(f.Class, "v-warn") || !strings.Contains(f.Claim, "Could not compare with the run") {
		t.Errorf("after a failed comparison: %+v", f)
	}
	if !r.LinkReached || r.FocusAfterLink != "h-run" {
		t.Errorf("the details link: reached by Tab %v, focus after Enter on %q, want h-run", r.LinkReached, r.FocusAfterLink)
	}
	same := r.Same
	if same.Hidden || same.Heading != "Compared with a run" || same.Labelled != "h-run" || same.Word != "SAME DIGEST" || !same.Icon ||
		!strings.Contains(same.Claim, digest) || !strings.Contains(same.Facts, "Run r1 of the flow pay") || !strings.Contains(same.Read, "does not follow it") {
		t.Errorf("as opened: %+v", same)
	}
	d := r.Differs
	if d.Word != "DIFFERS FROM THE RUN" || !d.Icon || !strings.Contains(d.Claim, "will not resume under this file") {
		t.Errorf("after the edit: %+v", d)
	}
	if len(d.Lists) == 0 || !strings.HasPrefix(d.Lists[0].Label, "Changes from the run (1)") || !slices.ContainsFunc(d.Lists[0].Items, func(l string) bool { return strings.Contains(l, "changed node b") }) {
		t.Errorf("the changes: %+v", d.Lists)
	}
	if !strings.Contains(r.LiveEdit, "DIFFERS FROM THE RUN") || !strings.Contains(r.LiveReload, "Run read again") {
		t.Errorf("announcements: %q then %q", r.LiveEdit, r.LiveReload)
	}
	if r.ReloadLabel != "Reload run" || r.AfterReload.Word != "DIFFERS FROM THE RUN" {
		t.Errorf("reload: %q %+v", r.ReloadLabel, r.AfterReload)
	}
	if len(r.Results) != 6 {
		t.Errorf("%d states were checked, want 6 (2 states in 3 themes)", len(r.Results))
	}
	for state, res := range r.Results {
		if res.Passes < 20 {
			t.Errorf("%s: only %d rules passed: the page was not checked", state, res.Passes)
		}
		for _, v := range res.Violations {
			t.Errorf("%s: %s (%s): %s at %q", state, v.ID, v.Impact, v.Help, v.Nodes)
		}
	}
	for _, c := range r.Console {
		if !strings.HasPrefix(c, "log:") {
			t.Errorf("console: %s", c)
		}
	}
	if len(r.Failed) != 0 || len(r.OffOrigin) != 0 {
		t.Errorf("failed %q off origin %q", r.Failed, r.OffOrigin)
	}
}

// The comparison follows the editing session: a rename made in the page is matched to the run's node, and an
// answer to older text, arriving last, never replaces the answer to newer text.
func TestRunPanelFollowsTheSessionInChrome(t *testing.T) {
	j, _ := ranRun(t, runCfg)
	s := runServer(t, runCfg, runview.JournalSource{J: j}, "r1", true)
	var r struct {
		OK         bool   `json:"ok"`
		Error      string `json:"error"`
		First      string `json:"first"`
		Second     string `json:"second"`
		AfterStale string `json:"afterStale"`
		Lists      []struct {
			Label string   `json:"label"`
			Items []string `json:"items"`
		} `json:"lists"`
	}
	runScript(t, "run-session.mjs", s, &r)
	if !r.OK {
		t.Fatalf("the script failed: %s", r.Error)
	}
	if r.First != "SAME DIGEST" || r.Second != "SAME DIGEST" || r.AfterStale != "SAME DIGEST" {
		t.Errorf("race: %q, then %q, then %q after the older answer arrived", r.First, r.Second, r.AfterStale)
	}
	var renamed []string
	for _, l := range r.Lists {
		if strings.HasPrefix(l.Label, "Renames of this session") {
			renamed = l.Items
		}
		for _, it := range l.Items {
			if strings.Contains(it, "removed node") || strings.Contains(it, "added node") {
				t.Errorf("the rename read as %q", it)
			}
		}
	}
	if !slices.Equal(renamed, []string{"b -> c"}) {
		t.Errorf("renames listed: %q (lists %+v)", renamed, r.Lists)
	}
}

// TestRunPanelIsAbsentWithoutARunInChrome: without -run the panel stays hidden and the page asks once.
func TestRunPanelIsAbsentWithoutARunInChrome(t *testing.T) {
	s, _ := newTestServer(t, "refund.yaml", testFlow)
	var r struct {
		OK     bool   `json:"ok"`
		Error  string `json:"error"`
		Hidden bool   `json:"hidden"`
		Asked  int    `json:"asked"`
	}
	runScript(t, "run-off.mjs", s, &r)
	if !r.OK || !r.Hidden || r.Asked != 1 {
		t.Errorf("%+v", r)
	}
}

// TestRunPathInChrome draws the run's path on the canvas in real Chrome: a ring and words under each node the
// run reached, a dashed line over each connection taken, the words in the outline and in the node's name,
// the ring's dash pattern telling started from ran without color (and still drawn under forced colors), a
// retyped text drawing the same marks and not more, a node of another block losing its marks and being
// listed, and axe-core clean in the three themes.
func TestRunPathInChrome(t *testing.T) {
	type nodeMarks struct {
		Ring       string  `json:"ring"`
		Dash       string  `json:"dash"`
		Width      float64 `json:"width"`
		Stroke     string  `json:"stroke"`
		Words      string  `json:"words"`
		MarkHidden string  `json:"markHidden"`
		Title      string  `json:"title"`
	}
	type pageMarks struct {
		Nodes   map[string]nodeMarks `json:"nodes"`
		Taken   []string             `json:"taken"`
		Outline map[string]string    `json:"outline"`
		Lists   []struct {
			Label string   `json:"label"`
			Items []string `json:"items"`
		} `json:"lists"`
		Legend string `json:"legend"`
	}
	var r struct {
		OK      bool      `json:"ok"`
		Error   string    `json:"error"`
		Opened  pageMarks `json:"opened"`
		Redrawn pageMarks `json:"redrawn"`
		Pending struct {
			Rings int `json:"rings"`
			Marks int `json:"marks"`
			Taken int `json:"taken"`
		} `json:"whilePending"`
		Forced  pageMarks `json:"forced"`
		Again   pageMarks `json:"again"`
		Edited  pageMarks `json:"edited"`
		Results map[string]struct {
			Violations []struct {
				ID     string   `json:"id"`
				Impact string   `json:"impact"`
				Help   string   `json:"help"`
				Nodes  []string `json:"nodes"`
			} `json:"violations"`
			Passes int `json:"passes"`
		} `json:"results"`
		Console   []string `json:"console"`
		Failed    []string `json:"failed"`
		OffOrigin []string `json:"offOrigin"`
	}
	s := loopServer(t, loopRunCfg, runview.JournalSource{J: ranLoop(t, 1)}, true)
	runScript(t, "run-path.mjs", s, &r)
	if !r.OK {
		t.Fatalf("the script failed: %s", r.Error)
	}
	for _, name := range []string{"seed", "refine", "check", "done"} {
		n := r.Opened.Nodes[name]
		if !strings.Contains(n.Ring, "run-ran") || !strings.HasPrefix(n.Words, "ran") || n.MarkHidden != "true" || !strings.Contains(n.Title, ". The run: ran") || !strings.HasPrefix(r.Opened.Outline[name], "The run: ran") {
			t.Errorf("%s: %+v outline %q", name, n, r.Opened.Outline[name])
		}
		if n.Stroke == "" || n.Stroke == "none" || n.Width < 2 {
			t.Errorf("%s: the ring is not drawn: %+v", name, n)
		}
		if f := r.Forced.Nodes[name]; f.Stroke == "" || f.Stroke == "none" || f.Width < 2 || !strings.HasPrefix(f.Words, "ran") {
			t.Errorf("%s under forced colors: %+v", name, f)
		}
	}
	if w := r.Opened.Nodes["refine"].Words; !strings.HasSuffix(w, " times") {
		t.Errorf("refine ran more than once: %q", w)
	}
	if len(r.Redrawn.Nodes) != 4 || strings.Count(r.Redrawn.Nodes["refine"].Ring, "rring") != 1 || len(r.Redrawn.Taken) != 4 {
		t.Errorf("a redraw lost or stacked the marks: %+v", r.Redrawn)
	}
	if r.Pending.Rings != 0 || r.Pending.Marks != 0 || r.Pending.Taken != 0 {
		t.Errorf("the marks of the old text stayed while the answer was awaited: %+v", r.Pending)
	}
	slices.Sort(r.Opened.Taken)
	if !slices.Equal(r.Opened.Taken, []string{"check>done", "check>refine", "refine>check", "seed>refine"}) {
		t.Errorf("connections taken: %q", r.Opened.Taken)
	}
	if !strings.Contains(r.Opened.Legend, "dashed ring for started") {
		t.Errorf("legend %q", r.Opened.Legend)
	}
	if !slices.Equal(slices.Sorted(maps.Keys(r.Again.Nodes)), slices.Sorted(maps.Keys(r.Opened.Nodes))) || len(r.Again.Taken) != len(r.Opened.Taken) {
		t.Errorf("retyped: %+v", r.Again)
	}
	for name, n := range r.Again.Nodes {
		if strings.Count(n.Ring, "rring") != 1 {
			t.Errorf("%s: rings stacked: %q", name, n.Ring)
		}
	}
	if n := r.Edited.Nodes["refine"]; n.Ring != "" || n.Words != "" || strings.Contains(n.Title, "The run") || r.Edited.Outline["refine"] != "" {
		t.Errorf("refine kept its marks: %+v", n)
	}
	if n := r.Edited.Nodes["seed"]; !strings.Contains(n.Ring, "run-ran") {
		t.Errorf("seed lost its mark: %+v", n)
	}
	listed := false
	for _, l := range r.Edited.Lists {
		for _, it := range l.Items {
			if strings.HasPrefix(it, "refine: the run ran") && strings.Contains(it, "with block refine; this file's refine has block check, so it is another node.") {
				listed = true
			}
		}
	}
	if !listed {
		t.Errorf("the edited node is not listed: %+v", r.Edited.Lists)
	}
	if len(r.Results) != 6 {
		t.Errorf("%d states were checked, want 6 (2 states in 3 themes)", len(r.Results))
	}
	for state, res := range r.Results {
		if res.Passes < 20 {
			t.Errorf("%s: only %d rules passed", state, res.Passes)
		}
		for _, v := range res.Violations {
			t.Errorf("%s: %s (%s): %s at %q", state, v.ID, v.Impact, v.Help, v.Nodes)
		}
	}
	for _, c := range r.Console {
		if !strings.HasPrefix(c, "log:") {
			t.Errorf("console: %s", c)
		}
	}
	if len(r.Failed) != 0 || len(r.OffOrigin) != 0 {
		t.Errorf("failed %q off origin %q", r.Failed, r.OffOrigin)
	}
}
