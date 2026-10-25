package flowedit

import (
	"slices"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// TestAffectedListInChrome drives the list of unfinished runs in real Chrome: the panel is there for a store with no run
// (and the run panel is not), the button is reached and pressed by keyboard alone, the answer is a table with a
// caption and header cells that tells a refused group, a same-digest group and another flow's in words, focus
// is on the headline, an edit of the text says the list is out of date, and axe-core finds nothing in the three
// themes.
func TestAffectedListInChrome(t *testing.T) {
	v1, v2, other := flowCfg("pay", "dbl"), flowCfg("pay", "inc"), flowCfg("elsewhere", "dbl")
	tv1, tv2, tother := templateRun(t, v1), templateRun(t, v2), templateRun(t, other)
	st := agent.NewMemStore()
	for _, id := range []string{"same-a", "same-b"} {
		cloneRun(t, tv1, st, id, unfinished, nil)
	}
	cloneRun(t, tv2, st, "changed-a", unfinished, nil)
	cloneRun(t, tv2, st, "changed-b", unfinished, nil)
	cloneRun(t, tother, st, "foreign", unfinished, nil)
	cloneRun(t, tv1, st, "nodigest", func(n string) bool { return unfinished(n) || n == "flow:digest" }, nil)
	s, _ := storeServer(t, v1, st, true)
	type panelState struct {
		Hidden         bool     `json:"hidden"`
		Heading        string   `json:"heading"`
		RunPanelHidden bool     `json:"runPanelHidden"`
		Button         string   `json:"button"`
		AgainHidden    bool     `json:"againHidden"`
		Word           string   `json:"word"`
		Line           string   `json:"line"`
		Caption        string   `json:"caption"`
		ColHeaders     []string `json:"colHeaders"`
		Rows           []struct {
			Result string   `json:"result"`
			Scope  string   `json:"scope"`
			Cells  []string `json:"cells"`
		} `json:"rows"`
		Notes []string `json:"notes"`
		Stale string   `json:"stale"`
		Focus string   `json:"focus"`
		Live  string   `json:"live"`
	}
	var r struct {
		OK      bool       `json:"ok"`
		Error   string     `json:"error"`
		Before  panelState `json:"before"`
		Reached bool       `json:"reached"`
		Listed  panelState `json:"listed"`
		Stale   panelState `json:"stale"`
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
	runScript(t, "affected.mjs", s, &r)
	if !r.OK {
		t.Fatalf("the script failed: %s", r.Error)
	}
	b := r.Before
	if b.Hidden || b.Heading != "Unfinished runs this file would refuse" || !b.RunPanelHidden || b.Button != "Check unfinished runs" || !b.AgainHidden || b.Word != "" {
		t.Errorf("before the check: %+v", b)
	}
	l := r.Listed
	if !r.Reached || l.Word != "2 RUNS REFUSED" || l.Focus != "aff-word" || !strings.Contains(l.Line, "2 of 6 checked") || !strings.Contains(l.Live, "2 of 6 checked") {
		t.Errorf("listed: reached %v %+v", r.Reached, l)
	}
	if l.Caption == "" || !slices.Equal(l.ColHeaders, []string{"Result:col", "Runs:col", "Recorded digest:col", "Run IDs (the first):col"}) {
		t.Errorf("the table: %q %q", l.Caption, l.ColHeaders)
	}
	var words []string
	for _, row := range l.Rows {
		words = append(words, row.Result)
		if row.Scope != "row" {
			t.Errorf("a row header without scope=row: %+v", row)
		}
	}
	if !slices.Equal(words, []string{"REFUSED", "another flow", "same digest"}) {
		t.Errorf("results %q", words)
	}
	if len(l.Rows) == 3 && (!strings.Contains(l.Rows[0].Cells[2], "changed-a, changed-b") || !strings.Contains(l.Rows[0].Cells[2], "bide edit -run changed-a <store flags> <file>")) {
		t.Errorf("the refused row: %+v", l.Rows[0])
	}
	if !strings.Contains(strings.Join(l.Notes, "\n"), "1 unfinished run records no digest yet") {
		t.Errorf("notes %q", l.Notes)
	}
	if r.Stale.Stale != "The text has changed since this list was made: press the button to check it again." || r.Stale.Word != "2 RUNS REFUSED" || r.Stale.AgainHidden {
		t.Errorf("after an edit: %+v", r.Stale)
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
