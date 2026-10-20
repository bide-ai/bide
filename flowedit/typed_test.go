package flowedit

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
		reg.RegisterStep("plain", order, plan.Idempotent()),
		reg.RegisterStep("shout", func(_ context.Context, s string) (string, error) { return strings.ToUpper(s), nil }, plan.Idempotent()),
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

// A node's block changes to one whose Go registration declares no gate. The typed editor adds what
// the program knows (the gate the old block declares, written as the viewer writes it), and keeps
// what the structural editor says, that the new block's gate may be missing: it is never quieter. The
// swap the other way shows the gate that was added and still needs confirming, as it does
// structurally, because the baseline was written for a program the editor does not know.
func TestTypedEditorAddsToWhatTheStructuralEditorSays(t *testing.T) {
	servers := typedServers(t, "pay.yaml", gateSwapFlow)
	structural := servers["structural"]
	for kind, s := range servers {
		w := call(s.Handler(), s, "/api/apply", applyReq(gateSwapFlow, Operation{Op: "set_block", Node: "pay", Value: "plain"}), nil)
		var e Edited
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || w.Code != 200 || !e.Changed {
			t.Fatalf("%s: %d %v %s", kind, w.Code, err, w.Body)
		}
		all := strings.Join(e.Weakening.Weakened, "\n")
		if !strings.Contains(all, "not visible without the plan.Registry") {
			t.Errorf("%s: the structural 'possibly' is gone: %q", kind, all)
		}
		if kind != "structural" && !strings.Contains(all, "pay: approval 2 of 2: ops, fin -> none") {
			t.Errorf("%s: the typed gate line is missing: %q", kind, all)
		}
		if _, err := s.Save([]byte(e.Text), hashOf([]byte(gateSwapFlow)), nil); err == nil {
			t.Errorf("%s: a save that removes a gate went through unconfirmed", kind)
		}
	}
	_ = structural
	plainFlow := strings.Replace(gateSwapFlow, "block: gated", "block: plain", 1)
	for kind, s := range typedServers(t, "pay.yaml", plainFlow) {
		w := call(s.Handler(), s, "/api/apply", applyReq(plainFlow, Operation{Op: "set_block", Node: "pay", Value: "gated"}), nil)
		var e Edited
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || !e.Changed {
			t.Fatalf("%s: %d %v %s", kind, w.Code, err, w.Body)
		}
		if kind != "structural" && !strings.Contains(strings.Join(e.Weakening.Strengthened, "\n"), "pay: approval none -> 2 of 2: ops, fin") {
			t.Errorf("%s: the typed gate is not shown as added: %q", kind, e.Weakening.Strengthened)
		}
		if _, err := s.Save([]byte(e.Text), hashOf([]byte(plainFlow)), nil); err == nil {
			t.Errorf("%s: the save needs no confirmation, and the structural editor's does", kind)
		}
	}
}

