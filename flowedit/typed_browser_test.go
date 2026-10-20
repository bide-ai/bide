package flowedit

import (
	"strings"
	"testing"

	"github.com/bide-ai/bide/internal/runview"
)

type typedPage struct {
	OK             bool     `json:"ok"`
	Error          string   `json:"error"`
	Original       string   `json:"original"`
	Tag            string   `json:"tag"`
	TagHidden      bool     `json:"tagHidden"`
	Mode           string   `json:"mode"`
	DigestSummary  string   `json:"digestSummary"`
	DigestFull     string   `json:"digestFull"`
	Palette        []string `json:"palette"`
	Count          string   `json:"count"`
	FirstLabel     string   `json:"firstLabel"`
	SkipLinkShown  bool     `json:"skipLinkShown"`
	TabStops       int      `json:"tabStops"`
	TabOrder       []string `json:"tabOrder"`
	AfterArrow     string   `json:"afterArrow"`
	CurrentRows    []string `json:"currentRows"`
	SearchRows     []string `json:"searchRows"`
	FitOffDefault  bool     `json:"fitOffByDefault"`
	InspectorTypes []string `json:"inspectorTypes"`
	AllRows        int      `json:"allRows"`
	FitRows        []string `json:"fitRows"`
	FitCount       string   `json:"fitCount"`
	AddFormBlock   string   `json:"addFormBlock"`
	AddFormName    string   `json:"addFormName"`
	AddFocus       string   `json:"addFocus"`
	AddNothing     bool     `json:"addNothingWritten"`
	WithCount      string   `json:"withCount"`
	EdgeOptions    []string `json:"edgeOptions"`
	EdgeValues     []string `json:"edgeValues"`
	FormRefusal    string   `json:"formRefusal"`
	FormLive       string   `json:"formLive"`
	FormUnchanged  bool     `json:"formUnchanged"`
	DragRefusal    string   `json:"dragRefusal"`
	DragLive       string   `json:"dragLive"`
	DragUnchanged  bool     `json:"dragUnchanged"`
	UseLabel       string   `json:"useLabel"`
	UseRefusal     string   `json:"useRefusal"`
	AfterUse       string   `json:"afterUse"`
	UseFocus       string   `json:"useFocus"`
	UseLive        string   `json:"useLive"`
	Console        []string `json:"console"`
	Failed         []string `json:"failed"`
	OffOrigin      []string `json:"offOrigin"`
}

