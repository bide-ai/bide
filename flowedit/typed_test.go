package flowedit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/plan"
)

type tOrder struct{ ID string }

// typedRegistry registers what testFlow uses (validate, charge, receipt) and blocks the palette
// lists that it does not: a gated and an ungated order step, a string step, and an int step that
// fits nothing in the flow.
func typedRegistry(t *testing.T) *plan.Registry {
	t.Helper()
	reg := plan.NewRegistry()
	order := func(_ context.Context, o tOrder) (tOrder, error) { return o, nil }
	for _, err := range []error{
		reg.RegisterStep("validate", order, plan.ReadOnly()),
		reg.RegisterStep("charge", order),
		reg.RegisterStep("receipt", func(_ context.Context, o tOrder) (string, error) { return o.ID, nil }, plan.Idempotent()),
		reg.RegisterStep("gated", order, plan.Approval(&agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "fin"}})),
		reg.RegisterStep("plain", order),
		reg.RegisterStep("shout", func(_ context.Context, s string) (string, error) { return strings.ToUpper(s), nil }),
		reg.RegisterStep("count", func(_ context.Context, n int) (int, error) { return n + 1, nil }),
		reg.RegisterPredicate("large", func(o tOrder) bool { return len(o.ID) > 3 }),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

// typedServers makes the two typed editors for one config (against the Registry, and against its
// description) and the structural one, over separate copies of the file.
func typedServers(t *testing.T, name, text string) map[string]*Server {
	t.Helper()
	write := func() string {
		p := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	reg := typedRegistry(t)
	out := map[string]*Server{}
	for kind, opt := range map[string]Option{
		"registry":    WithRegistry(reg),
		"description": WithBlocks(reg.Describe()),
		"structural":  func(*options) {},
	} {
		s, err := newServer(t, write(), opt)
		if err != nil {
			t.Fatal(err)
		}
		out[kind] = s
	}
	return out
}

// Typed mode adds what the program knows: the label, the digest, the palette, each node's types. An
// unused palette block is no problem, so a clean config stays clean beside a long palette.
func TestTypedViewCarriesWhatTheProgramKnows(t *testing.T) {
	servers := typedServers(t, "refund.yaml", testFlow)
	var digests []string
	for _, kind := range []string{"registry", "description"} {
		s := servers[kind]
		d, err := s.Doc()
		if err != nil {
			t.Fatal(err)
		}
		v := d.View
		if v.Typed == nil || v.Typed.Source != kind || v.Typed.Label != v.Mode || v.Typed.Digest == "" {
			t.Fatalf("%s: typed %+v mode %q", kind, v.Typed, v.Mode)
		}
		if kind == "description" && v.Mode != runview.DescriptionNote || kind == "registry" && strings.Contains(v.Mode, "description") {
			t.Errorf("%s: mode %q", kind, v.Mode)
		}
		if v.Errors != 0 || v.Verdict.Class == "bad" {
			t.Errorf("%s: errors %d (%+v), verdict %+v: an unused palette block must not be a problem", kind, v.Errors, v.Problems, v.Verdict)
		}
		used := map[string]bool{}
		for _, b := range v.Typed.Palette {
			used[b.Name] = b.Used
		}
		if len(v.Typed.Palette) != 7 || !used["validate"] || !used["charge"] || !used["receipt"] || used["gated"] || used["shout"] || used["count"] {
			t.Errorf("%s: palette %+v", kind, v.Typed.Palette)
		}
		for _, b := range v.Typed.Palette {
			if b.Name == "gated" && (b.Gate != "2 of 2: ops, fin" || b.Safety != "side_effect" || b.Kind != "step") {
				t.Errorf("%s: gated block %+v", kind, b)
			}
		}
		if v.Nodes[0].In == "" || v.Nodes[0].Out != v.Nodes[1].In || v.Nodes[1].Out != v.Nodes[2].In {
			t.Errorf("%s: node types %+v", kind, v.Nodes)
		}
		digests = append(digests, v.Typed.Digest)
	}
	if digests[0] != digests[1] {
		t.Errorf("the digest from the Registry %s and from its description %s differ", digests[0], digests[1])
	}
	// Structural: nothing typed, the structural label.
	d, _ := servers["structural"].Doc()
	if d.Typed != nil || d.Mode != ModeLine || d.Nodes[0].Out != "" {
		t.Errorf("structural: typed %+v mode %q", d.Typed, d.Mode)
	}
}

const gateSwapFlow = `version: 1
flow: pay
entry: validate
nodes:
  - {name: validate, block: validate, safety: readonly}
  - {name: pay, block: gated}
  - {name: receipt, block: receipt}
wiring:
  - edge: [validate, pay]
  - edge: [pay, receipt]
`

// G4: a node's block changes to one whose Go registration declares no gate. Structurally the gate
// the old block declared is invisible and the change is only "possibly" a weakening; typed, the gate
// is known on both sides and the change is judged by what it shows (and the swap the other way is
// stronger). A save of it needs confirming in each mode.
func TestTypedEditorKnowsTheGateABlockDeclares(t *testing.T) {
	servers := typedServers(t, "pay.yaml", gateSwapFlow)
	for kind, s := range servers {
		h := s.Handler()
		w := call(h, s, "/api/apply", applyReq(gateSwapFlow, Operation{Op: "set_block", Node: "pay", Value: "plain"}), nil)
		var e Edited
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || w.Code != 200 || !e.Changed {
			t.Fatalf("%s: %d %v %s", kind, w.Code, err, w.Body)
		}
		if e.Weakening == nil || len(e.Weakening.Weakened) == 0 {
			t.Fatalf("%s: weakening %+v", kind, e.Weakening)
		}
		got := e.Weakening.Weakened[0] // the gate of the node itself comes first
		if kind == "structural" {
			if !strings.Contains(got, "not visible without the plan.Registry") {
				t.Errorf("structural: %q", got)
			}
		} else if !strings.Contains(got, "pay: approval 2 of 2: ops, fin -> none") || strings.Contains(got, "not visible") {
			t.Errorf("%s: %q", kind, got)
		}
		if _, err := s.Save([]byte(e.Text), hashOf([]byte(gateSwapFlow)), nil); err == nil {
			t.Errorf("%s: a save that removes a gate went through unconfirmed", kind)
		}
	}
	// A block that declares no gate, changed to a gated one, is not a weakening in typed mode.
	plainFlow := strings.Replace(gateSwapFlow, "block: gated", "block: plain", 1)
	for kind, s := range typedServers(t, "pay.yaml", plainFlow) {
		w := call(s.Handler(), s, "/api/apply", applyReq(plainFlow, Operation{Op: "set_block", Node: "pay", Value: "gated"}), nil)
		var e Edited
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || !e.Changed {
			t.Fatalf("%s: %d %v %s", kind, w.Code, err, w.Body)
		}
		if kind != "structural" && (len(e.Weakening.Weakened) != 0 || len(e.Weakening.Strengthened) != 1) {
			t.Errorf("%s: weakened %q strengthened %q", kind, e.Weakening.Weakened, e.Weakening.Strengthened)
		}
		// Saving it: typed, the gate is known to be added, so nothing needs confirming; structurally the
		// editor cannot tell and asks.
		_, err := s.Save([]byte(e.Text), hashOf([]byte(plainFlow)), nil)
		if (kind == "structural") != (err != nil) {
			t.Errorf("%s: save of a block changed to a gated one: %v", kind, err)
		}
	}
}

// A connection that does not fit is refused by the server, with plan's reason, and nothing changes;
// the structural editor takes the same operation and shows the result as a problem later. An
// operation that fits goes through, and so does one on a text that already had the problem.
func TestTypedEditorRefusesWhatDoesNotFit(t *testing.T) {
	servers := typedServers(t, "refund.yaml", testFlow)
	misfit := Operation{Op: "set_block", Node: "charge", Value: "count"} // validate gives an order; count takes an int
	for _, kind := range []string{"registry", "description"} {
		s := servers[kind]
		h := s.Handler()
		w := call(h, s, "/api/apply", applyReq(testFlow, misfit), nil)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"refused":"the types do not fit: `) || !strings.Contains(w.Body.String(), "connects") || strings.Contains(w.Body.String(), `"text"`) {
			t.Errorf("%s: %d %s", kind, w.Code, w.Body)
		}
		// A block of the same types fits.
		w = call(h, s, "/api/apply", applyReq(testFlow, Operation{Op: "set_block", Node: "charge", Value: "plain"}), nil)
		var e Edited
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || w.Code != 200 || !e.Changed {
			t.Errorf("%s: a fitting block: %d %s", kind, w.Code, w.Body)
		}
		// A text that already has a type problem: an unrelated operation is not refused for it.
		bad := strings.Replace(testFlow, "block: charge", "block: count", 1)
		w = call(h, s, "/api/apply", applyReq(bad, Operation{Op: "set_safety", Node: "validate", Value: "side_effect"}), nil)
		e = Edited{}
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || w.Code != 200 || !e.Changed || e.Errors == 0 {
			t.Errorf("%s: an unrelated operation on a text with a problem: %d %s", kind, w.Code, w.Body)
		}
	}
	s := servers["structural"]
	w := call(s.Handler(), s, "/api/apply", applyReq(testFlow, misfit), nil)
	var e Edited
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || !e.Changed {
		t.Errorf("structural: %d %s", w.Code, w.Body)
	}
}

