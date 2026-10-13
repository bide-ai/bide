package runview

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/plan"
)

// sampleGraphs are the flows the layout is checked on: the policy config (a switch, a gated node,
// a long edge), a diff (ghost nodes), and graphs built to stress it: a fan-out wider than a row, a
// loop, and an edge that skips several rows.
func sampleGraphs(t *testing.T) map[string]*Graph {
	t.Helper()
	pol := BuildConfig(Config{Name: "policy.json", Data: []byte(policyConfig)})
	df := diffOf(t, diffV1, diffV2, nil, nil)
	mk := func(names []string, edges [][3]string) *Graph {
		g := &Graph{}
		for _, n := range names {
			g.Nodes = append(g.Nodes, GraphNode{Name: n, Kind: "step", Status: "done"})
		}
		for _, e := range edges {
			g.Edges = append(g.Edges, GraphEdge{From: e[0], To: e[1], Label: e[2], Back: e[2] == "loop"})
		}
		layout(g, names[0])
		return g
	}
	return map[string]*Graph{
		"policy": pol.Graph,
		"diff":   df.Graph,
		"fan": mk([]string{"a", "b", "c", "d", "e", "f", "g"},
			[][3]string{{"a", "b", ""}, {"a", "c", ""}, {"a", "d", ""}, {"a", "e", ""}, {"a", "f", ""}, {"b", "g", ""}, {"c", "g", ""}, {"d", "g", ""}, {"e", "g", ""}, {"f", "g", ""}}),
		"loop": mk([]string{"a", "b", "c", "d"},
			[][3]string{{"a", "b", ""}, {"b", "c", "when"}, {"c", "d", "else"}, {"c", "b", "loop"}, {"d", "a", "loop"}}),
		"skip": mk([]string{"a", "b", "c", "d", "e"},
			[][3]string{{"a", "b", ""}, {"b", "c", ""}, {"c", "d", ""}, {"d", "e", ""}, {"a", "e", "direct"}, {"a", "d", "short cut"}, {"b", "e", ""}}),
	}
}

func TestLayoutNoOverlap(t *testing.T) {
	for name, g := range sampleGraphs(t) {
		t.Run(name, func(t *testing.T) {
			if g == nil || len(g.Nodes) == 0 {
				t.Fatal("no graph")
			}
			for i, a := range g.Nodes {
				ra := rect{a.x, a.y, g.nw, g.nh}
				if a.x < 0 || a.y < 0 || a.x+g.nw > g.Width || a.y+g.nh > g.Height {
					t.Errorf("%s at (%d,%d) is outside the %dx%d canvas", a.Name, a.x, a.y, g.Width, g.Height)
				}
				for _, b := range g.Nodes[i+1:] {
					if ra.overlaps(rect{b.x - gapDum, b.y, g.nw + 2*gapDum, g.nh}) {
						t.Errorf("%s (%d,%d) and %s (%d,%d) overlap or touch", a.Name, a.x, a.y, b.Name, b.x, b.y)
					}
				}
			}
			for _, e := range g.Edges {
				if e.d == "" {
					t.Errorf("edge %s -> %s has no path", e.From, e.To)
				}
			}
			// A forward edge points down: its target's row is below its source's.
			byName := map[string]GraphNode{}
			for _, n := range g.Nodes {
				byName[n.Name] = n
			}
			for _, e := range g.Edges {
				if !e.arc && byName[e.To].y <= byName[e.From].y {
					t.Errorf("forward edge %s -> %s does not point down", e.From, e.To)
				}
			}
		})
	}
}

// TestLayoutLongEdgesClearNodes: the vertical runs of an edge that skips rows pass beside every
// node of the rows it crosses, never through one.
func TestLayoutLongEdgesClearNodes(t *testing.T) {
	g := sampleGraphs(t)["skip"]
	re := regexp.MustCompile(`L(\d+),(\d+)`)
	for _, e := range g.Edges {
		for _, m := range re.FindAllStringSubmatch(e.d, -1) {
			var x, y int
			fmt.Sscan(m[1], &x)
			fmt.Sscan(m[2], &y)
			for _, n := range g.Nodes {
				if x > n.x && x < n.x+g.nw && y > n.y && y < n.y+g.nh {
					t.Errorf("edge %s -> %s passes through %s at (%d,%d)", e.From, e.To, n.Name, x, y)
				}
			}
		}
	}
}

