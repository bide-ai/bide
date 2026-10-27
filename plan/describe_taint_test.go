package plan

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// taintRegistry registers one block of each shape that declares taint, and one that declares none.
func describeTaintRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	for _, err := range []error{
		reg.RegisterStep("fetch", dsStep, Untrusted("inbound-email"), Untrusted("crm")),
		reg.RegisterStep("clean", dsLook, ReadOnly(), Trusted("host table")),
		reg.RegisterStep("pay", dsPay, TaintExempt("pays only a fixed amount")),
		reg.RegisterStep("plain", dsLook),
		reg.RegisterModel[dsMid, dsMid]("draft", "draft {{.}}", Untrusted("model-notes")),
		reg.RegisterTool[dsMid, dsOut]("refund", agent.MustFunc("refund", "refund", func(context.Context, dsMid) (dsOut, error) { return dsOut{}, nil }, agent.Untrusted("web"))),
		reg.RegisterJoin2("both", dsJoin, Untrusted("join-src"), TaintExempt("merge only")),
	} {
		if err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	return reg
}

func blockByName(d RegistryDescription, name string) BlockDescription {
	for _, b := range d.Blocks {
		if b.Name == name {
			return b
		}
	}
	return BlockDescription{}
}

func TestDescribeCarriesTaintDeclarations(t *testing.T) {
	d := describeTaintRegistry(t).Describe()
	if d.Version != 2 || !d.TaintDeclared {
		t.Fatalf("version %d, taint_declared %v; want 2, true", d.Version, d.TaintDeclared)
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("a Describe result is invalid: %v", err)
	}
	if d.TaintPolicy != nil {
		t.Errorf("Describe filled the policy, which a Registry does not hold: %+v", d.TaintPolicy)
	}
	want := map[string]BlockDescription{
		"fetch":  {Untrusted: []string{"crm", "inbound-email"}},
		"clean":  {Trusted: "host table"},
		"pay":    {TaintExempt: "pays only a fixed amount"},
		"plain":  {},
		"draft":  {Untrusted: []string{"model-notes"}},
		"refund": {Untrusted: []string{"web"}},
	}
	for name, w := range want {
		g := blockByName(d, name)
		if !reflect.DeepEqual(g.Untrusted, w.Untrusted) || g.Trusted != w.Trusted || g.TaintExempt != w.TaintExempt {
			t.Errorf("block %s: untrusted %v trusted %q exempt %q; want %v %q %q", name, g.Untrusted, g.Trusted, g.TaintExempt, w.Untrusted, w.Trusted, w.TaintExempt)
		}
	}
	if len(d.Merges) != 1 || !reflect.DeepEqual(d.Merges[0].Untrusted, []string{"join-src"}) || d.Merges[0].TaintExempt != "merge only" {
		t.Errorf("merge: %+v", d.Merges)
	}
}

