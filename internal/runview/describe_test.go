package runview

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/plan"
)

// gatedRegistry has two blocks of one type, "charge" that declares an approval gate in Go and
// "post" that declares none: the registrations G4 is about, because a config that swaps one for the
// other changes the gate without a word of it in the config.
func gatedRegistry(t *testing.T) *plan.Registry {
	t.Helper()
	reg := plan.NewRegistry()
	pass := func(_ context.Context, o order) (order, error) { return o, nil }
	for _, err := range []error{
		reg.RegisterStep("intake", pass, plan.ReadOnly()),
		reg.RegisterStep("charge", pass, plan.Approval(&agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "fin"}})),
		reg.RegisterStep("post", pass),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

// swapCfg runs intake and then one pay node on the named block. The block is registered, so Load
// takes either; the config states no gate and no safety.
func swapCfg(block string) string {
	return `{"version": 1, "flow": "pay", "entry": "intake",
  "nodes": [{"name": "intake", "block": "intake"}, {"name": "pay", "block": "` + block + `"}],
  "wiring": [{"edge": ["intake", "pay"]}]}`
}

func swapDiff(t *testing.T, from, to string, oldReg, newReg *plan.Registry, oldD, newD *plan.RegistryDescription) *DiffView {
	t.Helper()
	// Load refuses a block registered and never used, so each version is judged with the blocks it
	// uses: the other block is dropped from that version's registry or description.
	v, err := BuildDiff(Diff{
		Old: Config{Name: "old.json", Data: []byte(swapCfg(from)), Registry: oldReg, Description: oldD},
		New: Config{Name: "new.json", Data: []byte(swapCfg(to)), Registry: newReg, Description: newD},
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// only is a registry with just the named blocks of gatedRegistry (and intake).
func only(t *testing.T, block string) *plan.Registry {
	t.Helper()
	reg := plan.NewRegistry()
	pass := func(_ context.Context, o order) (order, error) { return o, nil }
	if err := reg.RegisterStep("intake", pass, plan.ReadOnly()); err != nil {
		t.Fatal(err)
	}
	var err error
	if block == "charge" {
		err = reg.RegisterStep("charge", pass, plan.Approval(&agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "fin"}}))
	} else {
		err = reg.RegisterStep("post", pass)
	}
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func described(r *plan.Registry) *plan.RegistryDescription { d := r.Describe(); return &d }

// G4: a node's block changes to one whose Go registration declares no gate. From the command line
// alone the gate is unknown on both sides and the change is flagged as possibly weakening; with a
// Registry, or a description of it, the gate is known and the change is judged by what it shows.
func TestDiffBlockSwapWithADescription(t *testing.T) {
	// Structural: flagged, saying the gate is not visible.
	v := swapDiff(t, "charge", "post", nil, nil, nil, nil)
	if len(v.Weakened) != 1 || !strings.Contains(strings.Join(v.Weakened, "\n"), "not visible without the plan.Registry") || v.Typed {
		t.Errorf("structural: typed %v, weakened %q", v.Typed, v.Weakened)
	}
	for name, mk := range map[string]func(r *plan.Registry) (*plan.Registry, *plan.RegistryDescription){
		"registry":    func(r *plan.Registry) (*plan.Registry, *plan.RegistryDescription) { return r, nil },
		"description": func(r *plan.Registry) (*plan.Registry, *plan.RegistryDescription) { return nil, described(r) },
	} {
		oldR, oldD := mk(only(t, "charge"))
		newR, newD := mk(only(t, "post"))
		v := swapDiff(t, "charge", "post", oldR, newR, oldD, newD)
		if !v.Typed || len(v.Weakened) != 1 || !strings.Contains(v.Weakened[0], "pay: approval 2 of 2: ops, fin -> none") || strings.Contains(v.Weakened[0], "not visible") {
			t.Errorf("%s, charge -> post: typed %v, weakened %q", name, v.Typed, v.Weakened)
		}
		if got := v.Basis; (name == "registry") != (got == SourceRegistry) || (name == "description") != (got == SourceDescription) {
			t.Errorf("%s: basis %q", name, got)
		}
		// The other way round adds a gate: strengthened, never weakened.
		oldR, oldD = mk(only(t, "post"))
		newR, newD = mk(only(t, "charge"))
		v = swapDiff(t, "post", "charge", oldR, newR, oldD, newD)
		if len(v.Weakened) != 0 || len(v.Strengthened) != 1 {
			t.Errorf("%s, post -> charge: weakened %q, strengthened %q", name, v.Weakened, v.Strengthened)
		}
		// A block that declares no gate on either side is not a weakening, and says so by being quiet.
		v = swapDiff(t, "post", "post", oldR, oldR, oldD, oldD)
		if len(v.Weakened) != 0 || len(v.Changes) != 0 {
			t.Errorf("%s, post -> post: %+v", name, v.Changes)
		}
	}
}

// A Single (one-decision) gate is described with Need 0, as the topology carries it: swapping a block
// that declares one for an ungated block is a weakening, whichever source knows the gate.
func TestDiffBlockSwapFromASingleGate(t *testing.T) {
	mk := func(gated bool) *plan.Registry {
		reg := plan.NewRegistry()
		pass := func(_ context.Context, o order) (order, error) { return o, nil }
		if err := reg.RegisterStep("intake", pass, plan.ReadOnly()); err != nil {
			t.Fatal(err)
		}
		var err error
		if gated {
			err = reg.RegisterStep("charge", pass, plan.Approval(agent.SingleApproval()))
		} else {
			err = reg.RegisterStep("post", pass)
		}
		if err != nil {
			t.Fatal(err)
		}
		return reg
	}
	for name, mkSide := range map[string]func(*plan.Registry) (*plan.Registry, *plan.RegistryDescription){
		"registry":    func(r *plan.Registry) (*plan.Registry, *plan.RegistryDescription) { return r, nil },
		"description": func(r *plan.Registry) (*plan.Registry, *plan.RegistryDescription) { return nil, described(r) },
	} {
		oR, oD := mkSide(mk(true))
		nR, nD := mkSide(mk(false))
		v := swapDiff(t, "charge", "post", oR, nR, oD, nD)
		if len(v.Weakened) != 1 || !strings.Contains(v.Weakened[0], "pay") || v.Verdict.Word != "APPROVAL GATE WEAKENED" {
			t.Errorf("%s: weakened %q, verdict %q", name, v.Weakened, v.Verdict.Word)
		}
		v = swapDiff(t, "post", "charge", nR, oR, nD, oD)
		if len(v.Weakened) != 0 || len(v.Strengthened) != 1 {
			t.Errorf("%s, reverse: weakened %q, strengthened %q", name, v.Weakened, v.Strengthened)
		}
	}
}

// One side checked against a description and the other against a Registry is a description-based
// result: the page and the text say so, and a Registry-only diff says nothing of the kind.
func TestDiffBasisWhenSidesDiffer(t *testing.T) {
	oR, nR := only(t, "charge"), only(t, "post")
	for name, v := range map[string]*DiffView{
		"old described": swapDiff(t, "charge", "post", nil, nR, described(oR), nil),
		"new described": swapDiff(t, "charge", "post", oR, nil, nil, described(nR)),
	} {
		if v.Basis != SourceDescription || v.BasisLine() != DescriptionNote {
			t.Errorf("%s: basis %q, line %q", name, v.Basis, v.BasisLine())
		}
	}
	v := swapDiff(t, "charge", "post", oR, nR, nil, nil)
	if v.Basis != SourceRegistry || v.BasisLine() != "" {
		t.Errorf("registry both: basis %q, line %q", v.Basis, v.BasisLine())
	}
	var out bytes.Buffer
	if _, err := CheckDiff(&out, Diff{Old: Config{Name: "o.json", Data: []byte(swapCfg("charge")), Registry: oR}, New: Config{Name: "n.json", Data: []byte(swapCfg("post")), Registry: nR}}); err != nil || strings.Contains(out.String(), "description file") {
		t.Errorf("registry diff text: %v %q", err, out.String())
	}
}

// The description's answer is the Registry's: the same changes, weakened and strengthened lists, and
// digests, for a set of edits, with only the basis differing.
func TestDiffDescriptionAgreesWithRegistry(t *testing.T) {
	for _, c := range [][2]string{{"charge", "post"}, {"post", "charge"}, {"charge", "charge"}} {
		oldR, newR := only(t, c[0]), only(t, c[1])
		a := swapDiff(t, c[0], c[1], oldR, newR, nil, nil)
		b := swapDiff(t, c[0], c[1], nil, nil, described(oldR), described(newR))
		a.Basis, b.Basis = "", ""
		ja, _ := json.Marshal(a)
		jb, _ := json.Marshal(b)
		// The graph's labels carry reflect's short type names in one and paths in the other.
		a.Graph, b.Graph = nil, nil
		ja, _ = json.Marshal(a)
		jb, _ = json.Marshal(b)
		if string(ja) != string(jb) {
			t.Errorf("%v:\nregistry    %s\ndescription %s", c, ja, jb)
		}
		if a.OldDigest == "" || a.OldDigest != b.OldDigest || a.NewDigest != b.NewDigest {
			t.Errorf("%v: digests %q %q, %q %q", c, a.OldDigest, b.OldDigest, a.NewDigest, b.NewDigest)
		}
	}
}

// A typed result from a description says so, in the text and on the pages, and one from a Registry
// does not; a Registry wins over a description given with it.
func TestDescriptionIsLabelled(t *testing.T) {
	reg := only(t, "post")
	var out bytes.Buffer
	Check(&out, Config{Name: "pay.json", Data: []byte(swapCfg("post")), Description: described(reg)})
	if !strings.Contains(out.String(), "not checked against the program") {
		t.Errorf("check output %q", out.String())
	}
	out.Reset()
	Check(&out, Config{Name: "pay.json", Data: []byte(swapCfg("post")), Registry: reg})
	if out.Len() != 0 {
		t.Errorf("a Registry check printed %q", out.String())
	}
	both := Config{Name: "pay.json", Data: []byte(swapCfg("post")), Registry: reg, Description: &plan.RegistryDescription{Version: 1}}
	if v := BuildConfig(both); v.Source != SourceRegistry || v.Errors != 0 {
		t.Errorf("both given: source %q, errors %d", v.Source, v.Errors)
	}
	v := configView(t, Config{Name: "pay.json", Data: []byte(swapCfg("post")), Description: described(reg)})
	if !v.Typed || v.Source != SourceDescription || !strings.Contains(v.CheckLabel, "not checked against the program") || v.Digest == "" {
		t.Errorf("view: typed %v source %q label %q digest %q", v.Typed, v.Source, v.CheckLabel, v.Digest)
	}
	h := Handler(Options{Config: &Config{Name: "pay.json", Data: []byte(swapCfg("post")), Description: described(reg)}})
	if _, body := get(t, h, "/"); !strings.Contains(body, "Per a description file, not checked against the program.") {
		t.Error("the config page does not say it rests on a description file")
	}
	d := Diff{Old: Config{Name: "old.json", Data: []byte(swapCfg("post")), Description: described(reg)}, New: Config{Name: "new.json", Data: []byte(swapCfg("post")), Description: described(reg)}}
	if _, body := get(t, Handler(Options{Diff: &d}), "/"); !strings.Contains(body, "Checked per a description file") {
		t.Error("the diff page does not say it rests on a description file")
	}
}

func TestReadDescription(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good, _ := json.Marshal(only(t, "charge").Describe())
	if d, err := ReadDescription(write("good.json", good)); err != nil || len(d.Blocks) != 2 {
		t.Fatalf("a Describe result: %v, %+v", err, d)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(filepath.Join(dir, "good.json"), link); err == nil {
		if _, err := ReadDescription(link); err != nil {
			t.Errorf("a symlink to a regular file: %v", err)
		}
	}
	cases := map[string][]byte{
		"unknown field":   []byte(`{"version":1,"blocks":[],"predicates":[],"merges":[],"extra":1}`),
		"misspelled":      []byte(`{"version":1,"Blocks":[],"predicates":[],"merges":[]}`),
		"trailing data":   append(append([]byte{}, good...), []byte(` {}`)...),
		"not json":        []byte(`blocks:`),
		"wrong version":   []byte(`{"version":2,"blocks":[],"predicates":[],"merges":[]}`),
		"invalid utf-8":   []byte("{\"version\":1,\"blocks\":[{\"name\":\"a\xff\"}]}"),
		"bad kind":        []byte(`{"version":1,"blocks":[{"name":"a","kind":"x","in":"i","out":"o","safety":"readonly"}],"predicates":[],"merges":[]}`),
		"duplicate field": []byte(`{"version":1,"version":1,"blocks":[],"predicates":[],"merges":[]}`),
	}
	for name, data := range cases {
		if _, err := ReadDescription(write("c.json", data)); err == nil {
			t.Errorf("%s: read without error", name)
		}
	}
	big := bytes.Repeat([]byte(" "), MaxDescriptionFile+1)
	if _, err := ReadDescription(write("big.json", append(big, good...))); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("too large: %v", err)
	}
	if _, err := ReadDescription(dir); err == nil {
		t.Error("a directory was read")
	}
	if _, err := ReadDescription(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("a missing file was read")
	}
}

// bide view -config -blocks and -diff -blocks/-old-blocks, end to end through Main.
func TestMainBlocksFlags(t *testing.T) {
	dir := t.TempDir()
	put := func(name, data string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	chargeD, _ := json.Marshal(only(t, "charge").Describe())
	postD, _ := json.Marshal(only(t, "post").Describe())
	oldCfg, newCfg := put("old.json", swapCfg("charge")), put("new.json", swapCfg("post"))
	blocksOld, blocksNew := put("old-blocks.json", string(chargeD)), put("new-blocks.json", string(postD))
	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := Main(args, &out, &errb, Stores{})
		return code, out.String(), errb.String()
	}
	if code, out, errs := run("-config", newCfg, "-check", "-blocks", blocksNew); code != 0 || !strings.Contains(out, "not checked against the program") {
		t.Errorf("check -blocks: %d %q %q", code, out, errs)
	}
	// One description for both versions: the old version then uses "charge", which it does not
	// describe, and is an error; -old-blocks gives it the description of its own program.
	if code, out, _ := run("-config", newCfg, "-diff", oldCfg, "-check", "-blocks", blocksNew); code != 0 || !strings.Contains(out, "error") {
		t.Errorf("one description: %d %q", code, out)
	}
	code, out, errs := run("-config", newCfg, "-diff", oldCfg, "-check", "-fail-on-weaken", "-blocks", blocksNew, "-old-blocks", blocksOld)
	if code != 1 || !strings.Contains(out, "approval 2 of 2: ops, fin -> none") || !strings.Contains(out, "per description file") {
		t.Errorf("-old-blocks: %d %q %q", code, out, errs)
	}
	if code, _, errs := run("-blocks", blocksNew, "-journal", "x.json"); code != 2 || !strings.Contains(errs, "-blocks needs -config") {
		t.Errorf("-blocks without -config: %d %q", code, errs)
	}
	for name, args := range map[string][]string{
		"-blocks needs -config":      {"-blocks", blocksNew},
		"-old-blocks needs -diff":    {"-config", newCfg, "-old-blocks", blocksOld},
		"missing file":               {"-config", newCfg, "-check", "-blocks", filepath.Join(dir, "none.json")},
		"description is not a file":  {"-config", newCfg, "-check", "-blocks", dir},
		"description is not invalid": {"-config", newCfg, "-check", "-blocks", put("bad.json", `{"version":3}`)},
	} {
		if code, _, errs := run(args...); code == 0 || errs == "" {
			t.Errorf("%s: exit %d, stderr %q", name, code, errs)
		}
	}
}

// A description InspectConfigDescribed cannot use (invalid, or past the process's type-name budget:
// both come back as one error problem and an untyped result) is never a clean pass: the verdict is
// not good, the check fails, and nothing says "checked per description" as if it had been.
func TestAnUnusableDescriptionIsNotACleanPass(t *testing.T) {
	d := described(only(t, "post"))
	d.Errors = []string{`duplicate block registration "post"`}
	c := Config{Name: "pay.json", Data: []byte(swapCfg("post")), Description: d}
	v := BuildConfig(c)
	if v.Typed || v.Errors == 0 || v.Verdict.Class == "good" || v.Verdict.Class == "" {
		t.Errorf("typed %v, errors %d, verdict %+v", v.Typed, v.Errors, v.Verdict)
	}
	var out bytes.Buffer
	if !Check(&out, c) || !strings.Contains(out.String(), "blocks description:") || strings.Contains(out.String(), "per description file") {
		t.Errorf("check output %q", out.String())
	}
	diff, err := BuildDiff(Diff{Old: c, New: Config{Name: "new.json", Data: []byte(swapCfg("post")), Description: described(only(t, "post"))}})
	if err != nil || diff.Typed || diff.Basis != "" || diff.OldErrors == 0 {
		t.Errorf("diff: typed %v basis %q old errors %d (%v)", diff.Typed, diff.Basis, diff.OldErrors, err)
	}
}