// TestLayoutDeterministic: the same graph draws byte for byte the same, run after run.
func TestLayoutDeterministic(t *testing.T) {
	first := map[string]string{}
	for name, g := range sampleGraphs(t) {
		first[name] = g.SVG
	}
	for i := 0; i < 5; i++ {
		for name, g := range sampleGraphs(t) {
			if g.SVG != first[name] {
				t.Fatalf("%s: the drawing changed between runs", name)
			}
		}
	}
}

func TestSVGIsAccessibleAndComplete(t *testing.T) {
	g := sampleGraphs(t)["policy"]
	for _, want := range []string{`role="group"`, `aria-label="flow graph:`, `role="img" aria-label="charge (`, `<title>`, `href="#gi-side"`, `class="gbadge`, `>2/3<`, `<symbol id="gi-gate"`} {
		if !strings.Contains(g.SVG, want) {
			t.Errorf("the drawing lacks %s", want)
		}
	}
	// A node's risk shows in words and a glyph, not only color.
	if !strings.Contains(g.SVG, "side effect") || !strings.Contains(g.SVG, "read-only") {
		t.Error("the risk classes are not in words")
	}
	if strings.Contains(g.SVG, "<script") || strings.Contains(g.SVG, "http://") && !strings.Contains(g.SVG, `xmlns`) {
		t.Error("the drawing has a script or an external reference")
	}
}

// assertions is one assertion result: one that holds, or one that is violated.
func assertions(holds bool) []plan.AssertionResult {
	return []plan.AssertionResult{{Text: "a", Holds: holds, Path: []string{"x"}}}
}

func TestVerdictSelection(t *testing.T) {
	ok := &Proof{OK: true, Covers: 5, Summary: "verifies: all 5 records are under the signed head"}
	bad := &Proof{OK: false, Covers: 5, Summary: "does not verify"}
	gate := func(sig string) []Gate {
		return []Gate{{ToolUseID: "c1", ToolName: "charge", Need: 2, Approvers: []string{"a", "b", "c"}, ApprovedBy: []string{"a", "b"}, Tallied: true, Signatures: sig, SignaturesOK: strings.Contains(sig, "verified") && !strings.Contains(sig, "NOT")}}
	}
	for _, c := range []struct {
		name  string
		v     View
		word  string
		class string
	}{
		{"halted", View{State: "halted", Halts: []Halt{{ID: "charge-1"}}}, "HALTED", "bad"},
		{"halted beats a verified proof", View{State: "halted", Halts: []Halt{{ID: "x"}}, Proof: ok}, "HALTED", "bad"},
		{"waiting", View{State: "waiting", Gates: []Gate{{ToolName: "refund", Need: 2, Approvers: []string{"a", "b"}, Waiting: true, Pending: []string{"b"}, Status: "pending"}}}, "WAITING FOR APPROVAL", "wait"},
		{"aborted", View{State: "aborted", Terminal: "boom"}, "FAILED", "bad"},
		{"cancelled", View{State: "cancelled"}, "FAILED", "bad"},
		{"completed, proof fails", View{State: "completed", Proof: bad}, "FAILED", "bad"},
		{"completed, signatures fail", View{State: "completed", Proof: ok, Gates: gate("signatures NOT verified: x")}, "FAILED", "bad"},
		{"completed, proof verifies", View{State: "completed", Proof: ok}, "VERIFIED", "good"},
		{"completed, signatures verified", View{State: "completed", Proof: ok, Gates: gate("2 of 3: signatures verified")}, "VERIFIED", "good"},
		{"completed, no proof", View{State: "completed"}, "COMPLETED", "info"},
		{"started", View{State: "started"}, "IN PROGRESS", "info"},
		{"not started", View{State: "not_started"}, "NOT STARTED", "info"},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.v.Records = 5
			d := runVerdict(&c.v)
			if d.Word != c.word || d.Class != c.class || d.Icon == "" || d.Line == "" {
				t.Errorf("verdict = %q/%s/%q, want %q/%s", d.Word, d.Class, d.Icon, c.word, c.class)
			}
			if n := len(d.Stats); n < 1 || n > 4 {
				t.Errorf("%d stats, want 1 to 4", n)
			}
		})
	}
}

func TestVerdictStats(t *testing.T) {
	v := View{State: "completed", Records: 12, Proof: &Proof{OK: true, Covers: 12},
		Gates: []Gate{{Need: 2, Approvers: []string{"a", "b", "c"}, ApprovedBy: []string{"a", "b"}, Tallied: true, SignaturesOK: true, Signatures: "2 of 3: signatures verified"}}}
	d := runVerdict(&v)
	got := map[string]string{}
	for _, s := range d.Stats {
		got[s.Label] = s.Value
	}
	if got["journal records"] != "12" || got["records signed"] != "12/12" || got["approvals"] != "2/3" {
		t.Errorf("stats = %v", got)
	}
}