// The program changed since the baseline: the old block's gate was removed in Go, and the config
// swaps the block. Read with the program as it is now, both blocks are gate-less and the swap looks
// harmless; the structural editor says the gate may be gone, and the typed editor must not be quieter.
// The baseline's program is not known, so the editor reports what either comparison reports.
func TestTypedEditorIsNotQuieterWhenTheProgramChangedSinceTheBaseline(t *testing.T) {
	reg := plan.NewRegistry() // the program now: "gated" has lost its gate
	order := func(_ context.Context, o tOrder) (tOrder, error) { return o, nil }
	for _, err := range []error{
		reg.RegisterStep("validate", order, plan.ReadOnly()),
		reg.RegisterStep("gated", order),
		reg.RegisterStep("plain", order, plan.Idempotent()),
		reg.RegisterStep("receipt", func(_ context.Context, o tOrder) (string, error) { return o.ID, nil }, plan.Idempotent()),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	text := strings.Replace(gateSwapFlow, "block: gated", "block: plain", 1)
	for kind, opt := range map[string]Option{"registry": WithRegistry(reg), "description": WithBlocks(reg.Describe()), "structural": func(*options) {}} {
		p := filepath.Join(t.TempDir(), "pay.yaml")
		if err := os.WriteFile(p, []byte(gateSwapFlow), 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := newServer(t, p, opt)
		if err != nil {
			t.Fatal(err)
		}
		w := s.compare([]byte(text))
		if len(w.Weakened) == 0 || !strings.Contains(strings.Join(w.Weakened, "\n"), "not visible without") {
			t.Errorf("%s: weakened %q", kind, w.Weakened)
		}
		if c := s.confirmations([]byte(text)); len(c) == 0 {
			t.Errorf("%s: the save needs no confirmation", kind)
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

// A typed editor is never less strict than a structural one. For each edit of a baseline, the typed
// verdict keeps every weakening the structural one reports, word for word; and when the text has errors against the blocks (a block the program lacks, a type
// that does not fit), the comparison is the structural one, in full, named not certified, and a save
// of it needs the confirmation a weakening needs.
func TestTypedEditorIsNeverLessStrictThanStructural(t *testing.T) {
	reg := typedRegistry(t)
	baseline := testFlow
	edits := map[string]func(string) string{
		"gate dropped": func(s string) string {
			return strings.Replace(s, "    approval: {need: 2, approvers: [ops, finance, risk], exclude_principal: true}\n", "", 1)
		},
		"need lowered":     func(s string) string { return strings.Replace(s, "need: 2", "need: 1", 1) },
		"block changed":    func(s string) string { return strings.Replace(s, "block: charge", "block: plain", 1) },
		"to a gated block": func(s string) string { return strings.Replace(s, "block: charge", "block: gated", 1) },
		"unknown block":    func(s string) string { return strings.Replace(s, "block: charge", "block: nowhere", 1) },
		"misfit block":     func(s string) string { return strings.Replace(s, "block: charge", "block: count", 1) },
		"safety lowered":   func(s string) string { return strings.Replace(s, "safety: readonly", "safety: side_effect", 1) },
		"effect added": func(s string) string {
			return strings.Replace(s, "  - {name: receipt", "  - {name: audit, block: plain}\n  - {name: receipt", 1)
		},
		"unchanged": func(s string) string { return s },
	}
	for kind, opt := range map[string]Option{"registry": WithRegistry(reg), "description": WithBlocks(reg.Describe())} {
		p := filepath.Join(t.TempDir(), "refund.yaml")
		if err := os.WriteFile(p, []byte(baseline), 0o600); err != nil {
			t.Fatal(err)
		}
		typed, err := newServer(t, p, opt)
		if err != nil {
			t.Fatal(err)
		}
		structural, err := newServer(t, p)
		if err != nil {
			t.Fatal(err)
		}
		for name, edit := range edits {
			text := []byte(edit(baseline))
			wt, ws := typed.compare(text), structural.compare(text)
			if wt.CompareError != ws.CompareError {
				t.Errorf("%s/%s: compare errors %q vs %q", kind, name, wt.CompareError, ws.CompareError)
			}
			if wt.NotCertified != "" {
				if !slices.Equal(wt.Weakened, ws.Weakened) {
					t.Errorf("%s/%s: not certified, weakened %q vs the structural %q", kind, name, wt.Weakened, ws.Weakened)
				}
				if c := typed.confirmations(text); len(c) == 0 || !strings.Contains(strings.Join(c, "\n"), "not certified") {
					t.Errorf("%s/%s: a save of an uncertified comparison needs no confirmation: %q", kind, name, c)
				}
				continue
			}
			for _, x := range ws.Weakened {
				if !slices.Contains(wt.Weakened, x) {
					t.Errorf("%s/%s: the structural weakening %q vanished: %q", kind, name, x, wt.Weakened)
				}
			}
			if len(structural.confirmations(text)) > 0 && len(typed.confirmations(text)) == 0 {
				t.Errorf("%s/%s: the typed save needs no confirmation, the structural one does", kind, name)
			}
		}
	}
	// The two cases the test is about are really in it: an uncertified comparison and a certified one.
	ty := typedServers(t, "refund.yaml", baseline)["registry"]
	if w := ty.compare([]byte(edits["unknown block"](baseline))); w.NotCertified == "" {
		t.Errorf("an unknown block is certified: %+v", w)
	}
	gs := typedServers(t, "pay.yaml", gateSwapFlow)["registry"]
	if w := gs.compare([]byte(strings.Replace(gateSwapFlow, "block: gated", "block: plain", 1))); w.NotCertified != "" || len(w.Weakened) < 2 {
		t.Errorf("a known block swap: %+v", w)
	}
}

// A type misfit the text already has is not the operation's, even when the operation renames a node
// the misfit names: the problem is compared under the rename. A rename that introduces a misfit of
// its own (none can; the types do not change) is not what this tests, and a different operation that
// does is still refused.
func TestTypedRefusalComparesProblemsUnderARename(t *testing.T) {
	bad := strings.Replace(testFlow, "block: charge", "block: count", 1) // charge takes an int; validate gives an order
	for kind, s := range typedServers(t, "refund.yaml", bad) {
		if kind == "structural" {
			continue
		}
		h := s.Handler()
		w := call(h, s, "/api/apply", applyReq(bad, Operation{Op: "rename_node", Node: "charge", Value: "bill"}), nil)
		var e Edited
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || w.Code != 200 || !e.Changed || !strings.Contains(e.Text, "name: bill") {
			t.Errorf("%s: a rename of a node in an existing misfit: %d %s", kind, w.Code, w.Body)
		}
		// A rename of another node does not matter either, and a new misfit is still refused.
		w = call(h, s, "/api/apply", applyReq(bad, Operation{Op: "rename_node", Node: "receipt", Value: "slip"}), nil)
		e = Edited{}
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || !e.Changed {
			t.Errorf("%s: a rename elsewhere: %d %s", kind, w.Code, w.Body)
		}
		w = call(h, s, "/api/apply", applyReq(testFlow, Operation{Op: "set_block", Node: "charge", Value: "count"}), nil)
		if !strings.Contains(w.Body.String(), `"refused"`) {
			t.Errorf("%s: a new misfit after the rename code: %s", kind, w.Body)
		}
	}
}

func TestTypedOptionsRefuseNilAndZero(t *testing.T) {
	p := filepath.Join(t.TempDir(), "refund.yaml")
	if err := os.WriteFile(p, []byte(testFlow), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(p, WithRegistry(nil)); err == nil || !strings.Contains(err.Error(), "WithRegistry(nil)") {
		t.Errorf("WithRegistry(nil): %v", err)
	}
	if _, err := New(p, WithBlocks(plan.RegistryDescription{})); err == nil {
		t.Error("WithBlocks of a zero description made an editor")
	}
	var nilReg *plan.Registry
	if _, err := New(p, WithRegistry(nilReg), WithBlocks(typedRegistry(t).Describe())); err == nil {
		t.Error("a nil Registry beside a description made an editor")
	}
}

// A node not wired in yet makes the whole flow fail to build (it is unreachable), and a report that
// does not build has no types; the palette is used exactly then, so each node's types come from the
// program's description of its block, whatever the rest of the config says.
func TestTypedNodesKeepTheirTypesWhileAnotherNodeIsUnwired(t *testing.T) {
	unwired := strings.Replace(testFlow, "wiring:\n", "  - {name: loose, block: count}\nwiring:\n", 1)
	for kind, s := range typedServers(t, "refund.yaml", unwired) {
		if kind == "structural" {
			continue
		}
		d, err := s.Doc()
		if err != nil {
			t.Fatal(err)
		}
		if d.Errors == 0 {
			t.Fatalf("%s: the flow with an unwired node builds", kind)
		}
		got := map[string][2]string{}
		for _, n := range d.Nodes {
			got[n.Name] = [2]string{n.In, n.Out}
		}
		order, count := reflect.TypeFor[tOrder]().PkgPath()+".tOrder", "int"
		if got["validate"] != [2]string{order, order} || got["receipt"] != [2]string{order, "string"} || got["loose"] != [2]string{count, count} {
			t.Errorf("%s: node types %v", kind, got)
		}
	}
	// A block the program does not describe has no types, and says so on the page ("types unknown").
	ghost := strings.Replace(testFlow, "block: charge", "block: nowhere", 1)
	for kind, s := range typedServers(t, "refund.yaml", ghost) {
		if kind == "structural" {
			continue
		}
		d, _ := s.Doc()
		if d.Nodes[1].In != "" || d.Nodes[1].Out != "" || d.Nodes[0].Out == "" {
			t.Errorf("%s: %+v", kind, d.Nodes)
		}
	}
}

// The page reads these keys of the typed view; a Go test decodes the shapes from the structs, so a rename
// on either side fails here and not in a browser.
func TestPageContractForTypedMode(t *testing.T) {
	s := typedServers(t, "refund.yaml", strings.Replace(testFlow, "block: charge", "block: nowhere", 1))["registry"]
	b, _ := json.Marshal(s.view([]byte(strings.Replace(testFlow, "block: charge", "block: nowhere", 1))))
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	ty, ok := m["typed"].(map[string]any)
	if !ok {
		t.Fatalf("no typed view: %s", b)
	}
	for _, k := range []string{"source", "label", "palette"} {
		if _, ok := ty[k]; !ok {
			t.Errorf("typed has no %q", k)
		}
	}
	row := ty["palette"].([]any)[0].(map[string]any)
	for _, k := range []string{"name", "kind", "in", "out", "safety", "used"} {
		if _, ok := row[k]; !ok {
			t.Errorf("a palette row has no %q", k)
		}
	}
	if wk, ok := m["weakening"].(map[string]any); !ok || wk["not_certified"] == nil {
		t.Errorf("a text with errors against the blocks has no not_certified: %v", m["weakening"])
	}
	good, _ := json.Marshal(s.view([]byte(testFlow)))
	var g map[string]any
	_ = json.Unmarshal(good, &g)
	if gt := g["typed"].(map[string]any); gt["digest"] == nil {
		t.Errorf("a config without errors has no digest: %v", gt)
	}
	n := g["nodes"].([]any)[0].(map[string]any)
	if n["in"] == nil || n["out"] == nil {
		t.Errorf("a typed node has no in and out: %v", n)
	}
	js, _ := fs.ReadFile(pageFS, "page/editor.js")
	for _, k := range []string{"v.typed", "t.palette", "t.digest", "t.source", "b.name", "b.kind", "b.in", "b.out", "b.inputs", "b.safety", "b.gate", "b.used", "w.not_certified", "n.in", "n.out"} {
		if !strings.Contains(string(js), k) {
			t.Errorf("the script no longer reads %s: update this list with it", k)
		}
	}
}

// The refusal of a misfit does not need the rest of the config to resolve: plan finds the type problems
// from the config and the registered types alone, so an unknown block elsewhere does not hide a misfit
// an edit adds (it once did, and the edit went through).
func TestTypedRefusalDoesNotNeedTheRestOfTheConfigToResolve(t *testing.T) {
	s := typedServers(t, "refund.yaml", testFlow)["registry"]
	broken := strings.Replace(testFlow, "block: receipt", "block: nowhere", 1) // an unknown block elsewhere
	w := call(s.Handler(), s, "/api/apply", applyReq(broken, Operation{Op: "set_block", Node: "charge", Value: "count"}), nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"refused":"the types do not fit: `) {
		t.Errorf("a misfit beside an unknown block was not refused: %d %s", w.Code, w.Body)
	}
}

// A config with an error elsewhere and a connection that does not fit: the view must not read as
// checked, whatever is listed. Its verdict is not
// good, it has errors, each node still carries the types the program gave its block (so the page marks
// the target), the weakening is not certified, and a save needs confirming.
func TestAMisfitBesideAnotherErrorNeverReadsAsChecked(t *testing.T) {
	text := strings.Replace(strings.Replace(testFlow, "block: receipt", "block: nowhere", 1), "wiring:\n", "  - {name: loose, block: count}\nwiring:\n  - edge: [validate, loose]\n", 1)
	for kind, s := range typedServers(t, "refund.yaml", testFlow) {
		if kind == "structural" {
			continue
		}
		v := s.view([]byte(text))
		if v.Errors == 0 || v.Verdict.Class == "good" || v.Weakening == nil || v.Weakening.NotCertified == "" {
			t.Errorf("%s: errors %d verdict %q not certified %v", kind, v.Errors, v.Verdict.Class, v.Weakening)
		}
		types := map[string][2]string{}
		for _, n := range v.Nodes {
			types[n.Name] = [2]string{n.In, n.Out}
		}
		if types["validate"][1] == "" || types["loose"][0] == "" || types["validate"][1] == types["loose"][0] {
			t.Errorf("%s: the page cannot mark the misfit: %v", kind, types)
		}
		if len(s.confirmations([]byte(text))) == 0 {
			t.Errorf("%s: a save of an uncertified text needs no confirmation", kind)
		}
	}
}

// Each item a save asks to confirm says what it is, by position, so the page words the statement from the kind
// and never from the item's text.
func TestPreviewNamesTheKindOfEachConfirmation(t *testing.T) {
	noAssert := testFlow[:strings.Index(testFlow, "assert:")]
	unknown := strings.Replace(noAssert, "block: receipt", "block: nowhere", 1)
	cases := []struct {
		name     string
		baseline string
		text     string
		kinds    []string
	}{
		{"nothing", noAssert, noAssert + "# note\n", nil},
		{"weakened", noAssert, strings.Replace(noAssert, "need: 2", "need: 1", 1), []string{ConfirmWeakens}},
		{"uncertified", unknown, unknown + "# note\n", []string{ConfirmUncertified}},
		{"both", unknown, strings.Replace(unknown, "need: 2", "need: 1", 1), []string{ConfirmWeakens, ConfirmUncertified}},
	}
	for _, c := range cases {
		s := typedServers(t, "refund.yaml", c.baseline)["registry"]
		pv, err := s.Preview([]byte(c.text), hashOf([]byte(c.baseline)))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(pv.NeedsKinds, c.kinds) || len(pv.NeedsKinds) != len(pv.NeedsConfirmation) {
			t.Errorf("%s: kinds %q for items %q, want %q", c.name, pv.NeedsKinds, pv.NeedsConfirmation, c.kinds)
		}
		items, kinds := s.confirmationKinds([]byte(c.text))
		if !slices.Equal(items, pv.NeedsConfirmation) || !slices.Equal(kinds, pv.NeedsKinds) {
			t.Errorf("%s: confirmationKinds %q %q differs from the preview", c.name, items, kinds)
		}
	}
	// A baseline that cannot be compared is its own kind.
	s := typedServers(t, "refund.yaml", testFlow)["structural"]
	s.baseline = []byte("nodes: [")
	if items, kinds := s.confirmationKinds([]byte(testFlow)); len(items) != 1 || !slices.Equal(kinds, []string{ConfirmUncompared}) {
		t.Errorf("an uncomparable baseline: %q %q", items, kinds)
	}
}

// Names as long as the file do not hide a type misfit: plan shortens the quoted names in its messages
// and keeps the words around them, which the typed editor matches (typeProblem), so a flow with long
// node names is still refused for connecting what does not fit, in both modes.
func TestTypedEditorRefusesAMisfitBetweenLongNames(t *testing.T) {
	for _, n := range []int{230, 300, 1000} {
		long := func(c string) string { return strings.Repeat(c, n) }
		// Only the node names are long: the blocks are the registered ones, and the edges and the
		// assertion name the nodes, so those are rewritten too.
		text := strings.NewReplacer("{name: validate,", "{name: "+long("v")+",", "entry: validate", "entry: "+long("v"),
			"name: charge", "name: "+long("c"), "edge: [validate, charge]", "edge: ["+long("v")+", "+long("c")+"]",
			"edge: [charge, receipt]", "edge: ["+long("c")+", receipt]").Replace(testFlow)
		text = strings.Replace(text, "every path to charge", "every path to "+long("c"), 1)
		servers := typedServers(t, "refund.yaml", text)
		misfit := Operation{Op: "set_block", Node: long("c"), Value: "count"}
		for _, kind := range []string{"registry", "description"} {
			s := servers[kind]
			w := call(s.Handler(), s, "/api/apply", applyReq(text, misfit), nil)
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"refused":"the types do not fit: `) || strings.Contains(w.Body.String(), `"text"`) {
				t.Errorf("%s, names of %d: %d %.300s", kind, n, w.Code, w.Body)
			}
		}
	}
}

// The same, with a node name over the 64 characters plan keeps of a quoted name: the rename of a node
// in an existing misfit is not refused for the misfit it already had.
func TestTypedRefusalComparesProblemsUnderARenameOfALongName(t *testing.T) {
	for _, n := range []int{64, 65, 300, 1000} {
		long := strings.Repeat("c", n)
		bad := strings.NewReplacer("name: charge", "name: "+long, "[validate, charge]", "[validate, "+long+"]", "[charge, receipt]", "["+long+", receipt]", "path to charge", "path to "+long).Replace(strings.Replace(testFlow, "block: charge", "block: count", 1))
		for kind, s := range typedServers(t, "refund.yaml", bad) {
			if kind == "structural" {
				continue
			}
			w := call(s.Handler(), s, "/api/apply", applyReq(bad, Operation{Op: "rename_node", Node: long, Value: "bill"}), nil)
			var e Edited
			if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || w.Code != 200 || !e.Changed || strings.Contains(w.Body.String(), `"refused"`) {
				t.Errorf("%s, name of %d: a rename of a node in an existing misfit: %d %.300s", kind, n, w.Code, w.Body)
			}
		}
	}
}

// chainFlow is a chain of blocks that all take and give an order, ending in the receipt. Setting a
// middle block to "count" (an int to an int) makes two misfits, on its two edges.
const chainFlow = `version: 1
flow: chain
entry: validate
nodes:
  - {name: validate, block: validate, safety: readonly}
  - {name: first, block: plain}
  - {name: second, block: plain}
  - {name: third, block: plain}
  - {name: receipt, block: receipt, safety: idempotent}
wiring:
  - edge: [validate, first]
  - edge: [first, second]
  - edge: [second, third]
  - edge: [third, receipt]
`

// When the text already has a type misfit, an operation that adds another is refused, not only one
// that adds the first: plan reports every type problem, so the editor sees the new one beside the old
// one (it once saw only the first, and let the second through).
func TestTypedEditorRefusesASecondMisfit(t *testing.T) {
	text := strings.Replace(chainFlow, "{name: first, block: plain}", "{name: first, block: count}", 1) // one misfit, on two edges
	servers := typedServers(t, "chain.yaml", text)
	for _, kind := range []string{"registry", "description"} {
		s := servers[kind]
		h := s.Handler()
		// An operation that adds a misfit elsewhere in the text.
		w := call(h, s, "/api/apply", applyReq(text, Operation{Op: "set_block", Node: "third", Value: "count"}), nil)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"refused":"the types do not fit: `) || strings.Contains(w.Body.String(), `"text"`) {
			t.Errorf("%s: a second misfit was not refused: %d %.300s", kind, w.Code, w.Body)
		}
		// An operation that fixes the misfit, and one that is unrelated, are not refused.
		w = call(h, s, "/api/apply", applyReq(text, Operation{Op: "set_block", Node: "first", Value: "plain"}), nil)
		var e Edited
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || !strings.Contains(e.Text, "{name: first, block: plain}") || strings.Contains(w.Body.String(), `"refused"`) {
			t.Errorf("%s: the fix was refused: err=%v %d %.100s", kind, err, w.Code, w.Body)
		}
		w = call(h, s, "/api/apply", applyReq(text, Operation{Op: "set_safety", Node: "validate", Value: "side_effect"}), nil)
		e = Edited{}
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || !e.Changed || strings.Contains(w.Body.String(), `"refused"`) {
			t.Errorf("%s: an unrelated operation was refused: %d %.300s", kind, w.Code, w.Body)
		}
	}
}

// The editor compares the full lists of type problems, not the 100 a report lists by default: with
// more than 100 misfits in the text, an edit that adds one more (past the cap, where a count would hide
// it) is refused, and an edit that fixes one is not.
func TestTypedEditorRefusesAMisfitPastTheReportCap(t *testing.T) {
	var nodes, wiring []string
	nodes = append(nodes, `  - {name: a0, block: validate, safety: readonly}`)
	for i := 1; i <= 130; i++ {
		block := "plain"
		if i%2 == 1 {
			block = "count" // an int step between order steps: a misfit on each of its edges
		}
		nodes = append(nodes, fmt.Sprintf("  - {name: a%d, block: %s}", i, block))
		wiring = append(wiring, fmt.Sprintf("  - edge: [a%d, a%d]", i-1, i))
	}
	nodes = append(nodes, "  - {name: t1, block: plain}", "  - {name: t2, block: plain}", "  - {name: end, block: receipt, safety: idempotent}")
	wiring = append(wiring, "  - edge: [a130, t1]", "  - edge: [t1, t2]", "  - edge: [t2, end]")
	text := "version: 1\nflow: long\nentry: a0\nnodes:\n" + strings.Join(nodes, "\n") + "\nwiring:\n" + strings.Join(wiring, "\n") + "\n"
	servers := typedServers(t, "long.yaml", text)
	for _, kind := range []string{"registry", "description"} {
		s := servers[kind]
		h := s.Handler()
		if got := len(typeProblemsOf(s.typing.inspect([]byte(mustJSON(t, text))))); got != 130 {
			t.Fatalf("%s: the text has %d type problems, want 130 (past the 100 a default report lists)", kind, got)
		}
		w := call(h, s, "/api/apply", applyReq(text, Operation{Op: "set_block", Node: "t2", Value: "count"}), nil)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"refused":"the types do not fit: `) || strings.Contains(w.Body.String(), `"text"`) {
			t.Errorf("%s: a misfit past the cap was not refused: %d %.300s", kind, w.Code, w.Body)
		}
		// A swap: a130 takes an int step, which fixes its in-edge misfit and adds a misfit on its out-edge,
		// at the end of the list. The number of misfits is the same (130) and the new one is past the
		// default cap: only a comparison of the full lists sees it.
		w = call(h, s, "/api/apply", applyReq(text, Operation{Op: "set_block", Node: "a130", Value: "count"}), nil)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"refused":"the types do not fit: `) || !strings.Contains(w.Body.String(), `a130`) {
			t.Errorf("%s: a swap past the cap (one misfit fixed, one added) was not refused: %d %.300s", kind, w.Code, w.Body)
		}
		w = call(h, s, "/api/apply", applyReq(text, Operation{Op: "set_safety", Node: "a0", Value: "side_effect"}), nil)
		if strings.Contains(w.Body.String(), `"refused"`) {
			t.Errorf("%s: an unrelated operation on a text with 130 misfits was refused: %.300s", kind, w.Body)
		}
	}
}

func typeProblemsOf(r plan.ConfigReport) []plan.ConfigProblem {
	var out []plan.ConfigProblem
	for _, p := range r.Problems {
		if typeProblem(p) {
			out = append(out, p)
		}
	}
	return out
}

func mustJSON(t *testing.T, yamlText string) string {
	t.Helper()
	j, err := ToJSON([]byte(yamlText))
	if err != nil {
		t.Fatal(err)
	}
	return string(j)
}

// If plan lists only the first 100 type problems (the option that lifts the cap missing), the editor
// cannot tell what an edit added past them and refuses rather than guess: the defence behind the
// complete lists.
func TestTypedEditorRefusesWhenTheReportIsCutShort(t *testing.T) {
	var nodes, wiring []string
	nodes = append(nodes, `  - {name: a0, block: validate, safety: readonly}`)
	for i := 1; i <= 130; i++ {
		block := "plain"
		if i%2 == 1 {
			block = "count"
		}
		nodes = append(nodes, fmt.Sprintf("  - {name: a%d, block: %s}", i, block))
		wiring = append(wiring, fmt.Sprintf("  - edge: [a%d, a%d]", i-1, i))
	}
	nodes = append(nodes, "  - {name: end, block: receipt, safety: idempotent}")
	wiring = append(wiring, "  - edge: [a130, end]")
	text := "version: 1\nflow: long\nentry: a0\nnodes:\n" + strings.Join(nodes, "\n") + "\nwiring:\n" + strings.Join(wiring, "\n") + "\n"
	servers := typedServers(t, "long.yaml", text)
	old := allTypeProblems
	allTypeProblems = func() plan.InspectOption { return plan.AllowUnusedBlocks() } // an option that does not lift the cap
	defer func() { allTypeProblems = old }()
	for _, kind := range []string{"registry", "description"} {
		s := servers[kind]
		w := call(s.Handler(), s, "/api/apply", applyReq(text, Operation{Op: "set_safety", Node: "a0", Value: "side_effect"}), nil)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"refused":"the types do not fit: too many type problems to compare`) || strings.Contains(w.Body.String(), `"text"`) {
			t.Errorf("%s: a cut-short report was not refused: %d %.300s", kind, w.Code, w.Body)
		}
	}
}

// Two different problems are two problems, whatever their names look like once shortened: two nodes
// whose names differ only after the 70th character each have a misfit, and the second is as new as the
// first would be.
func TestTypedEditorTellsApartProblemsWithLongNamesThatShortenAlike(t *testing.T) {
	l1, l2 := strings.Repeat("a", 70)+"1", strings.Repeat("a", 70)+"2"
	text := "version: 1\nflow: long\nentry: validate\nnodes:\n  - {name: validate, block: validate, safety: readonly}\n" +
		"  - {name: " + l1 + ", block: count}\n  - {name: " + l2 + ", block: plain}\nwiring:\n  - edge: [validate, " + l1 + "]\n  - edge: [validate, " + l2 + "]\n"
	servers := typedServers(t, "long.yaml", text)
	for _, kind := range []string{"registry", "description"} {
		s := servers[kind]
		w := call(s.Handler(), s, "/api/apply", applyReq(text, Operation{Op: "set_block", Node: l2, Value: "count"}), nil)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"refused":"the types do not fit: `) || strings.Contains(w.Body.String(), `"text"`) {
			t.Errorf("%s: a second misfit whose message looks like the first was not refused: %d %.300s", kind, w.Code, w.Body)
		}
	}
}