// TestTypedModeInChrome drives the typed editor in real Chrome, once beside the program's Registry
// and once from a description file: the source label, the digest, the palette (a search, the filter that
// fits the selected node, one Tab stop, the two actions), the hint on a target whose types differ, and the
// server's refusal of a connection that does not fit, by the form and by drag, in the same words.
func TestTypedModeInChrome(t *testing.T) {
	for kind, wantTag := range map[string]string{"registry": "typed: program's Registry", "description": "typed: per description file"} {
		t.Run(kind, func(t *testing.T) {
			s := typedServers(t, "refund.yaml", testFlow)[kind]
			var r typedPage
			runScript(t, "typed.mjs", s, &r)
			if !r.OK {
				t.Fatalf("the script failed: %s\nconsole: %q", r.Error, r.Console)
			}
			if r.Tag != wantTag || r.TagHidden {
				t.Errorf("tag %q hidden %v, want %q", r.Tag, r.TagHidden, wantTag)
			}
			if kind == "description" && !strings.Contains(r.Mode, "not checked against the program") || kind == "registry" && !strings.Contains(r.Mode, "typed check against the program's plan.Registry") {
				t.Errorf("mode line %q", r.Mode)
			}
			if !strings.HasPrefix(r.DigestSummary, "Digest ") || len(r.DigestFull) != 64 || !strings.Contains(r.DigestSummary, r.DigestFull[:12]) {
				t.Errorf("digest: %q / %q", r.DigestSummary, r.DigestFull)
			}
			if got := strings.Join(r.Palette, ","); got != "charge,count,gated,plain,receipt,shout,validate" || r.Count != "7 of 7 blocks" || !r.SkipLinkShown {
				t.Errorf("palette %q, count %q, skip link shown %v", got, r.Count, r.SkipLinkShown)
			}
			if !strings.HasPrefix(r.FirstLabel, "charge, step, takes ") || !strings.Contains(r.FirstLabel, ", gives ") || !strings.Contains(r.FirstLabel, "safety side effect, no approval gate, used") {
				t.Errorf("first row's name for a screen reader: %q", r.FirstLabel)
			}
			if r.TabStops != 1 || len(r.TabOrder) < 3 || r.TabOrder[0] != "pal-fit" || r.TabOrder[1] != "charge" || r.TabOrder[2] != "pal-add" {
				t.Errorf("the list is not one Tab stop: stops %d, order %q", r.TabStops, r.TabOrder)
			}
			if r.AfterArrow != "count" || len(r.CurrentRows) != 1 || r.CurrentRows[0] != "count" {
				t.Errorf("arrow key: focus %q, current %q", r.AfterArrow, r.CurrentRows)
			}
			if strings.Join(r.SearchRows, ",") != "gated" {
				t.Errorf("search: %q", r.SearchRows)
			}
			if !r.FitOffDefault || r.AllRows != 7 || strings.Join(r.FitRows, ",") != "charge,gated,plain,receipt,validate" || r.FitCount != "5 of 7 blocks that fit validate" {
				t.Errorf("fit filter: off by default %v, all %d, fitting %q, %q", r.FitOffDefault, r.AllRows, r.FitRows, r.FitCount)
			}
			if len(r.InspectorTypes) != 2 || !strings.HasSuffix(r.InspectorTypes[0], "tOrder") || !strings.HasSuffix(r.InspectorTypes[1], "tOrder") {
				t.Errorf("inspector types %q", r.InspectorTypes)
			}
			if r.AddFormBlock != "count" || r.AddFormName != "count" || r.AddFocus != "an-name" || !r.AddNothing || !strings.Contains(r.WithCount, "name: count,") {
				t.Errorf("add with this block: block %q name %q focus %q, nothing written %v\n%s", r.AddFormBlock, r.AddFormName, r.AddFocus, r.AddNothing, r.WithCount)
			}
			if strings.Join(r.EdgeOptions, "|") != "charge|receipt|count (types differ)" || strings.Join(r.EdgeValues, "|") != "charge|receipt|count" {
				t.Errorf("target hints: %q values %q", r.EdgeOptions, r.EdgeValues)
			}
			for what, got := range map[string]string{"form": r.FormRefusal, "drag": r.DragRefusal} {
				if !strings.HasPrefix(got, "Not applied: the types do not fit: ") || !strings.Contains(got, "connects") {
					t.Errorf("the refusal by %s: %q", what, got)
				}
			}
			if r.FormRefusal != r.DragRefusal || r.FormLive != r.DragLive || !r.FormUnchanged || !r.DragUnchanged {
				t.Errorf("form and drag differ or wrote: %q vs %q, live %q vs %q, unchanged %v %v", r.FormRefusal, r.DragRefusal, r.FormLive, r.DragLive, r.FormUnchanged, r.DragUnchanged)
			}
			if r.UseLabel != "Use as the block of receipt" || !strings.HasPrefix(r.UseRefusal, "Not applied: the types do not fit: ") ||
				!strings.Contains(r.AfterUse, "name: receipt, block: plain") || r.UseFocus != "pal-use" || !strings.Contains(r.UseLive, "receipt: block plain") {
				t.Errorf("use as block: label %q refusal %q focus %q live %q\n%s", r.UseLabel, r.UseRefusal, r.UseFocus, r.UseLive, r.AfterUse)
			}
			if len(r.Console) > 0 || len(r.Failed) > 0 || len(r.OffOrigin) > 0 {
				t.Errorf("console %q, failed requests %q, off the origin %q", r.Console, r.Failed, r.OffOrigin)
			}
		})
	}
}

