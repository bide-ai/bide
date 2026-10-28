package runview

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/bide-ai/bide/internal/taintwords"
	"github.com/bide-ai/bide/internal/term"
	"github.com/bide-ai/bide/plan"
)

func TestTaintView_Known(t *testing.T) {
	r := taintReport(map[string]plan.NodeTaint{
		"src":  {Verdict: noSrc, Declares: []string{"inbound-email"}, Reason: taintwords.SelfSource + "; " + taintwords.Caveat},
		"pay":  {Verdict: escal, Sources: []string{"inbound-email"}, Path: []string{"src", "pay"}},
		"look": {Verdict: reads, Sources: []string{"inbound-email"}, Reason: "taint policy not given: whether it escalates is not known"},
	}, effect, "src", "pay", "look")
	v := taintView(r)
	if v == nil || !v.Known {
		t.Fatalf("view %+v", v)
	}
	if v.Line != "taint: 2 nodes may read untrusted data (1 escalates)" {
		t.Errorf("line %q", v.Line)
	}
	var texts []string
	for _, n := range v.Nodes {
		texts = append(texts, n.Text)
	}
	want := []string{
		"src: no declared source before it; source: inbound-email; it is itself a source, and its own read is not gated; split the block",
		"pay: escalates, reads inbound-email via src > pay",
		"look: reads untrusted, reads inbound-email (earlier in the flow's order); taint policy not given: whether it escalates is not known",
	}
	if strings.Join(texts, "\n") != strings.Join(want, "\n") {
		t.Errorf("nodes:\n%s\nwant:\n%s", strings.Join(texts, "\n"), strings.Join(want, "\n"))
	}
	if v.Caveat == "" || !strings.Contains(v.Caveat, "input untrusted") {
		t.Errorf("caveat %q", v.Caveat)
	}
	b, _ := json.Marshal(v)
	if !strings.Contains(string(b), `"verdict":"escalates"`) || !strings.Contains(string(b), `"path":["src","pay"]`) || !strings.Contains(string(b), `"node":"pay"`) {
		t.Errorf("json %s", b)
	}
	var out bytes.Buffer
	writeTaint(&out, term.Style{}, "f.json", v)
	if !strings.HasPrefix(out.String(), "f.json: taint: 2 nodes may read untrusted data (1 escalates)\n  src:") {
		t.Errorf("text %q", out.String())
	}
}

func TestTaintView_NoSourceNeverSaysNoTaint(t *testing.T) {
	r := taintReport(map[string]plan.NodeTaint{"src": {Verdict: noSrc}, "pay": {Verdict: noSrc}, "look": {Verdict: noSrc}}, effect, "src", "pay", "look")
	v := taintView(r)
	if v == nil || !v.Known || !strings.HasPrefix(v.Line, "taint: no node declares an untrusted source") {
		t.Fatalf("%+v", v)
	}
	for _, w := range []string{"safe", "clean", "no taint", "protected"} {
		b, _ := json.Marshal(v)
		if strings.Contains(strings.ToLower(string(b)), w) {
			t.Errorf("view says %q: %s", w, b)
		}
	}
}

func TestTaintView_StructuralAndErrors(t *testing.T) {
	cfg := []byte(`{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"a","untrusted":"crm"},{"name":"b","block":"b"}],"wiring":[{"edge":["a","b"]}]}`)
	var out bytes.Buffer
	Check(&out, Config{Name: "f.json", Data: cfg})
	if out.String() != "f.json: taint: not known without -blocks\n" {
		t.Errorf("structural check %q", out.String())
	}
	v := BuildConfig(Config{Name: "f.json", Data: cfg})
	if v.Taint == nil || v.Taint.Known || len(v.Taint.Nodes) != 0 {
		t.Errorf("structural view %+v", v.Taint)
	}
	// Not known from a description of version 1 says why.
	// Not known from a description of version 1 says why.
	if tv := taintView(plan.ConfigReport{Typed: true, Taint: map[string]plan.NodeTaint{"a": {Verdict: "unknown", Reason: "the description does not carry taint declarations (version 1)"}}}); tv == nil || tv.Known || !strings.Contains(tv.Line, "version 1") {
		t.Errorf("v1 view %+v", tv)
	}
	// A typed report with errors has none.
	if taintView(plan.ConfigReport{Typed: true}) != nil {
		t.Error("a report with no taint has a view")
	}
}

func TestTaintTagAndGraph(t *testing.T) {
	lines, class := TaintTag(plan.NodeTaint{Verdict: escal, Sources: []string{"a", "b"}, Declares: []string{"c"}})
	if class != "escalates" || strings.Join(lines, "|") != "escalates: reads a, b|source: c" {
		t.Errorf("escalates: %q %v", class, lines)
	}
	for verdict, want := range map[string]string{reads: "reads", "refused": "refused", exempt: "exempt"} {
		if _, c := TaintTag(plan.NodeTaint{Verdict: verdict, Sources: []string{"a"}}); c != want {
			t.Errorf("%s: class %q want %q", verdict, c, want)
		}
	}
	if lines, c := TaintTag(plan.NodeTaint{Verdict: noSrc, Declares: []string{"c"}}); c != "source" || len(lines) != 1 {
		t.Errorf("source only: %q %v", c, lines)
	}
	if lines, c := TaintTag(plan.NodeTaint{Verdict: noSrc}); c != "" || len(lines) != 0 {
		t.Errorf("neither: %q %v", c, lines)
	}
	r := taintReport(map[string]plan.NodeTaint{
		"src":  {Verdict: noSrc, Declares: []string{"inbound-email"}},
		"pay":  {Verdict: escal, Sources: []string{"inbound-email"}, Path: []string{"src", "pay"}},
		"look": {Verdict: noSrc},
	}, effect, "src", "pay", "look")
	r.Topology.Entry = "src"
	r.Topology.Edges = []plan.TopologyEdge{{From: "src", To: "pay"}, {From: "pay", To: "look"}}
	g := configGraph(r)
	for _, want := range []string{`class="tring tr-source"`, `class="tring tr-escalates"`, `escalates: reads inbound-email`, `source: inbound-email`, `Taint: escalates, reads inbound-email via src &gt; pay`} {
		if !strings.Contains(g.SVG, want) {
			t.Errorf("the drawing lacks %q", want)
		}
	}
	if strings.Count(g.SVG, `class="tring`) != 2 {
		t.Errorf("a node with no taint has a ring")
	}
	// Unknown nodes carry no tag.
	unk := plan.ConfigReport{Typed: false, Topology: r.Topology, Taint: map[string]plan.NodeTaint{"src": {Verdict: "unknown"}, "pay": {Verdict: "unknown"}, "look": {Verdict: "unknown"}}}
	if g := configGraph(unk); strings.Contains(g.SVG, "tring") || strings.Contains(g.SVG, "ntaint") {
		t.Error("an unknown report is tagged")
	}
}

// Two tag lines of a node are drawn on lines of their own, below the box.
func TestTaintTagLinesAreSpaced(t *testing.T) {
	r := taintReport(map[string]plan.NodeTaint{
		"src": {Verdict: escal, Sources: []string{"a"}, Declares: []string{"b"}}, "pay": {Verdict: noSrc}, "look": {Verdict: noSrc},
	}, effect, "src", "pay", "look")
	r.Topology.Entry = "src"
	svg := configGraph(r).SVG
	ys := regexp.MustCompile(`class="ntaint [^"]*" x="\d+" y="(\d+)"`).FindAllStringSubmatch(svg, -1)
	if len(ys) != 2 || ys[0][1] == ys[1][1] {
		t.Errorf("tag lines at %v", ys)
	}
}