// An edit that only moves wiring elements does not make a problem the text already had new, though
// the message of a switch arm names its wiring element by position.
func TestTypedEditorIgnoresWiringPositionsWhenComparingProblems(t *testing.T) {
	text := "version: 1\nflow: arm\nentry: first\nnodes:\n  - {name: first, block: count}\n  - {name: cnt, block: count}\n  - {name: y, block: plain}\n  - {name: z, block: plain}\nwiring:\n" +
		"  - edge: [first, cnt]\n  - switch: cnt\n    when:\n      - {pred: large, to: y}\n    else: z\n" // an arm whose predicate tests an order, over an int
	servers := typedServers(t, "arm.yaml", text)
	for _, kind := range []string{"registry", "description"} {
		s := servers[kind]
		h := s.Handler()
		w := call(h, s, "/api/apply", applyReq(text, Operation{Op: "set_safety", Node: "y", Value: "side_effect"}), nil)
		if strings.Contains(w.Body.String(), `"refused"`) {
			t.Fatalf("%s: the text with its one existing misfit refuses an unrelated edit: %.300s", kind, w.Body)
		}
		w = call(h, s, "/api/apply", applyReq(text, Operation{Op: "move_wiring", Index: 0, Value: "down"}), nil)
		if w.Code != 200 || strings.Contains(w.Body.String(), `"refused"`) {
			t.Errorf("%s: moving the wiring element refused for a misfit it already had: %d %.300s", kind, w.Code, w.Body)
		}
	}
}

