package flowedit

import (
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
		OK          bool   `json:"ok"`
		Error       string `json:"error"`
		Same        runPanel
		Differs     runPanel
		AfterReload runPanel `json:"afterReload"`
		LiveEdit    string   `json:"liveAfterEdit"`
		LiveReload  string   `json:"liveAfterReload"`
		ReloadLabel string   `json:"reloadLabel"`
		Results     map[string]struct {
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
