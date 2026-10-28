package flowedit

import (
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

type taintPage struct {
	OK              bool                `json:"ok"`
	Error           string              `json:"error"`
	OpenedTags      []string            `json:"openedTags"`
	OpenedRings     []string            `json:"openedRings"`
	OpenedLine      string              `json:"openedLine"`
	OpenedHidden    bool                `json:"openedPanelHidden"`
	OpenedSkip      bool                `json:"openedSkipHidden"`
	OpenedBanner    string              `json:"openedBanner"`
	BannerWord      string              `json:"bannerWord"`
	BannerRole      string              `json:"bannerRole"`
	BannerDetail    []string            `json:"bannerDetail"`
	Live            string              `json:"live"`
	Line            string              `json:"line"`
	Counts          []string            `json:"counts"`
	ExposedButtons  []string            `json:"exposedButtons"`
	UnknownButtons  []string            `json:"unknownButtons"`
	SourceButtons   []string            `json:"sourceButtons"`
	Caveat          string              `json:"caveat"`
	Tags            []string            `json:"tags"`
	Rings           map[string][]string `json:"rings"`
	Titles          map[string]string   `json:"titles"`
	Outline         map[string]string   `json:"outline"`
	TagsHidden      bool                `json:"tagsHidden"`
	TaintwDisplay   string              `json:"taintwDisplay"`
	SelectedByPanel string              `json:"selectedByPanel"`
	InspVerdict     string              `json:"inspectorVerdict"`
	InspPath        string              `json:"inspectorPath"`
	InspSection     string              `json:"inspectorSection"`
	LiveAfterSelect string              `json:"liveAfterSelect"`
	SelectedByKey   string              `json:"selectedByKeyboard"`
	FetchSection    string              `json:"fetchSection"`
	NotifySection   string              `json:"notifySection"`
	SkipFocus       string              `json:"skipFocus"`
	TabOrder        []string            `json:"tabOrder"`
	Patterns        map[string]struct {
		Dash   string `json:"dash"`
		Width  string `json:"width"`
		Stroke string `json:"stroke"`
	} `json:"patterns"`
	Forced struct {
		RingStroke string `json:"ringStroke"`
		RingWidth  string `json:"ringWidth"`
		TagFill    string `json:"tagFill"`
		CanvasText string `json:"canvasText"`
	} `json:"forced"`
	Themes map[string]struct {
		Tag   float64 `json:"tag"`
		Ring  float64 `json:"ring"`
		Words float64 `json:"words"`
	} `json:"themes"`
	AfterBanner string   `json:"afterBanner"`
	AfterLive   string   `json:"afterLive"`
	Console     []string `json:"console"`
	Failed      []string `json:"failed"`
	OffOrigin   []string `json:"offOrigin"`
}

// opened is the flow with fetch last: nothing before it reads its source.
func openedTaintFlow() string {
	return strings.NewReplacer("entry: fetch", "entry: triage", "  - edge: [fetch, triage]\n", "", "  - edge: [refund, notify]\n", "  - edge: [refund, notify]\n  - edge: [notify, fetch]\n").Replace(taintFlow)
}

// TestTaintViewInChrome drives the taint view in real Chrome: an edit makes the side-effecting refund read
// the source, with no taint policy to cover it, and the page says so in words on the canvas, in the outline, in
// the inspector, in the panel, in the banner and in the live region.
func TestTaintViewInChrome(t *testing.T) {
	opened := openedTaintFlow()
	s := taintServer(t, opened, WithRegistry(taintRegistry(t)), WithTaintPolicy(agent.TaintPolicy{}))
	t.Setenv("EXPOSED", taintFlow)
	t.Setenv("OPENED", opened)
	var r taintPage
	runScript(t, "taint.mjs", s, &r)
	if !r.OK {
		t.Fatalf("the script failed: %s\nconsole: %q", r.Error, r.Console)
	}
	// As opened: only fetch has anything to say, and the panel says no more than that.
	if strings.Join(r.OpenedTags, "|") != "source: inbound-email" || strings.Join(r.OpenedRings, "|") != "tring tr-source" {
		t.Errorf("as opened: tags %q rings %q", r.OpenedTags, r.OpenedRings)
	}
	if r.OpenedLine != "taint: no node has a declared source before it" || r.OpenedHidden || r.OpenedSkip || r.OpenedBanner != "" {
		t.Errorf("as opened: line %q hidden %v skip hidden %v banner %q", r.OpenedLine, r.OpenedHidden, r.OpenedSkip, r.OpenedBanner)
	}
	// The edit: an alert in words, and the live region.
	if r.BannerWord != "UNTRUSTED EXPOSURE WIDENED" || r.BannerRole != "alert" || len(r.BannerDetail) != 1 || r.BannerDetail[0] != "refund: now reads inbound-email and no taint policy: nothing gates its side effect" {
		t.Errorf("banner %q role %q detail %q", r.BannerWord, r.BannerRole, r.BannerDetail)
	}
	if r.Live != "Taint changed: 3 nodes may read untrusted data" {
		t.Errorf("announced %q", r.Live)
	}
	// The panel: counts, the buttons, and the cannot-know note.
	if r.Line != "taint: 3 nodes may read untrusted data" || strings.Join(r.Counts, "|") != "3 reads untrusted" || strings.Join(r.ExposedButtons, ",") != "refund" ||
		len(r.UnknownButtons) != 0 || strings.Join(r.SourceButtons, ",") != "fetch" || !strings.Contains(r.Caveat, "input untrusted") {
		t.Errorf("panel: %q %q exposed %q unknown %q sources %q caveat %q", r.Line, r.Counts, r.ExposedButtons, r.UnknownButtons, r.SourceButtons, r.Caveat)
	}
	// The canvas: words under the nodes, a ring on each, the words in the node's name for assistive technology.
	if strings.Join(r.Tags, "|") != "source: inbound-email|reads untrusted: inbound-email|reads untrusted: inbound-email|reads untrusted: inbound-email" || !r.TagsHidden {
		t.Errorf("tags %q hidden from assistive technology: %v", r.Tags, r.TagsHidden)
	}
	for node, want := range map[string]string{"fetch": "tring tr-source", "triage": "tring tr-reads", "refund": "tring tr-reads", "notify": "tring tr-reads"} {
		if strings.Join(r.Rings[node], "|") != want {
			t.Errorf("ring of %s: %q, want %q", node, r.Rings[node], want)
		}
	}
	if !strings.Contains(r.Titles["refund"], "Taint: reads untrusted, reads inbound-email via fetch > triage > refund; no taint policy: nothing gates its side effect") || !strings.Contains(r.Titles["fetch"], "Taint: no declared source before it; source: inbound-email") {
		t.Errorf("titles: %q", r.Titles)
	}
	if r.TaintwDisplay != "block" {
		t.Errorf("the outline's taint words are %q, want a line of their own", r.TaintwDisplay)
	}
	// The outline's buttons carry the same words.
	if !strings.HasSuffix(r.Outline["refund"], "Taint: reads untrusted, reads inbound-email via fetch > triage > refund; no taint policy: nothing gates its side effect") || !strings.Contains(r.Outline["triage"], "Taint: reads untrusted, reads inbound-email via fetch > triage") {
		t.Errorf("outline: %q", r.Outline)
	}
	// The inspector, reached from the panel by mouse and by keyboard.
	if r.SelectedByPanel != "refund" || r.InspVerdict != "reads untrusted" || r.InspPath != "fetch > triage > refund" || !strings.Contains(r.InspSection, "cannot know no taint policy: nothing gates its side effect") || !strings.Contains(r.InspSection, "may-view") {
		t.Errorf("inspector: %q %q %q %q", r.SelectedByPanel, r.InspVerdict, r.InspPath, r.InspSection)
	}
	if r.LiveAfterSelect != "Selected refund" || r.SelectedByKey != "fetch" || !strings.Contains(r.FetchSection, "declares source inbound-email") {
		t.Errorf("selection: live %q by key %q section %q", r.LiveAfterSelect, r.SelectedByKey, r.FetchSection)
	}
	if !strings.Contains(r.NotifySection, "verdict reads untrusted") || !strings.Contains(r.NotifySection, "path fetch > triage > refund > notify") {
		t.Errorf("notify section %q", r.NotifySection)
	}
	if r.SkipFocus != "h-taint" || len(r.TabOrder) < 2 || r.TabOrder[0] != "refund" || r.TabOrder[1] != "fetch" {
		t.Errorf("skip link focus %q, tab order %q", r.SkipFocus, r.TabOrder)
	}
	// Five ring patterns, all different: the states differ without color.
	seen := map[string]string{}
	for class, p := range r.Patterns {
		key := p.Dash + "/" + p.Width
		if other, dup := seen[key]; dup {
			t.Errorf("rings %s and %s are drawn alike (%s)", class, other, key)
		}
		seen[key] = class
	}
	if len(r.Patterns) != 5 {
		t.Errorf("patterns %v", r.Patterns)
	}
	// Forced colors: the system color for the ring and the words.
	if r.Forced.RingStroke != r.Forced.CanvasText || r.Forced.TagFill != r.Forced.CanvasText || r.Forced.RingWidth == "0px" {
		t.Errorf("forced colors: %+v", r.Forced)
	}
	// Contrast in each theme: text 4.5, the ring 3.
	for theme, m := range r.Themes {
		if m.Tag < 4.5 || m.Words < 4.5 || m.Ring < 3 {
			t.Errorf("theme %s: tag %.2f words %.2f ring %.2f", theme, m.Tag, m.Words, m.Ring)
		}
	}
	if len(r.Themes) != 4 {
		t.Errorf("themes measured: %v", r.Themes)
	}
	// Back to the baseline: the banner goes and the page says the taint changed.
	if r.AfterBanner != "" || r.AfterLive != "Taint changed: no node has a declared source before it" {
		t.Errorf("after: banner %q live %q", r.AfterBanner, r.AfterLive)
	}
	if len(r.Console) != 0 || len(r.Failed) != 0 || len(r.OffOrigin) != 0 {
		t.Errorf("console %q failed %q off origin %q", r.Console, r.Failed, r.OffOrigin)
	}
}

// The taint view's states under axe-core, in light, dark and high contrast.
func TestTaintAccessibilityInChrome(t *testing.T) {
	opened := openedTaintFlow()
	s := taintServer(t, opened, WithRegistry(taintRegistry(t)), WithTaintPolicy(agent.TaintPolicy{}))
	t.Setenv("EXPOSED", taintFlow)
	var r struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		Results map[string]struct {
			Violations []struct {
				ID     string   `json:"id"`
				Impact string   `json:"impact"`
				Help   string   `json:"help"`
				Nodes  []string `json:"nodes"`
			} `json:"violations"`
			Passes int `json:"passes"`
		} `json:"results"`
	}
	runScript(t, "taint-a11y.mjs", s, &r)
	if !r.OK {
		t.Fatalf("the script failed: %s", r.Error)
	}
	if len(r.Results) != 9 {
		t.Errorf("%d states were checked, want 9 (3 states in 3 themes)", len(r.Results))
	}
	for state, res := range r.Results {
		if res.Passes < 20 {
			t.Errorf("%s: only %d rules passed: the page was not checked", state, res.Passes)
		}
		for _, v := range res.Violations {
			t.Errorf("%s: %s (%s): %s at %q", state, v.ID, v.Impact, v.Help, v.Nodes)
		}
	}
}