// A problem of another check does not hide the type problems: with a node that is wired to a name the
// config does not have, or a safety above the registered one, an edit that adds a misfit is refused.
func TestTypedEditorSeesTypeProblemsBesideOtherErrors(t *testing.T) {
	ghost := chainFlow + "  - edge: [third, ghost]\n"
	servers := typedServers(t, "ghost.yaml", ghost)
	for _, kind := range []string{"registry", "description"} {
		s := servers[kind]
		w := call(s.Handler(), s, "/api/apply", applyReq(ghost, Operation{Op: "set_block", Node: "second", Value: "count"}), nil)
		if !strings.Contains(w.Body.String(), `"refused":"the types do not fit: `) {
			t.Errorf("%s: a misfit beside an unknown name was not refused: %.300s", kind, w.Body)
		}
	}
	servers = typedServers(t, "chain.yaml", chainFlow)
	for _, kind := range []string{"registry", "description"} {
		s := servers[kind]
		// receipt states safety idempotent, which the int step (side effect) does not allow: an error of
		// another kind comes with the misfit.
		w := call(s.Handler(), s, "/api/apply", applyReq(chainFlow, Operation{Op: "set_block", Node: "receipt", Value: "count"}), nil)
		if !strings.Contains(w.Body.String(), `"refused":"`) {
			t.Errorf("%s: a misfit beside a safety error was not refused: %.300s", kind, w.Body)
		}
		// With the safety stated low enough, only the misfit is left, and it is refused as one.
		low := strings.Replace(chainFlow, "{name: receipt, block: receipt, safety: idempotent}", "{name: receipt, block: receipt}", 1)
		w = call(s.Handler(), s, "/api/apply", applyReq(low, Operation{Op: "set_block", Node: "receipt", Value: "count"}), nil)
		if !strings.Contains(w.Body.String(), `"refused":"the types do not fit: `) {
			t.Errorf("%s: a misfit without a safety error was not refused: %.300s", kind, w.Body)
		}
	}
}

