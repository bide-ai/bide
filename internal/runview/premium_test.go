package runview

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"html/template"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
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

// TestStepsOnlyRuns: a run made of Journal.Step calls alone has no run:start. It is labeled
// "steps only (no run:start)", and its state is read from what the journal shows: done, halted
// (a live attempt marker with no result), then resolved once ResolveHalt records the outcome.
func TestStepsOnlyRuns(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	cs := &crashStore{Store: mem, lose: "charge", armed: false}
	j := agenttest.MustJournal(cs)
	h := Handler(Options{Source: JournalSource{J: j}})
	one := func(_ context.Context) (string, error) { return "ok", nil }

	if _, err := j.Step(ctx, "s-done", "lookup", one); err != nil {
		t.Fatal(err)
	}
	v := viewOf(t, h, "s-done")
	if v.Kind != KindSteps || v.State != "done" || v.StateText != "steps done" || v.KindText() != "steps only (no run:start)" {
		t.Fatalf("done run = %q/%q/%q", v.Kind, v.State, v.StateText)
	}
	if d := runVerdict(&View{State: "done", Records: 2}); d.Word != "STEPS DONE" {
		t.Errorf("verdict = %q", d.Word)
	}

	cs.armed = true
	if _, err := j.Step(ctx, "s-halt", "charge", func(context.Context) (string, error) { return "ch_1", nil }); !errors.Is(err, errCrash) {
		t.Fatalf("crashing Step = %v", err)
	}
	cs.armed = false
	if _, err := j.Step(ctx, "s-halt", "charge", one); !errorsAsHalt(err) {
		t.Fatalf("resumed Step = %v, want *agent.OutcomeUnknown", err)
	}
	v = viewOf(t, h, "s-halt")
	if v.Kind != KindSteps || v.State != "halted" || v.StateText != "halted: outcome unknown" || len(v.Halts) != 1 || v.Halts[0].ID != "charge" {
		t.Fatalf("halted run = %q/%q, halts %+v", v.Kind, v.State, v.Halts)
	}
	if d := runVerdict(&v); d.Word != "HALTED" {
		t.Errorf("halted verdict = %q", d.Word)
	}

	if err := agent.ResolveHalt(ctx, j, agent.HaltRef{RunID: "s-halt", Op: agent.OpRef{Kind: agent.OpStep, ID: "charge"}, Cause: agent.HaltCrashed}, agent.Outcome{Result: "ch_1"}); err != nil {
		t.Fatal(err)
	}
	v = viewOf(t, h, "s-halt")
	if v.State != "resolved" || v.StateText != "resolved" || len(v.Halts) != 1 && len(v.Halts) != 0 {
		t.Fatalf("resolved run = %q/%q, halts %+v", v.State, v.StateText, v.Halts)
	}
	if len(v.Halts) != 0 {
		t.Errorf("a resolved run still lists halts: %+v", v.Halts)
	}
	if d := runVerdict(&v); d.Word != "RESOLVED" {
		t.Errorf("resolved verdict = %q", d.Word)
	}

	// The list labels them, and groups halted ones for attention.
	l := listOf(t, h, "")
	byID := map[string]RunRow{}
	for _, r := range l.Rows {
		byID[r.ID] = r
	}
	if r := byID["s-done"]; r.KindText() != "steps only (no run:start)" || r.Group != "completed" {
		t.Errorf("s-done row = %+v", r)
	}
	if r := byID["s-halt"]; r.KindText() != "steps only (no run:start)" || r.Group != "completed" || r.StateText != "resolved" {
		t.Errorf("s-halt row = %+v", r)
	}
	_, page := get(t, h, "/")
	if !strings.Contains(page, "steps only (no run:start)") {
		t.Error("the run list does not say steps only")
	}
	_, page = get(t, h, "/run?id=s-halt")
	if !strings.Contains(page, "steps only (no run:start)") || !strings.Contains(page, "RESOLVED") {
		t.Error("the run page does not say steps only and resolved")
	}
}

func TestInitials(t *testing.T) {
	for id, want := range map[string]string{"ops": "OP", "finance": "FI", "risk-lead": "RL", "a": "A", "jane.doe@corp": "JD", "": "?", "---": "?", "élan": "ÉL"} {
		if got := initials(id); got != want {
			t.Errorf("initials(%q) = %q, want %q", id, got, want)
		}
	}
}