func TestConfigVerdictSelection(t *testing.T) {
	hold := assertions(true)
	viol := assertions(false)
	for _, c := range []struct {
		name string
		v    ConfigView
		word string
	}{
		{"violations first", ConfigView{Errors: 3, Assertions: append(hold, viol...)}, "1 POLICY VIOLATION"},
		{"two violations", ConfigView{Assertions: append(viol, viol...)}, "2 POLICY VIOLATIONS"},
		{"errors", ConfigView{Errors: 2, Assertions: hold}, "2 CONFIG ERRORS"},
		{"all hold", ConfigView{Assertions: append(hold, hold...)}, "ALL ASSERTIONS HOLD"},
		{"none declared", ConfigView{}, "NO ASSERTIONS"},
	} {
		if d := configVerdict(&c.v); d.Word != c.word {
			t.Errorf("%s: %q, want %q", c.name, d.Word, c.word)
		}
	}
	if d := configVerdict(&ConfigView{Assertions: viol}); d.Class != "bad" {
		t.Errorf("violated class = %s", d.Class)
	}
	if d := configVerdict(&ConfigView{Assertions: hold}); d.Class != "good" {
		t.Errorf("hold class = %s", d.Class)
	}
}

func TestDiffVerdictSelection(t *testing.T) {
	changes := []Change{{Change: "changed", What: "node", Node: "charge"}}
	for _, c := range []struct {
		name, word string
		v          DiffView
	}{
		{"weakened first", "APPROVAL GATE WEAKENED", DiffView{Changes: changes, Weakened: []string{"charge: approval 2 of 3 -> 1 of 3"}, Strengthened: []string{"x"}}},
		{"weakened with no other change", "APPROVAL GATE WEAKENED", DiffView{Weakened: []string{"x"}}},
		{"none", "NO CHANGES", DiffView{}},
		{"strengthened", "1 CHANGE, GATE STRENGTHENED", DiffView{Changes: changes, Strengthened: []string{"x"}}},
		{"plain", "1 CHANGE", DiffView{Changes: changes}},
	} {
		if d := diffVerdict(&c.v); d.Word != c.word {
			t.Errorf("%s: %q, want %q", c.name, d.Word, c.word)
		}
	}
	if d := diffVerdict(&DiffView{Weakened: []string{"x"}}); d.Class != "bad" || len(d.Details) != 1 {
		t.Errorf("weakened verdict %+v", d)
	}
}

func TestRunListVerdict(t *testing.T) {
	for _, c := range []struct {
		counts map[string]int
		all    int
		word   string
	}{
		{map[string]int{"halted": 2, "waiting": 1}, 9, "2 HALTED"},
		{map[string]int{"waiting": 1}, 4, "1 WAITING FOR APPROVAL"},
		{map[string]int{"failed": 3}, 4, "3 FAILED OR CANCELLED"},
		{map[string]int{"completed": 4}, 4, "ALL CLEAR"},
		{nil, 0, "NO RUNS"},
	} {
		if d := (RunList{Counts: c.counts, All: c.all}).Verdict(); d.Word != c.word {
			t.Errorf("%v: %q, want %q", c.counts, d.Word, c.word)
		}
	}
}

// TestPagesOpenWithTheVerdict: every page's first content is the verdict banner, ahead of the
// cards, and the JSON views do not carry it.
func TestPagesOpenWithTheVerdict(t *testing.T) {
	h := Handler(Options{Config: &Config{Name: "policy.json", Data: []byte(policyConfig)}})
	_, body := get(t, h, "/")
	i, j := strings.Index(body, `class="verdict`), strings.Index(body, `class="cards"`)
	if i < 0 || j < 0 || i > j || !strings.Contains(body, "POLICY VIOLATION") && !strings.Contains(body, "ASSERTIONS") {
		t.Errorf("verdict at %d, cards at %d", i, j)
	}
	_, js := get(t, h, "/config.json")
	if strings.Contains(strings.ToLower(js), "verdict") {
		t.Error("the JSON view carries the verdict")
	}
}