// The refusal reads plan's messages, so a change of their wording must fail here, not silently turn
// the refusal off. Each of the four kinds of misfit is built from real configs.
func TestTypeProblemMatchesWhatPlanSays(t *testing.T) {
	reg := plan.NewRegistry()
	for _, err := range []error{
		reg.RegisterStep("order", func(_ context.Context, o tOrder) (tOrder, error) { return o, nil }),
		reg.RegisterStep("num", func(_ context.Context, n int) (int, error) { return n, nil }),
		reg.RegisterStep("text", func(_ context.Context, o tOrder) (string, error) { return o.ID, nil }),
		reg.RegisterPredicate("isbig", func(n int) bool { return n > 1 }),
		reg.RegisterJoin2("both", func(_ context.Context, a tOrder, b string) (string, error) { return b, nil }),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]string{
		"edge": `{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"order"},{"name":"b","block":"num"},{"name":"c","block":"text"},{"name":"d","block":"num"}],
		 "wiring":[{"edge":["a","b"]},{"edge":["a","c"]}]}`,
		"predicate": `{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"order"},{"name":"b","block":"order"},{"name":"c","block":"order"}],
		 "wiring":[{"switch":"a","when":[{"pred":"isbig","to":"b"}],"else":"c"}]}`,
		"join": `{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"order"},{"name":"b","block":"order"},{"name":"c","block":"order"}],
		 "wiring":[{"edge":["a","b"]},{"edge":["a","c"]},{"join":"j","inputs":["b","c"],"merge":"both"}]}`,
		"terminal": `{"version":1,"flow":"f","entry":"a","nodes":[{"name":"a","block":"order"},{"name":"b","block":"text"},{"name":"c","block":"order"}],
		 "wiring":[{"edge":["a","b"]},{"edge":["a","c"]}]}`,
	}
	for name, cfg := range cases {
		r := plan.InspectConfig([]byte(cfg), reg, plan.AllowUnusedBlocks())
		found := false
		for _, p := range r.Problems {
			found = found || typeProblem(p)
		}
		if !found {
			t.Errorf("%s: no problem matched: %+v", name, r.Problems)
		}
	}
	// Not every error is a misfit.
	for _, p := range []plan.ConfigProblem{
		{Severity: "error", Message: `node "a" references unknown block "x"`},
		{Severity: "warning", Message: "edge connects a to b (types must match exactly)"},
	} {
		if typeProblem(p) {
			t.Errorf("%+v matched", p)
		}
	}
}