// The same for a loop: the inner-switch refusal holds when the text has an unrelated error.
func TestLoopShapeRefusalHoldsBesideOtherErrors(t *testing.T) {
	src := "version: 1\nflow: f\nentry: a\nnodes:\n  - {name: a, block: a}\n  - {name: b, block: b}\n  - {name: c, block: c}\n  - {name: d, block: d}\nwiring:\n  - edge: [a, b]\n  - switch: b\n    when:\n      - {pred: x, to: c}\n    else: c\n  - switch: c\n    when:\n      - {pred: y, to: a}\n    else: d\n  - edge: [d, ghost]\n"
	why := refusalOf(t, "f.yaml", src, Operation{Op: "set_loop_max", Node: "c", Index: 0, Max: 3})
	if !strings.Contains(why, "has its own Switch") {
		t.Errorf("%q", why)
	}
}

// A node that states a retry safety above what its block registers is a false claim (crash recovery
// would act on it): an edit that adds one is refused, whether or not the text has another error.
func TestTypedEditorRefusesASafetyAboveTheRegistration(t *testing.T) {
	ghost := strings.Replace(testFlow, "  - edge: [charge, receipt]\n", "  - edge: [charge, receipt]\n  - edge: [receipt, ghost]\n", 1)
	for name, text := range map[string]string{"clean text": testFlow, "text with another error": ghost} {
		servers := typedServers(t, "r.yaml", text)
		for _, kind := range []string{"registry", "description"} {
			s := servers[kind]
			// charge is registered with no options (a side effect): "idempotent" claims it safe to run twice.
			w := call(s.Handler(), s, "/api/apply", applyReq(text, Operation{Op: "set_safety", Node: "charge", Value: "idempotent"}), nil)
			if !strings.Contains(w.Body.String(), `"refused":"the safety is above what the program registers: `) {
				t.Errorf("%s, %s: a safety above the registration was not refused: %.300s", name, kind, w.Body)
			}
			// Lowering it, or leaving it as the program has it, is not refused.
			w = call(s.Handler(), s, "/api/apply", applyReq(text, Operation{Op: "set_safety", Node: "validate", Value: "side_effect"}), nil)
			if strings.Contains(w.Body.String(), `"refused"`) {
				t.Errorf("%s, %s: a lowered safety was refused: %.300s", name, kind, w.Body)
			}
		}
	}
	// A text that already states one is not refused for it by an unrelated edit.
	bad := strings.Replace(testFlow, "safety: side_effect", "safety: idempotent", 1)
	for _, kind := range []string{"registry", "description"} {
		s := typedServers(t, "r.yaml", bad)[kind]
		w := call(s.Handler(), s, "/api/apply", applyReq(bad, Operation{Op: "set_safety", Node: "validate", Value: "side_effect"}), nil)
		if strings.Contains(w.Body.String(), `"refused"`) {
			t.Errorf("%s: an unrelated edit was refused for a claim the text already had: %.300s", kind, w.Body)
		}
	}
}

