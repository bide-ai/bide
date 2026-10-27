package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bide-ai/bide/plan"
)

func flowDesc(blocks ...plan.BlockDescription) plan.RegistryDescription {
	return plan.RegistryDescription{Version: plan.DescriptionVersion, TaintDeclared: true, Blocks: blocks, Predicates: []plan.PredicateDescription{}, Merges: []plan.MergeDescription{}}
}

func blk(name, safety string) plan.BlockDescription {
	return plan.BlockDescription{Name: name, Kind: "step", In: "x.In", Out: "x.Out", Safety: safety}
}

var gatePolicy = &plan.TaintPolicyDescription{Gate: &plan.GateDescription{Need: 2, Approvers: []string{"a", "b"}, ExcludePrincipal: true}}

func writeFlow(t *testing.T, name string, d plan.RegistryDescription) string {
	t.Helper()
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return writeRaw(t, name, string(raw))
}

func writeRaw(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFlowDeclarations_Report(t *testing.T) {
	d := flowDesc(blk("fetch", "side_effect"), blk("pay", "side_effect"), blk("look", "readonly"))
	d.Blocks[0].Untrusted = []string{"inbound-email"}
	d.Blocks[1].TaintExempt = "fixed amount"
	d.TaintPolicy = gatePolicy
	code, out, errs := runMain("-declarations", writeFlow(t, "f.json", d))
	if code != 0 {
		t.Fatalf("code %d: %s", code, errs)
	}
	for _, want := range []string{"flow  taint policy: an escalation gate", "3 block(s)", "block pay", "is exempt from the escalation gate", "block look", "declares no provenance"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "digest") {
		t.Errorf("a flow report names a digest:\n%s", out)
	}
}

// No policy given: the report says the description gives none, not that the flow has none.
func TestFlowDeclarations_NoPolicyGiven(t *testing.T) {
	d := flowDesc(blk("pay", "side_effect"))
	code, out, _ := runMain("-declarations", writeFlow(t, "f.json", d))
	if code != 0 || !strings.Contains(out, "taint policy not given to the description") || !strings.Contains(out, "gives no taint policy") {
		t.Fatalf("code %d:\n%s", code, out)
	}
}

// A version 1 description is read and says taint is not known: never "no declarations".
func TestFlowDeclarations_Version1IsNotKnown(t *testing.T) {
	d := flowDesc(blk("pay", "side_effect"))
	d.Version, d.TaintDeclared = 1, false
	code, out, _ := runMain("-declarations", writeFlow(t, "f.json", d))
	if code != 0 || !strings.Contains(out, "taint policy not known") || !strings.Contains(out, "carries no taint declarations") || strings.Contains(out, "nothing to look at") {
		t.Fatalf("code %d:\n%s", code, out)
	}
}

func TestFlowDeclarations_RefusesWhatDoesNotRead(t *testing.T) {
	good, _ := json.Marshal(flowDesc(blk("a", "readonly")))
	for name, content := range map[string]string{
		"unknown member": strings.Replace(string(good), `"version":2`, `"version":2,"future":1`, 1),
		"version 3":      strings.Replace(string(good), `"version":2`, `"version":3`, 1),
		"no marker":      strings.Replace(string(good), `"taint_declared":true,`, ``, 1),
		"both sources":   strings.Replace(string(good), `"safety":"readonly"`, `"safety":"readonly","untrusted":["x"],"trusted":"y"`, 1),
		"duplicate":      strings.Replace(string(good), `"version":2`, `"version":2,"version":2`, 1),
	} {
		if code, _, errs := runMain("-declarations", writeRaw(t, "f.json", content)); code != 1 || errs == "" {
			t.Errorf("%s: code %d, stderr %q; want 1 and a message", name, code, errs)
		}
	}
}

func TestFlowDiff_Weakened(t *testing.T) {
	old := flowDesc(blk("fetch", "side_effect"), blk("pay", "side_effect"))
	old.Blocks[0].Untrusted = []string{"crm", "inbound-email"}
	old.TaintPolicy = gatePolicy
	newer := flowDesc(blk("fetch", "side_effect"), blk("pay", "side_effect"))
	newer.Blocks[0].Untrusted = []string{"crm"}
	newer.Blocks[1].TaintExempt = "reviewed"
	newer.TaintPolicy = &plan.TaintPolicyDescription{Gate: &plan.GateDescription{Need: 1, Approvers: []string{"a", "b"}}}
	o, n := writeFlow(t, "o.json", old), writeFlow(t, "n.json", newer)
	code, out, _ := runMain("-diff", o, n)
	for _, want := range []string{`Untrusted("inbound-email") removed`, `TaintExempt("reviewed") added`, "need lowered from 2 to 1", "the requester is no longer excluded", "4 change(s) weaken"} {
		if !strings.Contains(out, want) {
			t.Errorf("the diff lacks %q:\n%s", want, out)
		}
	}
	if code != 1 || strings.Contains(out, "digest") {
		t.Errorf("code %d:\n%s", code, out)
	}
	// The other way is only a strengthening.
	code, out, _ = runMain("-diff", n, o)
	if code != 0 || strings.Contains(out, "WEAKENED") || !strings.Contains(out, "strengthened") {
		t.Errorf("reverse: code %d:\n%s", code, out)
	}
}

func TestFlowDiff_PolicyAndProvenanceRules(t *testing.T) {
	base := func() plan.RegistryDescription {
		d := flowDesc(blk("fetch", "side_effect"))
		d.TaintPolicy = &plan.TaintPolicyDescription{Refuse: true, RequireProvenance: true}
		return d
	}
	for name, c := range map[string]struct {
		edit func(*plan.RegistryDescription)
		want string
	}{
		"policy removed":     {func(d *plan.RegistryDescription) { d.TaintPolicy = nil }, "the taint policy is removed"},
		"refuse to gate":     {func(d *plan.RegistryDescription) { d.TaintPolicy.Refuse, d.TaintPolicy.Gate = false, gatePolicy.Gate }, "an escalation gate, was refuse"},
		"provenance dropped": {func(d *plan.RegistryDescription) { d.TaintPolicy.RequireProvenance = false }, "no longer required to declare a provenance"},
		"block removed":      {func(d *plan.RegistryDescription) { d.Blocks[0].Untrusted = nil }, ""},
		"now readonly":       {func(d *plan.RegistryDescription) { d.Blocks[0].Safety = "readonly" }, "no longer has side effects"},
		"now idempotent":     {func(d *plan.RegistryDescription) { d.Blocks[0].Safety = "idempotent" }, "now Idempotent"},
	} {
		old := base()
		old.Blocks[0].Untrusted = []string{"s"}
		newer := base()
		newer.Blocks[0].Untrusted = []string{"s"}
		c.edit(&newer)
		code, out, _ := runMain("-diff", writeFlow(t, "o.json", old), writeFlow(t, "n.json", newer))
		if name == "block removed" {
			c.want = `Untrusted("s") removed`
		}
		if code != 1 || !strings.Contains(out, c.want) {
			t.Errorf("%s: code %d, want WEAKENED with %q:\n%s", name, code, c.want, out)
		}
	}
	// A removed block that declared a source, and a merge named like a block.
	old := base()
	old.Blocks[0].Untrusted = []string{"s"}
	newer := flowDesc()
	newer.TaintPolicy = old.TaintPolicy
	if code, out, _ := runMain("-diff", writeFlow(t, "o.json", old), writeFlow(t, "n.json", newer)); code != 1 || !strings.Contains(out, "removed; it declared untrusted source(s) s") {
		t.Errorf("removed block: code %d:\n%s", code, out)
	}
	m := flowDesc(blk("x", "readonly"))
	m.Merges = []plan.MergeDescription{{Name: "x", Inputs: []string{"a", "b"}, Out: "c", Safety: "side_effect", Untrusted: []string{"j"}}}
	m2 := flowDesc(blk("x", "readonly"))
	m2.Merges = []plan.MergeDescription{{Name: "x", Inputs: []string{"a", "b"}, Out: "c", Safety: "side_effect"}}
	if code, out, _ := runMain("-diff", writeFlow(t, "o.json", m), writeFlow(t, "n.json", m2)); code != 1 || !strings.Contains(out, "merge x") || !strings.Contains(out, `Untrusted("j") removed`) {
		t.Errorf("merge: code %d:\n%s", code, out)
	}
}

// Descriptions that do not both carry the declarations cannot be compared for taint: weakened, and the rest is listed.
func TestFlowDiff_VersionMismatchFailsClosed(t *testing.T) {
	v2 := flowDesc(blk("fetch", "side_effect"))
	v2.Blocks[0].Untrusted = []string{"s"}
	v1 := flowDesc(blk("fetch", "side_effect"))
	v1.Version, v1.TaintDeclared = 1, false
	for _, pair := range [][2]plan.RegistryDescription{{v2, v1}, {v1, v2}, {v1, v1}} {
		code, out, _ := runMain("-diff", writeFlow(t, "o.json", pair[0]), writeFlow(t, "n.json", pair[1]))
		if code != 1 || !strings.Contains(out, "cannot be compared") {
			t.Errorf("code %d:\n%s", code, out)
		}
	}
}

func TestFlowDiff_EqualAndMixedKinds(t *testing.T) {
	d := flowDesc(blk("a", "readonly"))
	d.TaintPolicy = gatePolicy
	p := writeFlow(t, "f.json", d)
	if code, out, _ := runMain("-diff", p, p); code != 0 || !strings.Contains(out, "no difference") {
		t.Errorf("equal: code %d:\n%s", code, out)
	}
	if code, _, errs := runMain("-diff", p, golden); code != 1 || !strings.Contains(errs, "cannot be compared") {
		t.Errorf("mixed kinds: code %d, %q", code, errs)
	}
}

// A nil taint_policy is "not given", in the header and in the findings, never "no policy".
func TestFlowDeclarations_NilPolicyIsNotGiven(t *testing.T) {
	d := flowDesc(blk("fetch", "side_effect"))
	d.Blocks[0].Untrusted = []string{"inbound-email"}
	_, out, _ := runMain("-declarations", writeFlow(t, "f.json", d))
	for _, want := range []string{"taint policy not given to the description", "the description gives no taint policy", "block fetch", "declares untrusted source(s) inbound-email and the description gives no taint policy"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "has no taint policy") || strings.Contains(out, "runs without a policy.") {
		t.Errorf("the report says the flow has no policy:\n%s", out)
	}
}

func TestFlowDeclarations_Version1SaysOnlyThat(t *testing.T) {
	d := flowDesc(blk("pay", "side_effect"))
	d.Version, d.TaintDeclared = 1, false
	_, out, _ := runMain("-declarations", writeFlow(t, "f.json", d))
	if !strings.Contains(out, "regenerate it with this version") || strings.Contains(out, "gives no taint policy") {
		t.Errorf("version 1 report:\n%s", out)
	}
}

func TestFlowDiff_BlockAndPolicyDetails(t *testing.T) {
	base := func() plan.RegistryDescription {
		d := flowDesc(blk("pay", "side_effect"))
		d.TaintPolicy = &plan.TaintPolicyDescription{Refuse: true}
		return d
	}
	run := func(old, newer plan.RegistryDescription) (int, string) {
		code, out, _ := runMain("-diff", writeFlow(t, "o.json", old), writeFlow(t, "n.json", newer))
		return code, out
	}
	// A block added already exempt is weakened.
	added := base()
	added.Blocks = append(added.Blocks, blk("zap", "side_effect"))
	added.Blocks[1].TaintExempt = "reviewed"
	if code, out := run(base(), added); code != 1 || !strings.Contains(out, `added with TaintExempt ("reviewed")`) {
		t.Errorf("added exempt: code %d:\n%s", code, out)
	}
	// A reworded exemption is weakened.
	a, b := base(), base()
	a.Blocks[0].TaintExempt, b.Blocks[0].TaintExempt = "one", "two"
	if code, out := run(a, b); code != 1 || !strings.Contains(out, `TaintExempt reason changed to "two"`) {
		t.Errorf("reworded exempt: code %d:\n%s", code, out)
	}
	// require_provenance turned on is a strengthening, turned off a weakening.
	on := base()
	on.TaintPolicy.RequireProvenance = true
	if code, out := run(base(), on); code != 0 || strings.Contains(out, "WEAKENED") || !strings.Contains(out, "every block must now declare a provenance") {
		t.Errorf("provenance on: code %d:\n%s", code, out)
	}
	// A merge that becomes Idempotent.
	m1, m2 := flowDesc(), flowDesc()
	m1.Merges = []plan.MergeDescription{{Name: "j", Inputs: []string{"a", "b"}, Out: "c", Safety: "side_effect"}}
	m2.Merges = []plan.MergeDescription{{Name: "j", Inputs: []string{"a", "b"}, Out: "c", Safety: "idempotent"}}
	if code, out := run(m1, m2); code != 1 || !strings.Contains(out, "merge j") || !strings.Contains(out, "now Idempotent") {
		t.Errorf("merge idempotent: code %d:\n%s", code, out)
	}
	// A flow diff names no digest, even for a change an agent's report calls "not in the digest".
	ro := base()
	ro.Blocks[0].Safety = "readonly"
	if _, out := run(base(), ro); strings.Contains(out, "not in the digest") || !strings.Contains(out, "no longer has side effects") {
		t.Errorf("uncovered change:\n%s", out)
	}
}

// A block's own gate is compared like a tool's.
func TestFlowDiff_OwnGate(t *testing.T) {
	a, b := flowDesc(blk("pay", "side_effect")), flowDesc(blk("pay", "side_effect"))
	a.Blocks[0].Gate = &plan.GateDescription{Need: 2, Approvers: []string{"a", "b"}}
	b.Blocks[0].Gate = &plan.GateDescription{Need: 1, Approvers: []string{"a", "b"}}
	code, out, _ := runMain("-diff", writeFlow(t, "o.json", a), writeFlow(t, "n.json", b))
	if code != 1 || !strings.Contains(out, "block pay own gate") || !strings.Contains(out, "need lowered from 2 to 1") {
		t.Errorf("code %d:\n%s", code, out)
	}
}