func TestTypedOptionsRefuseWhatCannotBeTyped(t *testing.T) {
	p := filepath.Join(t.TempDir(), "refund.yaml")
	if err := os.WriteFile(p, []byte(testFlow), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := typedRegistry(t)
	if _, err := New(p, WithRegistry(reg), WithBlocks(reg.Describe())); err == nil || !strings.Contains(err.Error(), "alternatives") {
		t.Errorf("both: %v", err)
	}
	bad := plan.NewRegistry()
	_ = bad.RegisterStep("x", func(_ context.Context, n int) (int, error) { return n, nil })
	_ = bad.RegisterStep("x", func(_ context.Context, n int) (int, error) { return n, nil })
	if _, err := New(p, WithRegistry(bad)); err == nil || !strings.Contains(err.Error(), "duplicate block registration") {
		t.Errorf("a Registry with errors: %v", err)
	}
	d := reg.Describe()
	d.Blocks[0].Kind = "wizard"
	if _, err := New(p, WithBlocks(d)); err == nil || !strings.Contains(err.Error(), "kind") {
		t.Errorf("an invalid description: %v", err)
	}
	// The description is copied: a host that changes its own copy later does not change the editor.
	d = reg.Describe()
	s, err := newServer(t, p, WithBlocks(d))
	if err != nil {
		t.Fatal(err)
	}
	d.Blocks[0].Name = "hijacked"
	d.Blocks = d.Blocks[:1]
	doc, err := s.Doc()
	if err != nil || len(doc.Typed.Palette) != 7 || doc.Typed.Palette[0].Name == "hijacked" {
		t.Errorf("palette %+v (%v)", doc.Typed.Palette, err)
	}
}