func TestHighlightJSON(t *testing.T) {
	src := "{\n  \"name\": \"a<b>&\\\"q\\\"\",\n  \"n\": -1.5e3,\n  \"ok\": true,\n  \"no\": false,\n  \"z\": null,\n  \"l\": [1, \"x\"]\n}"
	out := string(highlightJSON(src))
	for _, want := range []string{
		`<span class="jk">&#34;name&#34;</span>`, `<span class="jv">&#34;a&lt;b&gt;&amp;\&#34;q\&#34;&#34;</span>`,
		`<span class="jn">-1.5e3</span>`, `<span class="jb">true</span>`, `<span class="jb">false</span>`, `<span class="jz">null</span>`,
		`<span class="jk">&#34;l&#34;</span>`, `<span class="jn">1</span>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %s\n%s", want, out)
		}
	}
	if strings.Contains(out, "<b>") || strings.Contains(out, "<script") {
		t.Error("text was not escaped")
	}
	// Taking the tags off gives the text back, whatever the text is.
	strip := func(s string) string {
		s = regexp.MustCompile(`</?span[^>]*>`).ReplaceAllString(s, "")
		return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&", "&#34;", `"`, "&#39;", "'").Replace(s)
	}
	for _, s := range []string{src, `{"cut": "no end`, `not json <script>alert(1)</script> é ✓`, "", `"a" : 1`, `{"a":[tru`, "-", `"\`} {
		if got := strip(string(highlightJSON(s))); got != s {
			t.Errorf("round trip of %q gave %q", s, got)
		}
	}
	if !strings.Contains(string(highlightJSON(`"a" : 1`)), `class="jk"`) {
		t.Error("a key before a spaced colon is not a key")
	}
}

func TestPrettyJSON(t *testing.T) {
	if got := prettyJSON([]byte(`{"a":[1,2]}`)); got != "{\n  \"a\": [\n    1,\n    2\n  ]\n}" {
		t.Errorf("pretty = %q", got)
	}
	if got := prettyJSON([]byte("not json")); got != "not json" {
		t.Errorf("non-JSON = %q", got)
	}
	big := prettyJSON([]byte(`"` + strings.Repeat("é", maxPayload) + `"`))
	if !strings.Contains(big, "more bytes not shown") || len(big) > maxPayload+200 {
		t.Errorf("a large payload is not cut (len %d)", len(big))
	}
	for i := 0; i < len(big); i++ { // a cut never splits a character
		if big[i] == 0xEF {
			t.Fatal("invalid UTF-8 in the cut payload")
		}
	}
}

func TestTimelineTimes(t *testing.T) {
	t0 := time.Date(2026, 10, 13, 16, 0, 0, 0, time.UTC)
	evs := []Event{{Seq: 1}, {Seq: 2, At: t0}, {Seq: 3}, {Seq: 4, At: t0.Add(1500 * time.Millisecond)}, {Seq: 5, At: t0.Add(6 * time.Second)}}
	timeline(evs, t0)
	if evs[1].Rel != "start" || evs[3].Rel != "+1.5s" || evs[4].Rel != "+6.0s" || evs[0].Rel != "" || evs[2].Bar != nil {
		t.Errorf("rel = %q %q %q", evs[1].Rel, evs[3].Rel, evs[4].Rel)
	}
	if evs[3].Abs != "2026-10-13 16:00:01.500 UTC" {
		t.Errorf("abs = %q", evs[3].Abs)
	}
	if b := evs[3].Bar; b == nil || b.X < 24 || b.X > 26 || b.W < 74 || b.W > 76 || !strings.Contains(b.Label, "4.5s") {
		t.Errorf("bar = %+v", evs[3].Bar)
	}
	for _, e := range evs {
		if e.Bar != nil && (e.Bar.X < 0 || e.Bar.X+e.Bar.W > 100.001) {
			t.Errorf("bar %+v outside the track", e.Bar)
		}
	}
	for d, want := range map[time.Duration]string{850 * time.Millisecond: "850ms", 3200 * time.Millisecond: "3.2s", 245 * time.Second: "4m 05s", 130 * time.Minute: "2h 10m", 76 * time.Hour: "3d 4h"} {
		if got := durText(d); got != want {
			t.Errorf("durText(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestEventIcons(t *testing.T) {
	for _, e := range []Event{{Class: "model"}, {Class: "tool"}, {Class: "attempt"}, {Class: "approval"}, {Class: "end"}, {Class: "start"}, {Class: "meta"}, {Class: "node"}, {Class: "value"}, {Class: "value", Kind: string(agent.StepSignal)}, {Class: "unheard-of"}} {
		if _, ok := icons[e.IconName()]; !ok {
			t.Errorf("%+v: icon %q is not drawn", e, e.IconName())
		}
	}
}