// TestApproverCards: each approver of a gate is a card: what they decided, whether their signature
// verified (and why not), the fingerprint of their key and the head that covers it. Without keys no
// fingerprint and no claim of verification is made.
func TestApproverCards(t *testing.T) {
	logPub := audit.Ed25519Verifier{Pub: key("log").Public().(ed25519.PublicKey)}
	j, sth := gatedRun(t, map[string]bool{"ops": true, "finance": true})
	cardsOf := func(p Prover) map[string]ApproverCard {
		v := gatedView(t, j, p) // in memory: the JSON view does not carry the cards
		out := map[string]ApproverCard{}
		for _, c := range v.Gates[0].Cards {
			out[c.Name] = c
		}
		if len(out) != 3 {
			t.Fatalf("cards = %+v", v.Gates[0].Cards)
		}
		return out
	}
	cs := cardsOf(HeadProver(sth, logPub, Approvals{Keys: approverKeys(nil)}))
	for _, who := range []string{"ops", "finance"} {
		c := cs[who]
		if c.State != "approved" || !c.VerifyOK || c.Verify != "signature verified" || !regexp.MustCompile(`^ed25519:[0-9a-f]{8}$`).MatchString(c.Fingerprint) ||
			c.DecidedAt != "2026-10-13 12:14:03.123" || c.RecordedAt != "2026-10-13 12:14:05.000" || c.Gap != "recorded 1.877s after the signed time" || len(c.Flags) != 0 {
			t.Errorf("%s card = %+v", who, c)
		}
	}
	if c := cs["risk"]; c.State != "pending" || c.Verify != "" || c.Initials != "RI" || c.Fingerprint == "" {
		t.Errorf("risk card = %+v", c)
	}
	if cs["ops"].Fingerprint == cs["finance"].Fingerprint {
		t.Error("two approvers share a fingerprint")
	}

	cs = cardsOf(HeadProver(sth, logPub, Approvals{Keys: approverKeys(map[string]string{"finance": "mallory"})}))
	if c := cs["finance"]; c.VerifyOK || c.Verify != "signature NOT verified" || c.Why == "" {
		t.Errorf("finance with the wrong key = %+v", c)
	}
	if c := cs["ops"]; !c.VerifyOK {
		t.Errorf("ops = %+v, want verified", c)
	}

	cs = cardsOf(HeadProver(sth, logPub, Approvals{}))
	if c := cs["ops"]; c.VerifyOK || c.Verify != "in the signed log, signature not checked" || c.Fingerprint != "" {
		t.Errorf("no keys: ops = %+v", c)
	}
	cs = cardsOf(func(context.Context, string, []agent.Record) *Proof { return nil })
	if c := cs["ops"]; c.VerifyOK || c.Verify != "signature not checked" || c.DecidedAt != "2026-10-13 12:14:03.123" || c.RecordedAt == "" {
		t.Errorf("no proof: ops = %+v", c)
	}

	h := Handler(Options{Source: JournalSource{J: j}, Provers: []Prover{HeadProver(sth, logPub, Approvals{Keys: approverKeys(nil)})}})
	_, page := get(t, h, "/run?id=g1")
	for _, want := range []string{`class="acard a-approved"`, `class="avatar" aria-hidden="true">OP<`, "signature verified", "key ed25519:", "Proof path",
		">decided</dt><dd>2026-10-13 12:14:03.123</dd></div>", ">recorded</dt><dd>2026-10-13 12:14:05.000</dd></div>", "recorded 1.877s after the signed time", `class="small muted atimesnote"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %s", want)
		}
	}
}

// TestMerklePathClimbs: for journals of every small size and every record in them, the path the
// page draws climbs from the record's leaf hash to the signed root, with each sibling on the side
// RFC 6962 puts it, so the diagram is a path that holds.
func TestMerklePathClimbs(t *testing.T) {
	ctx := context.Background()
	for n := 1; n <= 9; n++ {
		j := agenttest.MemJournal()
		for i := range n {
			if _, err := j.Step(ctx, "m", fmt.Sprintf("s%d", i), func(context.Context) (int, error) { return i, nil }); err != nil {
				t.Fatal(err)
			}
		}
		th, err := audit.NewTreeHead(ctx, j, "m", time.Now().UnixNano())
		if err != nil {
			t.Fatal(err)
		}
		sth, err := audit.SignTreeHead(th, audit.Ed25519Signer{Priv: key("log")})
		if err != nil {
			t.Fatal(err)
		}
		for idx := 0; idx < sth.Size; idx++ {
			b, err := audit.ProveRecord(ctx, j, "m", idx, sth)
			if err != nil {
				t.Fatal(err)
			}
			m := merklePath(b, "r")
			if !m.Climbed || m.Seq != idx+1 || m.Size != sth.Size || len(m.Steps) != len(b.Inclusion.Path) {
				t.Fatalf("size %d, record %d: %+v", sth.Size, idx, m)
			}
			if len(m.Steps) > 0 && m.Steps[len(m.Steps)-1].Node != m.Root {
				t.Fatalf("size %d, record %d: the last node is not the root", sth.Size, idx)
			}
			if d := m.Diagram(); !strings.Contains(string(d), `role="group"`) || !strings.Contains(string(d), short8(m.Leaf)) {
				t.Fatalf("size %d, record %d: no diagram", sth.Size, idx)
			}
			// A path that does not climb to the root draws nothing.
			bad := b
			bad.STH.Root = append([]byte{}, b.STH.Root...)
			bad.STH.Root[0] ^= 1
			if bm := merklePath(bad, "r"); bm.Climbed || bm.Diagram() != "" {
				t.Fatalf("size %d, record %d: a wrong root still draws", sth.Size, idx)
			}
		}
	}
}

// TestProofCarriesThePath: a verified head gives the proof the path of the last record it covers,
// and an unverified one gives none.
func TestProofCarriesThePath(t *testing.T) {
	logPub := audit.Ed25519Verifier{Pub: key("log").Public().(ed25519.PublicKey)}
	j, sth := gatedRun(t, map[string]bool{"ops": true, "finance": true})
	m := gatedView(t, j, HeadProver(sth, logPub, Approvals{})).Proof.Merkle
	if m == nil || m.Seq != sth.Size || !m.Climbed || m.Root != fmt.Sprintf("%x", sth.Root) || m.SignedAt.IsZero() {
		t.Fatalf("merkle = %+v", m)
	}
	wrong := audit.Ed25519Verifier{Pub: key("someone-else").Public().(ed25519.PublicKey)}
	if p := gatedView(t, j, HeadProver(sth, wrong, Approvals{})).Proof; p.OK || p.Merkle != nil {
		t.Fatalf("a head that does not verify carries a path: %+v", p)
	}
}

// gatedView builds g1's View in memory, with one Prover.
func gatedView(t *testing.T, j *agent.Journal, p Prover) *View {
	t.Helper()
	o := Options{Source: JournalSource{J: j}, Provers: []Prover{p}}
	v, err := o.view(context.Background(), "g1")
	if err != nil || v == nil {
		t.Fatalf("view = %v, %v", v, err)
	}
	return v
}

func TestHeadAndFavicon(t *testing.T) {
	h := head("VERIFIED · r1 · bide view", "good", "approval")
	icon, err := url.PathUnescape(strings.TrimPrefix(string(h.Icon), "data:image/svg+xml,"))
	if err != nil || !strings.Contains(icon, "#2f9e4f") || !strings.Contains(icon, "<path") || h.Title != "VERIFIED · r1 · bide view" {
		t.Errorf("head = %+v, icon %q", h, icon)
	}
	if h := head("t", "no-such-class", "no-such-icon"); !strings.Contains(string(h.Icon), "6b6b66") {
		t.Errorf("an unknown class has no fallback color: %s", h.Icon)
	}
	// Each verdict class has its own color, so a tab strip tells runs apart.
	seen := map[template.URL]string{}
	for class := range faviconColors {
		i := head("t", class, "ok").Icon
		if other, dup := seen[i]; dup {
			t.Errorf("classes %s and %s share an icon", class, other)
		}
		seen[i] = class
	}
	hc := Handler(Options{Config: &Config{Name: "policy.json", Data: []byte(policyConfig)}})
	_, page := get(t, hc, "/")
	if !regexp.MustCompile(`<title>[^<]*POLICY VIOLATION[^<]* · refund · bide view</title>`).MatchString(page) || !strings.Contains(page, `rel="icon" type="image/svg+xml" href="data:image/svg&#43;xml,`) {
		t.Errorf("the config page's title or icon: %.400s", page)
	}
	if strings.Contains(page, `href="#ZgotmplZ"`) {
		t.Error("the favicon URL was filtered out")
	}
}

