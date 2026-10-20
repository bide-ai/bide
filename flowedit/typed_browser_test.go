package flowedit

import (
	"encoding/json"
	"reflect"
	"slices"
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
	FitRowsReceipt []string `json:"fitRowsReceipt"`
	SecondName     string   `json:"secondName"`
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
			if !strings.HasPrefix(r.FirstLabel, "charge step \u00b7 takes ") || !strings.Contains(r.FirstLabel, " \u00b7 gives ") || !strings.HasSuffix(r.FirstLabel, " \u00b7 safety side effect \u00b7 no approval gate \u00b7 used") {
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
			if strings.Join(r.FitRowsReceipt, ",") != "receipt,shout" || r.SecondName != "count_2" {
				t.Errorf("the filter does not follow the selection, or a second name is not unique: %q, %q", r.FitRowsReceipt, r.SecondName)
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
		OK            bool   `json:"ok"`
		Error         string `json:"error"`
		TypedTagHide  bool   `json:"typedTagHidden"`
		BlocksHidden  bool   `json:"blocksHidden"`
		SkipHidden    bool   `json:"skipHidden"`
		TypedInfo     int    `json:"typedInfo"`
		Baseline      string `json:"baseline"`
		TypedInfoText string `json:"typedInfoText"`
		VerdictClass  string `json:"verdictClass"`
		Banners       []struct {
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
		if !strings.Contains(r.TypedInfoText, "Types are checked in full once the errors below are fixed") || strings.Contains(r.VerdictClass, "v-good") {
			t.Errorf("%s: an unresolved config reads as checked: %q, verdict %q", kind, r.TypedInfoText, r.VerdictClass)
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

// The typed editor's states under axe-core, in light and dark: opened with its palette, a node selected with
// the palette filtered, a search with no match, and the NOT CERTIFIED banner.
func TestTypedAccessibilityInChrome(t *testing.T) {
	for _, kind := range []string{"registry", "description"} {
		s := typedServers(t, "refund.yaml", testFlow)[kind]
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
		runScript(t, "typed-a11y.mjs", s, &r)
		if !r.OK {
			t.Fatalf("%s: the script failed: %s", kind, r.Error)
		}
		if len(r.Results) != 8 {
			t.Errorf("%s: %d states were checked, want 8 (4 states in 2 themes)", kind, len(r.Results))
		}
		for state, res := range r.Results {
			if res.Passes < 20 {
				t.Errorf("%s %s: only %d rules passed: the page was not checked", kind, state, res.Passes)
			}
			for _, v := range res.Violations {
				t.Errorf("%s %s: %s (%s): %s at %q", kind, state, v.ID, v.Impact, v.Help, v.Nodes)
			}
		}
		break // one source is enough: the page is the same
	}
}

// Nodes of equal width stacked in a column, at a small scale (a fan-out of nine, a 420 px window): each port's
// hit box still contains its circle, the circle is what a press at its centre reaches, and a drag from it starts.
func TestStackedPortsInChrome(t *testing.T) {
	var b strings.Builder
	b.WriteString("version: 1\nflow: fan\nentry: a\nnodes:\n  - {name: a, block: a, safety: readonly}\n")
	for i := 0; i < 9; i++ {
		b.WriteString("  - {name: n" + string(rune('0'+i)) + ", block: n, safety: readonly}\n")
	}
	b.WriteString("  - {name: z, block: z, safety: readonly}\nwiring:\n")
	for i := 0; i < 9; i++ {
		b.WriteString("  - edge: [a, n" + string(rune('0'+i)) + "]\n  - edge: [n" + string(rune('0'+i)) + ", z]\n")
	}
	s, _ := newTestServer(t, "fan.yaml", b.String())
	var r struct {
		OK    bool    `json:"ok"`
		Error string  `json:"error"`
		Scale float64 `json:"scale"`
		Ports []struct {
			Name      string `json:"name"`
			Inside    bool   `json:"inside"`
			TopNode   string `json:"topNode"`
			TopIsHit  bool   `json:"topIsHit"`
			DragLines int    `json:"dragLines"`
		} `json:"ports"`
		Console []string `json:"console"`
	}
	runScript(t, "dense-stack.mjs", s, &r)
	if !r.OK {
		t.Fatalf("the script failed: %s", r.Error)
	}
	if r.Scale > 0.22 || len(r.Ports) != 11 {
		t.Fatalf("the test is not dense: scale %.2f, %d ports", r.Scale, len(r.Ports))
	}
	for _, p := range r.Ports {
		if !p.Inside || p.TopNode != p.Name || !p.TopIsHit || p.DragLines != 1 {
			t.Errorf("port of %s: circle inside its hit box %v, a press at its centre reaches %q (hit box %v), drag lines %d", p.Name, p.Inside, p.TopNode, p.TopIsHit, p.DragLines)
		}
	}
	if len(r.Console) > 0 {
		t.Errorf("console %q", r.Console)
	}
}

// What a save asks the author to confirm is exactly what is true of it, each statement with its own box, and
// Save waits for all of them: nothing asked, a weakening only, a text that cannot be certified only, and
// both. An existing edge whose types differ is marked in the inspector, with the words and not only a color.
func TestSaveDialogConfirmationsInChrome(t *testing.T) {
	noAssert := testFlow[:strings.Index(testFlow, "assert:")]
	unknown := func(s string) string { return strings.Replace(s, "block: receipt", "block: nowhere", 1) }
	cases := []struct {
		name     string
		baseline string
		edits    [][2]string
		titles   []string
		says     []string
	}{
		{"nothing to confirm", noAssert, nil, nil, nil},
		{"a weakening", noAssert, [][2]string{{"need: 2", "need: 1"}}, []string{"THIS WEAKENS THE POLICY"}, []string{"weakening this policy"}},
		{"cannot be certified", unknown(noAssert), nil, []string{"THIS CANNOT BE CERTIFIED"}, []string{"without a certified type check"}},
		{"both", unknown(noAssert), [][2]string{{"need: 2", "need: 1"}}, []string{"THIS WEAKENS THE POLICY", "THIS CANNOT BE CERTIFIED"}, []string{"weakening this policy", "without a certified type check"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := typedServers(t, "refund.yaml", c.baseline)["registry"]
			raw, _ := json.Marshal(c.edits)
			t.Setenv("EDITS", string(raw))
			var r struct {
				OK     bool   `json:"ok"`
				Error  string `json:"error"`
				Groups []struct {
					Title string `json:"title"`
					Label string `json:"label"`
					ID    string `json:"id"`
				} `json:"groups"`
				DialogText          string   `json:"dialogText"`
				SaveDisabledAtStart bool     `json:"saveDisabledAtStart"`
				AfterEach           []bool   `json:"afterEach"`
				Console             []string `json:"console"`
			}
			runScript(t, "typed-save.mjs", s, &r)
			if !r.OK {
				t.Fatalf("the script failed: %s", r.Error)
			}
			var titles, says []string
			for _, g := range r.Groups {
				titles = append(titles, g.Title)
				says = append(says, g.Label)
			}
			if strings.Join(titles, "|") != strings.Join(c.titles, "|") || len(says) != len(c.says) {
				t.Fatalf("statements %q, want %q", titles, c.titles)
			}
			for i, want := range c.says {
				if !strings.Contains(says[i], want) {
					t.Errorf("statement %d says %q, want it to say %q", i, says[i], want)
				}
			}
			// Only true statements: no word of a statement that is not being asked.
			for _, wrong := range []string{"THIS WEAKENS THE POLICY", "THIS CANNOT BE CERTIFIED", "THIS COULD NOT BE COMPARED"} {
				if strings.Contains(r.DialogText, wrong) != slices.Contains(c.titles, wrong) {
					t.Errorf("the dialog says %q: %v, asked: %v\n%s", wrong, strings.Contains(r.DialogText, wrong), slices.Contains(c.titles, wrong), r.DialogText)
				}
			}
			// Save waits for every box: disabled until the last is ticked.
			if len(c.titles) > 0 {
				want := make([]bool, len(c.titles))
				for i := range want {
					want[i] = i < len(want)-1
				}
				if !r.SaveDisabledAtStart || !slices.Equal(r.AfterEach, want) {
					t.Errorf("save disabled at start %v, after each box %v, want %v", r.SaveDisabledAtStart, r.AfterEach, want)
				}
			} else if r.SaveDisabledAtStart {
				t.Errorf("save is disabled although nothing needs confirming")
			}
		})
	}
	// The existing edge whose types differ.
	text := strings.Replace(strings.Replace(testFlow, "block: receipt", "block: nowhere", 1), "wiring:\n", "  - {name: loose, block: count}\nwiring:\n  - edge: [validate, loose]\n", 1)
	t.Setenv("EDITS", "[]")
	var r struct {
		OK            bool     `json:"ok"`
		Error         string   `json:"error"`
		ExistingEdges []string `json:"existingEdges"`
	}
	runScript(t, "typed-save.mjs", typedServers(t, "refund.yaml", text)["registry"], &r)
	if !r.OK || !slices.Contains(r.ExistingEdges, "validate \u2192 loose (types differ)") {
		t.Errorf("existing edges %q (%s)", r.ExistingEdges, r.Error)
	}
}

// The save dialog is worded from the server's answer to Save (each item's kind), never from the page's own view,
// which can be one edit old, and never from the text of an item. CASE=race: the check of the edit is held back
// and Save is clicked before it returns; the edit makes the text uncertifiable, the page's view still says it is
// certified, and the dialog must still say it cannot be certified and nothing about a weakening. CASE=kinds: the
// answer is replaced by items whose text says nothing and whose kinds are given; each lands under its kind.
// CASE=nokind: a kind the page does not know is a weakening.
func TestSaveDialogIsWordedFromTheServersAnswerInChrome(t *testing.T) {
	noAssert := testFlow[:strings.Index(testFlow, "assert:")]
	type group struct {
		Title string   `json:"title"`
		Items []string `json:"items"`
	}
	run := func(t *testing.T, mode string, edits [][2]string) (groups []group, held int, viewVerdict string) {
		s := typedServers(t, "refund.yaml", noAssert)["registry"]
		raw, _ := json.Marshal(edits)
		t.Setenv("EDITS", string(raw))
		t.Setenv("CASE", mode)
		var r struct {
			OK          bool    `json:"ok"`
			Error       string  `json:"error"`
			Groups      []group `json:"groups"`
			HeldReports int     `json:"heldReports"`
			ViewVerdict string  `json:"viewVerdict"`
		}
		runScript(t, "typed-race.mjs", s, &r)
		if !r.OK {
			t.Fatalf("the script failed: %s", r.Error)
		}
		return r.Groups, r.HeldReports, r.ViewVerdict
	}
	t.Run("race", func(t *testing.T) {
		g, _, verdict := run(t, "race", [][2]string{{"block: receipt", "block: nowhere"}})
		if strings.Contains(verdict, "ERROR") {
			t.Fatalf("the page's own view was current (%q): the race was not made", verdict)
		}
		if len(g) != 1 || g[0].Title != "THIS CANNOT BE CERTIFIED" {
			t.Errorf("the dialog, with the page's own view one edit old (%q): %+v", verdict, g)
		}
	})
	t.Run("kinds", func(t *testing.T) {
		g, _, _ := run(t, "kinds", [][2]string{{"need: 2", "need: 1"}})
		want := []group{{"THIS WEAKENS THE POLICY", []string{"beta"}}, {"THIS CANNOT BE CERTIFIED", []string{"alpha"}}, {"THIS COULD NOT BE COMPARED", []string{"gamma"}}}
		if !reflect.DeepEqual(g, want) {
			t.Errorf("grouped %+v, want %+v", g, want)
		}
	})
	t.Run("a kind the page does not know", func(t *testing.T) {
		g, _, _ := run(t, "nokind", [][2]string{{"need: 2", "need: 1"}})
		if len(g) != 1 || g[0].Title != "THIS WEAKENS THE POLICY" || len(g[0].Items) != 3 {
			t.Errorf("grouped %+v", g)
		}
	})
}
