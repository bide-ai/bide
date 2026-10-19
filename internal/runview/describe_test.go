package runview

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		if v.Basis != SourceDescription || !strings.Contains(v.BasisLine(), DescriptionNote) {
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
		a.Basis, b.Basis, a.OldBasis, b.OldBasis, a.NewBasis, b.NewBasis = "", "", "", "", "", ""
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
	if _, body := get(t, Handler(Options{Diff: &d}), "/"); !strings.Contains(body, "checked per description file") {
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
	// -blocks alone describes the new version only: it is never applied to the old one, which is
	// then checked structurally and labelled so.
	if code, out, _ := run("-config", newCfg, "-diff", oldCfg, "-check", "-blocks", blocksNew); code != 0 || !strings.Contains(out, "old config structural, new config per description file") {
		t.Errorf("-blocks alone: %d %q", code, out)
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

// wholeProgram is a description holding every block the program registers, though a flow config uses
// only some: the realistic -blocks file. Checking a config against it reports the unused blocks.
func wholeProgram(t *testing.T) *plan.RegistryDescription {
	t.Helper()
	reg := plan.NewRegistry()
	pass := func(_ context.Context, o order) (order, error) { return o, nil }
	for _, err := range []error{
		reg.RegisterStep("intake", pass, plan.ReadOnly()),
		reg.RegisterStep("charge", pass, plan.Approval(&agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "fin"}})),
		reg.RegisterStep("single", pass, plan.Approval(agent.SingleApproval())),
		reg.RegisterStep("post", pass),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	return described(reg)
}

// Adding a description must never make check -diff less strict than the same diff without it. A
// whole-program description makes every flow config report the blocks it does not use, so each side
// has typed errors; the gate comparison then has nothing to read, and used to say nothing. The diff is
// then made structurally, as it is without the description, and is not certified.
func TestDiffWithADescriptionIsNeverLessStrict(t *testing.T) {
	d := wholeProgram(t)
	for _, c := range []struct{ from, to string }{{"charge", "post"}, {"single", "post"}, {"charge", "single"}, {"post", "charge"}, {"post", "post"}} {
		without := Diff{Old: Config{Name: "old.json", Data: []byte(swapCfg(c.from))}, New: Config{Name: "new.json", Data: []byte(swapCfg(c.to))}}
		with := Diff{Old: Config{Name: "old.json", Data: []byte(swapCfg(c.from)), Description: d}, New: Config{Name: "new.json", Data: []byte(swapCfg(c.to)), Description: d}}
		var o1, o2 bytes.Buffer
		weak1, err1 := CheckDiff(&o1, without)
		weak2, err2 := CheckDiff(&o2, with)
		if err1 != nil || err2 != nil {
			t.Fatal(err1, err2)
		}
		v1, _ := BuildDiff(without)
		v2, _ := BuildDiff(with)
		if weak1 && !weak2 {
			t.Errorf("%s -> %s: weakened without the description, not with it\\n%s\\n%s", c.from, c.to, o1.String(), o2.String())
		}
		if len(v2.Weakened) < len(v1.Weakened) {
			t.Errorf("%s -> %s: weakened %q with, %q without", c.from, c.to, v2.Weakened, v1.Weakened)
		}
		if v2.Fallback == "" || v2.Typed || v2.OldErrors == 0 || !weak2 {
			t.Errorf("%s -> %s: with errors on both sides the diff is not certified: fallback %q typed %v errors %d/%d exit %v", c.from, c.to, v2.Fallback, v2.Typed, v2.OldErrors, v2.NewErrors, weak2)
		}
		if v2.Verdict.Class == "good" {
			t.Errorf("%s -> %s: verdict %+v", c.from, c.to, v2.Verdict)
		}
		if !strings.Contains(o2.String(), "not certified") {
			t.Errorf("%s -> %s: the text does not say so: %q", c.from, c.to, o2.String())
		}
	}
	// The reported case: a Single gate dropped.
	with := Diff{Old: Config{Name: "o.json", Data: []byte(swapCfg("single")), Description: d}, New: Config{Name: "n.json", Data: []byte(swapCfg("post")), Description: d}}
	v, _ := BuildDiff(with)
	if len(v.Weakened) != 1 || v.Verdict.Class != "bad" || !strings.Contains(strings.Join(v.Verdict.Details, "\n"), "not certified") {
		t.Errorf("single -> post: %q, verdict %+v", v.Weakened, v.Verdict)
	}
	// The same through a Registry that holds more than the config uses.
	reg := plan.NewRegistry()
	pass := func(_ context.Context, o order) (order, error) { return o, nil }
	for _, err := range []error{reg.RegisterStep("intake", pass, plan.ReadOnly()), reg.RegisterStep("charge", pass, plan.Approval(agent.SingleApproval())), reg.RegisterStep("post", pass)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	rd := Diff{Old: Config{Name: "o.json", Data: []byte(swapCfg("charge")), Registry: reg}, New: Config{Name: "n.json", Data: []byte(swapCfg("post")), Registry: reg}}
	if weak, err := CheckDiff(io.Discard, rd); err != nil || !weak {
		t.Errorf("a Registry with an unused block: weakened %v, %v", weak, err)
	}
}

// A description that fits each side exactly is typed and certified; the gate swap reads as the
// description says. A side that uses a block the description lacks is not certified either.
func TestDiffFallbackOnlyWhenATypedSideHasErrors(t *testing.T) {
	exact := Diff{
		Old: Config{Name: "o.json", Data: []byte(swapCfg("charge")), Description: described(only(t, "charge"))},
		New: Config{Name: "n.json", Data: []byte(swapCfg("post")), Description: described(only(t, "post"))},
	}
	v, _ := BuildDiff(exact)
	if v.Fallback != "" || !v.Typed || len(v.Weakened) != 1 {
		t.Errorf("exact descriptions: fallback %q typed %v weakened %q", v.Fallback, v.Typed, v.Weakened)
	}
	missing := exact
	missing.Old.Description = described(only(t, "post")) // the old config uses a block this lacks
	v, _ = BuildDiff(missing)
	if v.Fallback == "" || v.Typed || len(v.Weakened) != 1 {
		t.Errorf("an old block the description lacks: fallback %q typed %v weakened %q", v.Fallback, v.Typed, v.Weakened)
	}
	var out bytes.Buffer
	if weak, err := CheckDiff(&out, missing); err != nil || !weak {
		t.Errorf("exit: %v %v", weak, err)
	}
	// With nothing to weaken and a side that did not check, the verdict is not good and the exit fails.
	same := Diff{Old: Config{Name: "o.json", Data: []byte(swapCfg("post")), Description: wholeProgram(t)}, New: Config{Name: "n.json", Data: []byte(swapCfg("post")), Description: wholeProgram(t)}}
	v, _ = BuildDiff(same)
	if v.Verdict.Class == "good" || v.Fallback == "" {
		t.Errorf("no changes, errors on both sides: %+v fallback %q", v.Verdict, v.Fallback)
	}
	if weak, _ := CheckDiff(io.Discard, same); !weak {
		t.Error("no changes and a side that did not check passes -fail-on-weaken")
	}
}

// Every result line says which side was typed and how: a description for one side only is labelled,
// not read as a structural result or as a typed one.
func TestDiffBasisNamesEachSide(t *testing.T) {
	o, n := only(t, "charge"), only(t, "post")
	v := swapDiff(t, "charge", "post", nil, n, described(o), nil)
	if v.OldBasis != SourceDescription || v.NewBasis != SourceRegistry || v.BasisLine() == "" {
		t.Errorf("description and registry: %q %q %q", v.OldBasis, v.NewBasis, v.BasisLine())
	}
	v = swapDiff(t, "charge", "post", nil, nil, described(o), nil)
	if v.OldBasis != SourceDescription || v.NewBasis != "structural" || !strings.Contains(v.BasisLine(), "old config per description file") || !strings.Contains(v.BasisLine(), "new config structural") {
		t.Errorf("old only: %q %q %q", v.OldBasis, v.NewBasis, v.BasisLine())
	}
	if v = swapDiff(t, "charge", "post", nil, nil, nil, nil); v.OldBasis != "structural" || v.BasisLine() != "" {
		t.Errorf("neither: %q %q", v.OldBasis, v.BasisLine())
	}
	dir := t.TempDir()
	put := func(name, data string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	b, _ := json.Marshal(o.Describe())
	var out, errb bytes.Buffer
	code := Main([]string{"-config", put("n.json", swapCfg("post")), "-diff", put("o.json", swapCfg("charge")), "-check", "-old-blocks", put("ob.json", string(b))}, &out, &errb, Stores{})
	if code != 0 || !strings.Contains(out.String(), "old config per description file") {
		t.Errorf("view -old-blocks alone: %d %q %q", code, out.String(), errb.String())
	}
}

// ReadDescription never blocks on a file that is not a regular one, and a hostile file cannot make
// an error message long.
func TestReadDescriptionHostileFiles(t *testing.T) {
	dir := t.TempDir()
	if p, ok := makeFIFO(t, dir); ok {
		done := make(chan error, 1)
		go func() { _, err := ReadDescription(p); done <- err }()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "not a regular file") {
				t.Errorf("a FIFO: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("ReadDescription blocked on a FIFO")
		}
	}
	deep := filepath.Join(dir, "deep.json")
	if err := os.WriteFile(deep, bytes.Repeat([]byte("["), MaxDescriptionFile), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ReadDescription(deep)
	if err == nil || len(err.Error()) > 400 || !strings.Contains(err.Error(), "nested more than") {
		t.Errorf("a nested file: %d bytes of error", len(fmt.Sprint(err)))
	}
	long := filepath.Join(dir, "long.json")
	if err := os.WriteFile(long, []byte(`{"version":1,"`+strings.Repeat("k", 5000)+`":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDescription(long); err == nil || len(err.Error()) > 400 || !strings.Contains(err.Error(), "...") {
		t.Errorf("a long key: %v", err)
	}
}

// For every pair of configs and every kind of description a user might hand over (exact for each side,
// the whole program, one that lacks a block, an empty one), adding the description never lowers the
// exit status of check -diff -fail-on-weaken, and never drops a weakening the structural check reports,
// except the "possibly" it could not see past and the description now answers (a block whose Go gate
// is known).
func TestDiffDescriptionNeverLessStrictCorpus(t *testing.T) {
	blocks := []string{"charge", "single", "post"}
	empty := &plan.RegistryDescription{Version: 1, Blocks: []plan.BlockDescription{}, Predicates: []plan.PredicateDescription{}, Merges: []plan.MergeDescription{}}
	whole := wholeProgram(t)
	lacking := func() *plan.RegistryDescription {
		d := *whole
		d.Blocks = nil
		for _, b := range whole.Blocks {
			if b.Name != "charge" {
				d.Blocks = append(d.Blocks, b)
			}
		}
		return &d
	}()
	kinds := map[string]func(from, to string) (*plan.RegistryDescription, *plan.RegistryDescription){
		"whole":    func(string, string) (*plan.RegistryDescription, *plan.RegistryDescription) { return whole, whole },
		"lacking":  func(string, string) (*plan.RegistryDescription, *plan.RegistryDescription) { return lacking, lacking },
		"empty":    func(string, string) (*plan.RegistryDescription, *plan.RegistryDescription) { return empty, empty },
		"old only": func(string, string) (*plan.RegistryDescription, *plan.RegistryDescription) { return whole, nil },
		"new only": func(string, string) (*plan.RegistryDescription, *plan.RegistryDescription) { return nil, whole },
	}
	type key struct{ node, kind string }
	weakKeys := func(v *DiffView) map[key]bool {
		out := map[key]bool{}
		for _, c := range v.Changes {
			if c.Gate == "weakened" && !strings.Contains(strings.Join(c.Details, " "), "not visible without") {
				out[key{c.Node, c.Weakening}] = true
			}
		}
		return out
	}
	n := 0
	for _, from := range blocks {
		for _, to := range blocks {
			base := Diff{Old: Config{Name: "o.json", Data: []byte(swapCfg(from))}, New: Config{Name: "n.json", Data: []byte(swapCfg(to))}}
			without, _ := BuildDiff(base)
			exitWithout, _ := CheckDiff(io.Discard, base)
			for name, mk := range kinds {
				od, nd := mk(from, to)
				d := base
				d.Old.Description, d.New.Description = od, nd
				with, err := BuildDiff(d)
				if err != nil {
					t.Fatal(err)
				}
				exitWith, _ := CheckDiff(io.Discard, d)
				n++
				if exitWithout && !exitWith {
					t.Errorf("%s %s->%s: fails without the description, passes with it", name, from, to)
				}
				if (od == nil) != (nd == nil) && with.Fallback == "" {
					// One side only: the comparison is the structural one, change for change.
					if strings.Join(lines(with.Changes), "\n") != strings.Join(lines(without.Changes), "\n") || len(with.Weakened) != len(without.Weakened) || with.Typed {
						t.Errorf("%s %s->%s: a one-sided description is not compared structurally: %q vs %q", name, from, to, lines(with.Changes), lines(without.Changes))
					}
				}
				if with.Fallback != "" {
					// Not certified: the structural result, in full.
					if len(with.Weakened) != len(without.Weakened) || !exitWith {
						t.Errorf("%s %s->%s: fallback weakened %q vs %q, exit %v", name, from, to, with.Weakened, without.Weakened, exitWith)
					}
					continue
				}
				if with.Verdict.Class == "good" && without.Verdict.Class == "bad" && len(weakKeys(without)) > 0 {
					t.Errorf("%s %s->%s: good with, bad without, and the weakening was not a 'possibly'", name, from, to)
				}
				for k := range weakKeys(without) {
					if !weakKeys(with)[k] {
						t.Errorf("%s %s->%s: the weakening %v of the structural check vanished", name, from, to, k)
					}
				}
			}
		}
	}
	if n < 40 {
		t.Errorf("only %d diffs", n)
	}
}

// The realistic use: one description of the whole program for every flow config. The blocks a config
// does not use are not reported (a description is of the program; a flow uses some of it), so the gate
// swap is judged by what the description says, typed and certified, and fails -fail-on-weaken.
func TestMainWholeProgramDescription(t *testing.T) {
	dir := t.TempDir()
	put := func(name, data string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	b, _ := json.Marshal(wholeProgram(t))
	desc := put("blocks.json", string(b))
	run := func(args ...string) (int, string) {
		var out, errb bytes.Buffer
		code := Main(args, &out, &errb, Stores{})
		return code, out.String() + errb.String()
	}
	code, out := run("-config", put("new.json", swapCfg("post")), "-diff", put("old.json", swapCfg("single")), "-check", "-fail-on-weaken", "-blocks", desc, "-old-blocks", desc)
	if code != 1 || !strings.Contains(out, "approval one decision -> none") || strings.Contains(out, "not certified") || strings.Contains(out, "error") {
		t.Errorf("single -> post: %d %q", code, out)
	}
	code, out = run("-config", put("n2.json", swapCfg("single")), "-diff", put("o2.json", swapCfg("post")), "-check", "-fail-on-weaken", "-blocks", desc, "-old-blocks", desc)
	if code != 0 || !strings.Contains(out, "strengthened") && !strings.Contains(out, "1 change") {
		t.Errorf("post -> single: %d %q", code, out)
	}
	if code, out = run("-config", put("c.json", swapCfg("post")), "-check", "-blocks", desc); code != 0 || strings.Contains(out, "registered but never used") {
		t.Errorf("one config: %d %q", code, out)
	}
}

// A gate removed in Go with the config unchanged: the old description says the block is gated, the new
// one says it is not. With both descriptions the diff says so and fails; with -blocks alone the old
// side is never read with the new description (that would make the two sides agree on a gate-less
// block and hide it), so the result is no quieter than the same diff without any flag.
func TestMainAGoSideGateRemovalIsNotHiddenByOneDescription(t *testing.T) {
	dir := t.TempDir()
	put := func(name, data string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	gated, _ := json.Marshal(only(t, "charge").Describe())
	// The same program after its gate was removed in Go: the block is still called "charge".
	reg := plan.NewRegistry()
	pass := func(_ context.Context, o order) (order, error) { return o, nil }
	for _, err := range []error{reg.RegisterStep("intake", pass, plan.ReadOnly()), reg.RegisterStep("charge", pass)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	ungated, _ := json.Marshal(reg.Describe())
	cfg := put("flow.json", swapCfg("charge"))
	same := put("same.json", swapCfg("charge"))
	run := func(args ...string) (int, string) {
		var out, errb bytes.Buffer
		code := Main(args, &out, &errb, Stores{})
		return code, out.String() + errb.String()
	}
	base := []string{"-config", cfg, "-diff", same, "-check", "-fail-on-weaken"}
	exitPlain, _ := run(base...)
	exitNew, out := run(append(base, "-blocks", put("new.json", string(ungated)))...)
	// An unchanged config with no Go change on either side must not fail: the typed side knows safety the
	// structural side leaves unstated, and that is not a lowering.
	if exitNew != 0 || strings.Contains(out, "lowered") || strings.Contains(out, "weakened") || !strings.Contains(out, "old config structural") {
		t.Errorf("-blocks alone: exit %d (plain %d) %q", exitNew, exitPlain, out)
	}
	exitOld, out := run(append(base, "-old-blocks", put("old.json", string(gated)))...)
	if exitOld != 0 || strings.Contains(out, "lowered") || strings.Contains(out, "weakened") || !strings.Contains(out, "new config structural") {
		t.Errorf("-old-blocks alone: exit %d (plain %d) %q", exitOld, exitPlain, out)
	}
	// Both: the Go-side removal is a weakening and fails the gate.
	code, out := run(append(base, "-blocks", put("new2.json", string(ungated)), "-old-blocks", put("old2.json", string(gated)))...)
	if code != 1 || !strings.Contains(out, "approval 2 of 2: ops, fin -> none") {
		t.Errorf("both descriptions: %d %q", code, out)
	}
}

// A result that rests on description files says which files: their SHA-256, the one sha256sum prints,
// and for a diff that both sides rest on files, not on the programs.
func TestResultsNameTheDescriptionFilesTheyRestOn(t *testing.T) {
	dir := t.TempDir()
	put := func(name string, data []byte) (string, string) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		return p, hex.EncodeToString(sum[:])
	}
	co, _ := json.MarshalIndent(only(t, "charge").Describe(), "", " ")
	po, _ := json.Marshal(only(t, "post").Describe()) // different bytes, whitespace included
	oldP, oldSum := put("old-blocks.json", co)
	newP, newSum := put("new-blocks.json", po)
	cfgOld, _ := put("old.json", []byte(swapCfg("charge")))
	cfgNew, _ := put("new.json", []byte(swapCfg("post")))
	var out, errb bytes.Buffer
	code := Main([]string{"-config", cfgNew, "-diff", cfgOld, "-check", "-fail-on-weaken", "-blocks", newP, "-old-blocks", oldP}, &out, &errb, Stores{})
	text := out.String()
	if code != 1 || !strings.Contains(text, "per description files, not checked against the programs") ||
		!strings.Contains(text, "earlier file sha256 "+oldSum) || !strings.Contains(text, "this file sha256 "+newSum) {
		t.Errorf("both described: %d %q", code, text)
	}
	out.Reset()
	code = Main([]string{"-config", cfgNew, "-check", "-blocks", newP}, &out, &errb, Stores{})
	if code != 0 || !strings.Contains(out.String(), "description file sha256 "+newSum) {
		t.Errorf("one config: %d %q", code, out.String())
	}
	h := Handler(Options{Config: &Config{Name: "n.json", Data: []byte(swapCfg("post")), Description: described(only(t, "post")), DescriptionSHA256: newSum}})
	if _, body := get(t, h, "/"); !strings.Contains(body, newSum) {
		t.Error("the config page does not name the description file")
	}
	if _, body := get(t, h, "/config.json"); !strings.Contains(body, `"description_sha256": "`+newSum+`"`) {
		t.Errorf("config.json: %s", body)
	}
	if _, sum, err := ReadDescriptionFile(newP); err != nil || sum != newSum {
		t.Errorf("ReadDescriptionFile: %q %v", sum, err)
	}
}

// Brackets inside strings do not count towards the depth, an escaped quote does not end a string, and
// exactly the maximum depth is allowed.
func TestCheckDepth(t *testing.T) {
	deep := func(n int) []byte { return []byte(strings.Repeat("[", n) + strings.Repeat("]", n)) }
	for name, c := range map[string]struct {
		in string
		ok bool
	}{
		"exactly the maximum":  {string(deep(maxDescriptionDepth)), true},
		"one over":             {string(deep(maxDescriptionDepth + 1)), false},
		"brackets in a string": {`{"a":"` + strings.Repeat("[", 500) + `"}`, true},
		"escaped quote":        {`{"a":"x\"` + strings.Repeat("[", 500) + `"}`, true},
		"escaped backslash, then the string ends": {`{"a":"\\"` + string(deep(maxDescriptionDepth+1)), false},
		"stray closers buy no depth":              {strings.Repeat("]", 100) + string(deep(maxDescriptionDepth+1)), false},
		"object nesting":                          {strings.Repeat(`{"a":`, maxDescriptionDepth+1) + "1" + strings.Repeat("}", maxDescriptionDepth+1), false},
	} {
		if err := checkDepth([]byte(c.in)); (err == nil) != c.ok {
			t.Errorf("%s: %v, want ok=%v", name, err, c.ok)
		}
	}
}