// TestPagesAskForNothingElsewhere: every page and asset the viewer serves refers only to this
// server (or a data: URL), and no script is inline, which the content policy would refuse.
func TestPagesAskForNothingElsewhere(t *testing.T) {
	logPub := audit.Ed25519Verifier{Pub: key("log").Public().(ed25519.PublicKey)}
	j, sth := gatedRun(t, map[string]bool{"ops": true, "finance": true})
	h := Handler(Options{Source: JournalSource{J: j}, Provers: []Prover{HeadProver(sth, logPub, Approvals{Keys: approverKeys(nil)})}})
	ext := regexp.MustCompile(`(?:src|href)="(https?:)?//[^"]*"|url\(\s*['"]?https?:`)
	for _, target := range []string{"/", "/run?id=g1"} {
		_, page := get(t, h, target)
		if m := ext.FindString(page); m != "" {
			t.Errorf("%s refers off the server: %s", target, m)
		}
		for _, tag := range regexp.MustCompile(`<script[^>]*>`).FindAllString(page, -1) {
			if !strings.Contains(tag, ` src="/assets/`) {
				t.Errorf("%s has an inline script: %s", target, tag)
			}
		}
		if strings.Contains(page, ` style="`) {
			t.Errorf("%s has an inline style", target)
		}
	}
	for _, f := range []string{"theme.js", "view.js", "share.js", "live.js", "style.css", "graph.css", "timeline.css", "extras.css", "diff.css", "runs.css", "live.css"} {
		code, body := get(t, h, "/assets/"+f)
		if code != 200 || ext.MatchString(body) || strings.Contains(body, "@import") {
			t.Errorf("asset %s: %d, refers off the server", f, code)
		}
		if strings.HasSuffix(f, ".js") && (strings.Contains(body, "XMLHttpRequest") || strings.Contains(body, "sendBeacon") || strings.Contains(body, "WebSocket")) {
			t.Errorf("asset %s makes a request it should not", f)
		}
	}
}

