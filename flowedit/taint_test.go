package flowedit

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/plan"
)

// The taint view of the page (taint.go): a typed editor knows the declarations and the policy, a structural one
// states only what the config declares. The page draws what these tests read.

const taintFlow = `version: 1
flow: support
entry: fetch
nodes:
  - {name: fetch, block: fetch}
  - {name: triage, block: triage, safety: readonly}
  - {name: refund, block: refund}
  - {name: notify, block: notify, safety: readonly}
wiring:
  - edge: [fetch, triage]
  - edge: [triage, refund]
  - edge: [refund, notify]
`

func taintRegistry(t *testing.T, refundOpts ...plan.NodeOption) *plan.Registry {
	t.Helper()
	reg := plan.NewRegistry()
	order := func(_ context.Context, o tOrder) (tOrder, error) { return o, nil }
	for _, err := range []error{
		reg.RegisterStep("fetch", order, plan.Untrusted("inbound-email")),
		reg.RegisterStep("plainfetch", order),
		reg.RegisterStep("triage", order, plan.ReadOnly()),
		reg.RegisterStep("refund", order, refundOpts...),
		reg.RegisterStep("notify", order, plan.ReadOnly()),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

func taintServer(t *testing.T, text string, opts ...Option) *Server {
	t.Helper()
	p := filepath.Join(t.TempDir(), "support.yaml")
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := newServer(t, p, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func viewNode(v *View, name string) Node {
	for _, n := range v.Nodes {
		if n.Name == name {
			return n
		}
	}
	return Node{}
}

func TestTaintView_TypedWithAPolicy(t *testing.T) {
	gate := agent.TaintPolicy{Gate: agent.SingleApproval()}
	for name, opts := range map[string][]Option{
		"registry": {WithRegistry(taintRegistry(t)), WithTaintPolicy(gate)},
		"description": {WithBlocks(func() plan.RegistryDescription {
			d := taintRegistry(t).Describe()
			d.TaintPolicy, _ = plan.DescribeTaintPolicy(gate)
			return d
		}())},
	} {
		v := taintServer(t, taintFlow, opts...).view([]byte(taintFlow))
		if v.Errors != 0 {
			t.Fatalf("%s: %+v", name, v.Problems)
		}
		if v.Taint == nil || !v.Taint.Known {
			t.Fatalf("%s: summary %+v", name, v.Taint)
		}
		f := viewNode(v, "fetch").Taint
		if f == nil || len(f.Declares) != 1 || f.Declares[0] != "inbound-email" || f.Text != "no declared source before it; source: inbound-email; it is itself a source, and its own read is not gated; split the block" {
			t.Errorf("%s: fetch %+v", name, f)
		}
		r := viewNode(v, "refund").Taint
		if r == nil || r.Verdict != "escalates" || len(r.Sources) != 1 || strings.Join(r.Path, ">") != "fetch>triage>refund" || r.Text != "escalates, reads inbound-email via fetch > triage > refund" {
			t.Errorf("%s: refund %+v", name, r)
		}
		if n := viewNode(v, "triage").Taint; n == nil || n.Verdict != "reads untrusted" {
			t.Errorf("%s: triage %+v", name, n)
		}
		// Exposed are the nodes that can have a side effect; a read-only node that reads is counted, not exposed.
		if strings.Join(v.Taint.Exposed, ",") != "refund" || len(v.Taint.Unknown) != 0 || strings.Join(v.Taint.Sources, ",") != "fetch" {
			t.Errorf("%s: exposed %v unknown %v sources %v", name, v.Taint.Exposed, v.Taint.Unknown, v.Taint.Sources)
		}
		if v.Taint.Line != "taint: 3 nodes may read untrusted data (1 escalates)" || len(v.Taint.Counts) != 2 || v.Taint.Caveat == "" {
			t.Errorf("%s: line %q counts %v", name, v.Taint.Line, v.Taint.Counts)
		}
		if name == "description" && !strings.Contains(v.Taint.Basis, "description") {
			t.Errorf("a description result is not labelled: %q", v.Taint.Basis)
		}
		if name == "registry" && v.Taint.Basis != "" {
			t.Errorf("basis %q", v.Taint.Basis)
		}
		if !strings.Contains(v.Graph, "tr-escalates") || !strings.Contains(v.Graph, "escalates: reads inbound-email") {
			t.Errorf("%s: the drawing has no tag", name)
		}
	}
}

// Without the policy the editor says so: it never says a node escalates.
func TestTaintView_PolicyNotGiven(t *testing.T) {
	v := taintServer(t, taintFlow, WithRegistry(taintRegistry(t))).view([]byte(taintFlow))
	r := viewNode(v, "refund").Taint
	if r == nil || r.Verdict != "reads untrusted" || !strings.Contains(r.Reason, "taint policy not given") {
		t.Fatalf("refund %+v", r)
	}
	if strings.Join(v.Taint.Unknown, ",") != "refund" || strings.Contains(v.Taint.Line, "escalates") {
		t.Errorf("unknown %v line %q", v.Taint.Unknown, v.Taint.Line)
	}
	// The zero policy states there is none.
	v = taintServer(t, taintFlow, WithRegistry(taintRegistry(t)), WithTaintPolicy(agent.TaintPolicy{})).view([]byte(taintFlow))
	if r := viewNode(v, "refund").Taint; r == nil || !strings.HasPrefix(r.Reason, "no taint policy") {
		t.Errorf("zero policy: %+v", r)
	}
}

func TestTaintView_ExemptAndRefuse(t *testing.T) {
	v := taintServer(t, taintFlow, WithRegistry(taintRegistry(t, plan.TaintExempt("fixed amount"))), WithTaintPolicy(agent.TaintPolicy{Gate: agent.SingleApproval()})).view([]byte(taintFlow))
	if r := viewNode(v, "refund").Taint; r == nil || r.Verdict != "exempt" || r.Exempt != "fixed amount" || !strings.Contains(r.Text, "(exempt: fixed amount)") {
		t.Errorf("exempt %+v", r)
	}
	v = taintServer(t, taintFlow, WithRegistry(taintRegistry(t)), WithTaintPolicy(agent.TaintPolicy{Refuse: true})).view([]byte(taintFlow))
	if r := viewNode(v, "refund").Taint; r == nil || r.Verdict != "refused" {
		t.Errorf("refuse %+v", r)
	}
}

// A structural editor knows no blocks: one line, no verdict on any node, and only the sources the config states.
func TestTaintView_Structural(t *testing.T) {
	text := strings.Replace(taintFlow, "{name: fetch, block: fetch}", "{name: fetch, block: fetch, untrusted: crm}", 1)
	v := taintServer(t, text).view([]byte(text))
	if v.Taint == nil || v.Taint.Known || v.Taint.Line != "taint: not known without -blocks" || len(v.Taint.Exposed) != 0 {
		t.Fatalf("summary %+v", v.Taint)
	}
	if f := viewNode(v, "fetch").Taint; f == nil || f.Verdict != "unknown" || f.Text != "unknown; the config declares source crm" {
		t.Errorf("fetch %+v", f)
	}
	if n := viewNode(v, "refund").Taint; n != nil {
		t.Errorf("a structural node has a verdict: %+v", n)
	}
	if strings.Contains(v.Graph, "tring") {
		t.Error("a structural drawing is tagged")
	}
}

// A typed config with an error has no taint view at all.
func TestTaintView_ErrorsHaveNone(t *testing.T) {
	text := strings.Replace(taintFlow, "block: refund", "block: nowhere", 1)
	v := taintServer(t, taintFlow, WithRegistry(taintRegistry(t))).view([]byte(text))
	if v.Errors == 0 || v.Taint != nil {
		t.Errorf("errors %d, taint %+v", v.Errors, v.Taint)
	}
}

// The weakening banner: an edit that makes the side-effecting refund newly read a source when nothing gates it.
func TestTaintView_TheBannerForAnExposingEdit(t *testing.T) {
	// In the baseline fetch runs last, so nothing before it reads its source; the edit moves it first.
	baseline := strings.NewReplacer("entry: fetch", "entry: triage", "  - edge: [fetch, triage]\n", "", "  - edge: [refund, notify]\n", "  - edge: [refund, notify]\n  - edge: [notify, fetch]\n").Replace(taintFlow)
	for name, c := range map[string]struct {
		policy   agent.TaintPolicy
		word     string
		weakened bool
	}{
		"no policy": {agent.TaintPolicy{}, "UNTRUSTED EXPOSURE WIDENED", true},
		"gate":      {agent.TaintPolicy{Gate: agent.SingleApproval()}, "", false},
		"refuse":    {agent.TaintPolicy{Refuse: true}, "", false},
	} {
		p := filepath.Join(t.TempDir(), "support.yaml")
		if err := os.WriteFile(p, []byte(baseline), 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := newServer(t, p, WithRegistry(taintRegistry(t)), WithTaintPolicy(c.policy))
		if err != nil {
			t.Fatal(err)
		}
		w := s.compare([]byte(taintFlow))
		if (len(w.Weakened) > 0) != c.weakened || w.Word != c.word && c.weakened {
			t.Errorf("%s: banner %q weakened %v", name, w.Word, w.Weakened)
		}
		if !c.weakened && len(w.Changes) == 0 {
			t.Errorf("%s: the covered exposure is not listed", name)
		}
	}
	// The policy is not given: the comparison is not certified.
	p := filepath.Join(t.TempDir(), "support.yaml")
	_ = os.WriteFile(p, []byte(baseline), 0o600)
	s, err := newServer(t, p, WithRegistry(taintRegistry(t)))
	if err != nil {
		t.Fatal(err)
	}
	if w := s.compare([]byte(taintFlow)); !strings.Contains(w.NotCertified, "taint could not be compared") {
		t.Errorf("not certified: %q", w.NotCertified)
	}
}

// No state of the taint view is told by a claim of safety.
func TestTaintView_NoClaimWords(t *testing.T) {
	claims := []string{"safe", "clean", "no taint", "protected"}
	var all []string
	for _, opts := range [][]Option{
		{WithRegistry(taintRegistry(t)), WithTaintPolicy(agent.TaintPolicy{Gate: agent.SingleApproval()})},
		{WithRegistry(taintRegistry(t))},
		{WithRegistry(taintRegistry(t, plan.TaintExempt("fixed amount"))), WithTaintPolicy(agent.TaintPolicy{Gate: agent.SingleApproval()})},
		{WithRegistry(taintRegistry(t)), WithTaintPolicy(agent.TaintPolicy{Refuse: true})},
		nil,
	} {
		v := taintServer(t, taintFlow, opts...).view([]byte(taintFlow))
		b, _ := json.Marshal(struct {
			S *taintSummary `json:"s"`
			N []Node        `json:"n"`
		}{v.Taint, v.Nodes})
		all = append(all, string(b))
		for _, n := range v.Nodes {
			if n.Taint != nil {
				all = append(all, n.Taint.Text)
			}
		}
		// The drawing's tags and titles, text of the SVG only.
		all = append(all, v.Graph)
	}
	// The page: its markup, script and styles. The one pre-existing use of the word is the safety class
	// "idempotent: safe to repeat", which is about a resume and not about taint.
	for _, f := range []string{"page/index.html", "page/editor.js", "page/editor.css"} {
		b, err := fs.ReadFile(pageFS, f)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, strings.ReplaceAll(string(b), "idempotent: safe to repeat", ""))
	}
	for _, text := range all {
		low := strings.ToLower(text)
		for _, c := range claims {
			if loc := claimWord(c).FindStringIndex(low); loc != nil {
				lo, hi := max(0, loc[0]-40), min(len(low), loc[1]+40)
				t.Errorf("the taint view says %q: ...%s...", c, low[lo:hi])
			}
		}
	}
}

// claimWord matches the claim as a word of its own: "safety" and "cleanup" are not claims of safety.
func claimWord(c string) *regexp.Regexp { return regexp.MustCompile(`\b` + regexp.QuoteMeta(c) + `\b`) }

// The script reads the fields the view carries, so a field cannot be renamed on one side only.
func TestTaintView_PageContract(t *testing.T) {
	js, _ := fs.ReadFile(pageFS, "page/editor.js")
	for _, k := range []string{"n.taint", "v.taint", "t.verdict", "t.sources", "t.path", "t.declares", "t.exempt", "t.reason", "s.exposed", "s.unknown", "s.sources", "s.counts", "s.caveat", "s.known", "s.line", "s.basis", "n.taint.text"} {
		if !strings.Contains(string(js), k) {
			t.Errorf("the script no longer reads %s: update this list with it", k)
		}
	}
	html, _ := fs.ReadFile(pageFS, "page/index.html")
	for _, id := range []string{`id="taint-panel"`, `id="taint-body"`, `id="h-taint"`, `id="skip-taint"`} {
		if !strings.Contains(string(html), id) {
			t.Errorf("the page lacks %s", id)
		}
	}
	// The view's JSON keys.
	v := taintServer(t, taintFlow, WithRegistry(taintRegistry(t)), WithTaintPolicy(agent.TaintPolicy{Gate: agent.SingleApproval()})).view([]byte(taintFlow))
	b, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	sum := m["taint"].(map[string]any)
	for _, k := range []string{"known", "line", "counts", "exposed", "sources", "caveat"} {
		if _, ok := sum[k]; !ok {
			t.Errorf("the summary has no %q", k)
		}
	}
	for _, x := range m["nodes"].([]any) {
		n := x.(map[string]any)
		if n["name"] == "refund" {
			nt := n["taint"].(map[string]any)
			for _, k := range []string{"verdict", "sources", "path", "text"} {
				if _, ok := nt[k]; !ok {
					t.Errorf("a node's taint has no %q: %v", k, nt)
				}
			}
		}
	}
}

// WithTaintPolicy overrides the policy of a description, as InspectTaintPolicy overrides a description's own.
func TestTaintView_TheOptionOverridesTheDescriptionsPolicy(t *testing.T) {
	d := taintRegistry(t).Describe()
	d.TaintPolicy, _ = plan.DescribeTaintPolicy(agent.TaintPolicy{Gate: agent.SingleApproval()})
	v := taintServer(t, taintFlow, WithBlocks(d)).view([]byte(taintFlow))
	if r := viewNode(v, "refund").Taint; r == nil || r.Verdict != "escalates" {
		t.Fatalf("the description's policy: %+v", r)
	}
	v = taintServer(t, taintFlow, WithBlocks(d), WithTaintPolicy(agent.TaintPolicy{Refuse: true})).view([]byte(taintFlow))
	if r := viewNode(v, "refund").Taint; r == nil || r.Verdict != "refused" {
		t.Errorf("the option over the description's: %+v", r)
	}
}