// What the editor lets through as work in progress (a node not wired in, an unknown block, a node not
// reached) cannot be saved as clean: the verdict is not green and the save asks for the statement
// that the check could not certify the text.
func TestWorkInProgressErrorsAreNeverSavedAsClean(t *testing.T) {
	for name, text := range map[string]string{
		"a node not wired in": strings.Replace(testFlow, "wiring:\n", "  - {name: extra, block: count}\nwiring:\n", 1),
		"an unknown block":    strings.Replace(testFlow, "block: receipt", "block: nowhere", 1),
		"an unreached node":   strings.Replace(testFlow, "  - {name: receipt, block: receipt, safety: idempotent}\n", "  - {name: receipt, block: receipt, safety: idempotent}\n  - {name: stray, block: plain}\n", 1),
	} {
		for _, kind := range []string{"registry", "description"} {
			s := typedServers(t, "r.yaml", testFlow)[kind]
			if v := s.view([]byte(text)); v.Verdict.Class == "good" || v.Errors == 0 {
				t.Errorf("%s, %s: the verdict is %q with %d errors", name, kind, v.Verdict.Class, v.Errors)
			}
			if _, kinds := s.confirmationKinds([]byte(text)); !slices.Contains(kinds, ConfirmUncertified) {
				t.Errorf("%s, %s: the save does not ask the 'cannot be certified' statement: %v", name, kind, kinds)
			}
		}
	}
}