// The JSON keys are the ones the design names, and a block that declares nothing writes none of them.
func TestDescribeTaintJSONKeys(t *testing.T) {
	b, err := json.Marshal(describeTaintRegistry(t).Describe())
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"taint_declared":true`, `"untrusted":["crm","inbound-email"]`, `"trusted":"host table"`, `"taint_exempt":"pays only a fixed amount"`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("description lacks %s:\n%s", k, b)
		}
	}
	plain, _ := json.Marshal(blockByName(describeTaintRegistry(t).Describe(), "plain"))
	for _, k := range []string{"untrusted", "trusted", "taint_exempt"} {
		if strings.Contains(string(plain), k) {
			t.Errorf("a block with no declaration writes %q: %s", k, plain)
		}
	}
}

// A description round-trips through JSON and an inert Registry built from it describes the same taint.
func TestDescribedTaintRoundTrips(t *testing.T) {
	d := describeTaintRegistry(t).Describe()
	p, err := DescribeTaintPolicy(agent.TaintPolicy{Gate: &agent.ApprovalPolicy{Need: 1, Approvers: []string{"sec"}}, RequireProvenance: true})
	if err != nil {
		t.Fatal(err)
	}
	d.TaintPolicy = p
	raw, _ := json.Marshal(d)
	var back RegistryDescription
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if err := back.Validate(); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	reg, err := registryFromDescription(back)
	if err != nil {
		t.Fatal(err)
	}
	again := reg.Describe()
	again.TaintPolicy = back.TaintPolicy
	a, _ := json.Marshal(again)
	if string(a) != string(raw) {
		t.Errorf("the inert Registry describes other taint:\n%s\n%s", a, raw)
	}
}

func TestDescribeTaintPolicy(t *testing.T) {
	gate := &agent.ApprovalPolicy{Need: 2, Approvers: []string{"a", "b"}}
	p, err := DescribeTaintPolicy(agent.TaintPolicy{Gate: gate, RequireProvenance: true})
	if err != nil || p.Refuse || !p.RequireProvenance || p.Gate == nil || p.Gate.Need != 2 {
		t.Fatalf("gate policy: %+v, %v", p, err)
	}
	p, err = DescribeTaintPolicy(agent.TaintPolicy{Refuse: true})
	if err != nil || !p.Refuse || p.Gate != nil {
		t.Fatalf("refuse policy: %+v, %v", p, err)
	}
	for name, bad := range map[string]agent.TaintPolicy{
		"empty":        {},
		"both":         {Gate: gate, Refuse: true},
		"invalid gate": {Gate: &agent.ApprovalPolicy{Need: 3, Approvers: []string{"a"}}},
	} {
		if p, err := DescribeTaintPolicy(bad); err == nil {
			t.Errorf("%s: accepted: %+v", name, p)
		}
	}
}

// A description of version 1 is read and says nothing about taint; it cannot carry the members.
func TestDescriptionVersion1HasNoTaint(t *testing.T) {
	v1 := func() RegistryDescription {
		d := describedRegistry(t, false).Describe()
		d.Version, d.TaintDeclared = 1, false
		return d
	}
	if d := v1(); d.Validate() != nil {
		t.Fatalf("version 1 is refused: %v", d.Validate())
	}
	var got RegistryDescription
	raw, _ := json.Marshal(v1())
	if err := json.Unmarshal(raw, &got); err != nil || got.Validate() != nil || got.TaintDeclared || got.TaintPolicy != nil {
		t.Fatalf("version 1 reads as %+v (%v)", got, err)
	}
	if _, err := registryFromDescription(got); err != nil {
		t.Errorf("a version 1 description does not build a Registry: %v", err)
	}
	for name, edit := range map[string]func(*RegistryDescription){
		"marker":       func(d *RegistryDescription) { d.TaintDeclared = true },
		"policy":       func(d *RegistryDescription) { d.TaintPolicy = &TaintPolicyDescription{Refuse: true} },
		"block":        func(d *RegistryDescription) { d.Blocks[0].Untrusted = []string{"x"} },
		"block trust":  func(d *RegistryDescription) { d.Blocks[0].Trusted = "x" },
		"block exempt": func(d *RegistryDescription) { d.Blocks[0].TaintExempt = "x" },
		"merge":        func(d *RegistryDescription) { d.Merges[0].Untrusted = []string{"x"} },
	} {
		d := v1()
		edit(&d)
		if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "version 1") {
			t.Errorf("version 1 with %s: %v; want a refusal naming version 1", name, err)
		}
	}
}

func TestDescriptionVersion2TaintValidate(t *testing.T) {
	long := strings.Repeat("x", 300)
	cases := []struct {
		name   string
		edit   func(*RegistryDescription)
		reason string
	}{
		{"no marker", func(d *RegistryDescription) { d.TaintDeclared = false }, "taint_declared"},
		{"bad source", func(d *RegistryDescription) { d.Blocks[0].Untrusted = []string{"a\nb"} }, "untrusted"},
		{"empty source", func(d *RegistryDescription) { d.Blocks[0].Untrusted = []string{""} }, "untrusted"},
		{"long source", func(d *RegistryDescription) { d.Blocks[0].Untrusted = []string{long} }, "untrusted"},
		{"unsorted", func(d *RegistryDescription) { d.Blocks[0].Untrusted = []string{"b", "a"} }, "sorted"},
		{"repeat", func(d *RegistryDescription) { d.Blocks[0].Untrusted = []string{"a", "a"} }, "sorted"},
		{"both", func(d *RegistryDescription) { d.Blocks[0].Untrusted, d.Blocks[0].Trusted = []string{"a"}, "why" }, "both"},
		{"bad trusted", func(d *RegistryDescription) { d.Blocks[0].Trusted = "a\x00" }, "trusted"},
		{"bad exempt", func(d *RegistryDescription) { d.Blocks[0].TaintExempt = long }, "taint_exempt"},
		{"merge source", func(d *RegistryDescription) { d.Merges[0].Untrusted = []string{"a\tb"} }, "merge"},
		{"merge both", func(d *RegistryDescription) { d.Merges[0].Untrusted, d.Merges[0].Trusted = []string{"a"}, "why" }, "merge"},
		{"empty policy", func(d *RegistryDescription) { d.TaintPolicy = &TaintPolicyDescription{} }, "taint policy"},
		{"policy both", func(d *RegistryDescription) {
			d.TaintPolicy = &TaintPolicyDescription{Refuse: true, Gate: &GateDescription{Need: 1, Approvers: []string{"a"}}}
		}, "taint policy"},
		{"policy bad gate", func(d *RegistryDescription) {
			d.TaintPolicy = &TaintPolicyDescription{Gate: &GateDescription{Need: 5, Approvers: []string{"a"}}}
		}, "taint policy"},
		{"policy bad timeout", func(d *RegistryDescription) {
			d.TaintPolicy = &TaintPolicyDescription{Gate: &GateDescription{Need: 1, Approvers: []string{"a"}, Timeout: "-1s"}}
		}, "taint policy"},
	}
	for _, c := range cases {
		d := describeTaintRegistry(t).Describe()
		c.edit(&d)
		if err := d.Validate(); err == nil || !strings.Contains(err.Error(), c.reason) {
			t.Errorf("%s: %v; want an error with %q", c.name, err, c.reason)
		}
	}
	d := describeTaintRegistry(t).Describe()
	d.TaintPolicy = &TaintPolicyDescription{Refuse: true, RequireProvenance: true}
	if err := d.Validate(); err != nil {
		t.Errorf("a refuse policy: %v", err)
	}
}