// The structural editor shows nothing of typed mode; a text that fails against the program's blocks shows
// NOT CERTIFIED beside the weakened banner, announced as an alert, and the baseline line says "compared
// with" and not "not weaker than".
func TestTypedBannersInChrome(t *testing.T) {
	var r struct {
		OK           bool   `json:"ok"`
		Error        string `json:"error"`
		TypedTagHide bool   `json:"typedTagHidden"`
		BlocksHidden bool   `json:"blocksHidden"`
		SkipHidden   bool   `json:"skipHidden"`
		TypedInfo    int    `json:"typedInfo"`
		Baseline     string `json:"baseline"`
		Banners      []struct {
			Role string `json:"role"`
			Word string `json:"word"`
			Line string `json:"line"`
		} `json:"banners"`
		Console []string `json:"console"`
	}
	servers := typedServers(t, "refund.yaml", strings.Replace(testFlow, "block: charge", "block: nowhere", 1))
	runScript(t, "typed-structural.mjs", servers["structural"], &r)
	if !r.OK || !r.TypedTagHide || !r.BlocksHidden || !r.SkipHidden || r.TypedInfo != 0 || len(r.Banners) != 0 {
		t.Errorf("structural: %+v", r)
	}
	for _, kind := range []string{"registry", "description"} {
		runScript(t, "typed-structural.mjs", servers[kind], &r)
		if !r.OK || r.TypedTagHide || r.BlocksHidden || len(r.Banners) != 1 || r.Banners[0].Word != "NOT CERTIFIED" || r.Banners[0].Role != "alert" || !strings.Contains(r.Banners[0].Line, "not certified") {
			t.Errorf("%s: %+v", kind, r)
		}
		if !strings.Contains(r.Baseline, "compared with") || strings.Contains(r.Baseline, "not weaker than") {
			t.Errorf("%s: baseline line %q", kind, r.Baseline)
		}
	}
	_ = runview.DescriptionNote
}

// A port's hit area never reaches over a neighbour's body: with the drawing scaled down until two nodes
// of a row are under 22 CSS px apart, a press on the neighbour's edge selects it and a drag from the
// port lands on it.
func TestDenseGraphPortInChrome(t *testing.T) {
	text := "version: 1\nflow: dense\nentry: a\nnodes:\n  - {name: a, block: a, safety: readonly}\n  - {name: b, block: b, safety: readonly}\n  - {name: c, block: c, safety: readonly}\nwiring:\n  - edge: [a, b]\n  - edge: [a, c]\n"
	s, _ := newTestServer(t, "dense.yaml", text)
	var r struct {
		OK             bool     `json:"ok"`
		Error          string   `json:"error"`
		GapPx          float64  `json:"gapPx"`
		HitW           float64  `json:"hitW"`
		Overlap        float64  `json:"overlap"`
		UnderPort      string   `json:"underPort"`
		TopAtEdge      string   `json:"topAtNeighbourEdge"`
		SelectedByEdge string   `json:"selectedByEdgeClick"`
		SelectedBody   string   `json:"selectedBody"`
		AfterDrag      string   `json:"afterDrag"`
		Console        []string `json:"console"`
	}
	runScript(t, "dense.mjs", s, &r)
	if !r.OK {
		t.Fatalf("the script failed: %s", r.Error)
	}
	if r.GapPx >= 22 {
		t.Fatalf("the test is not dense: the neighbour is %.1f px away", r.GapPx)
	}
	if r.Overlap != 0 || r.HitW < 16 {
		t.Errorf("the port's hit area overlaps the neighbour by %.1f px2 (width %.1f)", r.Overlap, r.HitW)
	}
	if !strings.Contains(r.UnderPort, "port-hit-box") {
		t.Errorf("the port is not pressable where it is drawn: %q", r.UnderPort)
	}
	if r.TopAtEdge != "c" || r.SelectedByEdge != "c" || r.SelectedBody != "b" {
		t.Errorf("a press at the neighbour's edge reaches %q and selects %q; b's body selects %q", r.TopAtEdge, r.SelectedByEdge, r.SelectedBody)
	}
	if !strings.Contains(r.AfterDrag, "edge: [b, c]") {
		t.Errorf("the drag onto the neighbour did not land:\n%s", r.AfterDrag)
	}
	if len(r.Console) > 0 {
		t.Errorf("console %q", r.Console)
	}
}
