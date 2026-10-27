package runview

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bide-ai/bide/internal/term"
	"github.com/bide-ai/bide/plan"
)

func TestTaintView_Known(t *testing.T) {
	r := taintReport(map[string]plan.NodeTaint{
		"src":  {Verdict: noSrc, Declares: []string{"inbound-email"}, Reason: "caveat"},
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
		"src: no declared source before it; source: inbound-email",
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