// TestMerkleDiagramFitsTheSignedTime: the signed-time label beside the root sits inside the SVG,
// with a conservative 8 user units for each character at the label's font size, on either side
// the sibling can be.
func TestMerkleDiagramFitsTheSignedTime(t *testing.T) {
	for _, left := range []bool{true, false} {
		m := &MerklePath{
			Seq: 13, Size: 13, Name: "run:complete", Leaf: "7d1629c1aa", Root: "267b08bdaa", Alg: "ed25519:abcd", Climbed: true,
			SignedAt: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC),
			Steps: []MerkleStep{
				{Sibling: "91e79d07aa", Left: left, Node: "49c75e97aa"},
				{Sibling: "dbb04dc0aa", Left: left, Node: "267b08bdaa"},
			},
		}
		svg := string(m.Diagram())
		vb := regexp.MustCompile(`viewBox="0 0 (\d+) \d+"`).FindStringSubmatch(svg)
		lab := regexp.MustCompile(`<text class="mksub" x="(\d+)" y="\d+" text-anchor="(\w+)">(2026[^<]*)</text>`).FindStringSubmatch(svg)
		if vb == nil || lab == nil {
			t.Fatalf("left=%v: no viewBox or signed-time label in %s", left, svg)
		}
		var w, x int
		fmt.Sscan(vb[1], &w)
		fmt.Sscan(lab[1], &x)
		lo, hi := x, x+8*len(lab[3])
		if lab[2] == "end" {
			lo, hi = x-8*len(lab[3]), x
		}
		if lo < 0 || hi > w {
			t.Errorf("left=%v: the label %q spans %d to %d, outside the SVG width %d", left, lab[3], lo, hi, w)
		}
		// The label sits opposite the last sibling, so the two never overlap.
		if (lab[2] == "end") == left {
			t.Errorf("left=%v: the label is anchored %s, on the sibling's side", left, lab[2])
		}
	}
}
