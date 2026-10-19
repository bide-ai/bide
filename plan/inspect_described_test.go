package plan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// describedCase is a registry and a valid config for it, the seed of the differential test.
type describedCase struct {
	name string
	reg  func(*testing.T) *Registry
	cfg  string
}

func describedCases() []describedCase {
	return []describedCase{
		{"triage", triageRegistry, triageConfig},
		{"diamond", diamondRegistry, diamondConfig},
		{"loop", loopRegistry, loopConfig},
		{"three inputs", func(t *testing.T) *Registry {
			r := NewRegistry()
			step := func(_ context.Context, n int) (int, error) { return n, nil }
			for _, n := range []string{"s", "x", "y", "z"} {
				if err := r.RegisterStep(n, step); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.RegisterJoin3("m3", func(_ context.Context, a, b, c int) (string, error) { return "", nil }, Idempotent()); err != nil {
				t.Fatal(err)
			}
			return r
		}, `{"version":1,"flow":"j3","entry":"s","nodes":[{"name":"s","block":"s"},{"name":"x","block":"x"},{"name":"y","block":"y"},{"name":"z","block":"z"}],
		 "wiring":[{"edge":["s","x"]},{"edge":["s","y"]},{"edge":["s","z"]},{"join":"j","inputs":["x","y","z"],"merge":"m3"}]}`},
		{"every kind", func(t *testing.T) *Registry {
			r := describedRegistry(t, false)
			delete(r.merges, "both")
			delete(r.merges, "all3")
			return r
		}, describedFlowConfig},
	}
}

// shortToCanonical maps each spelling reflect's String gives a registered type to the spelling
// the description holds, the one difference InspectConfigDescribed documents.
func shortToCanonical(reg *Registry) map[string]string {
	m := map[string]string{}
	add := func(t reflect.Type) {
		if t != nil && t.String() != canonicalType(t) {
			m[t.String()] = canonicalType(t)
		}
	}
	for _, b := range reg.blocks {
		add(b.inType)
		add(b.outType)
	}
	for _, p := range reg.preds {
		add(p.mType)
	}
	for _, mg := range reg.merges {
		for _, t := range mg.inTypes {
			add(t)
		}
		add(mg.outType)
	}
	return m
}

// normalized is the report as JSON with every type spelling replaced by a token that stands for
// the type, so the real and the described report compare equal exactly when they agree on
// everything but the spelling.
func normalized(t *testing.T, r ConfigReport, reg *Registry) string {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	short := shortToCanonical(reg)
	var canon []string
	for _, c := range short {
		canon = append(canon, c)
	}
	sort.Slice(canon, func(i, j int) bool {
		return len(canon[i]) > len(canon[j]) || len(canon[i]) == len(canon[j]) && canon[i] < canon[j]
	})
	for i, c := range canon {
		s = strings.ReplaceAll(s, c, fmt.Sprintf("<T%d>", i))
	}
	var shorts []string
	for k := range short {
		shorts = append(shorts, k)
	}
	sort.Slice(shorts, func(i, j int) bool {
		return len(shorts[i]) > len(shorts[j]) || len(shorts[i]) == len(shorts[j]) && shorts[i] < shorts[j]
	})
	for _, k := range shorts {
		for i, c := range canon {
			if short[k] == c {
				s = strings.ReplaceAll(s, k, fmt.Sprintf("<T%d>", i))
			}
		}
	}
	return s
}

// variants derives configs from a valid one that exercise each check: another block on a node,
// another safety, a gate, a missing node or wiring element, a reversed edge, another predicate or
// merge, a join's inputs swapped, and assertions.
func variants(t *testing.T, cfg string, reg *Registry) []string {
	t.Helper()
	var base map[string]any
	if err := json.Unmarshal([]byte(cfg), &base); err != nil {
		t.Fatal(err)
	}
	clone := func() map[string]any {
		var m map[string]any
		b, _ := json.Marshal(base)
		_ = json.Unmarshal(b, &m)
		return m
	}
	out := []string{cfg}
	add := func(m map[string]any) {
		b, _ := json.Marshal(m)
		out = append(out, string(b))
	}
	blocks := []string{"nope"}
	for n := range reg.blocks {
		blocks = append(blocks, n)
	}
	preds := []string{"nope"}
	for n := range reg.preds {
		preds = append(preds, n)
	}
	merges := []string{"nope"}
	for n := range reg.merges {
		merges = append(merges, n)
	}
	sort.Strings(blocks)
	sort.Strings(preds)
	sort.Strings(merges)
	nodes, _ := base["nodes"].([]any)
	wiring, _ := base["wiring"].([]any)
	for i := range nodes {
		for _, b := range blocks {
			m := clone()
			m["nodes"].([]any)[i].(map[string]any)["block"] = b
			add(m)
		}
		for _, s := range []string{"readonly", "idempotent", "side_effect", "bogus"} {
			m := clone()
			m["nodes"].([]any)[i].(map[string]any)["safety"] = s
			add(m)
		}
		for _, ap := range []map[string]any{
			{"need": 1, "approvers": []any{"a"}},
			{"need": 2, "approvers": []any{"a", "b"}, "exclude_principal": true, "timeout": "2h"},
			{"need": 3, "approvers": []any{"a"}},
		} {
			m := clone()
			m["nodes"].([]any)[i].(map[string]any)["approval"] = ap
			add(m)
		}
		m := clone()
		ns := m["nodes"].([]any)
		m["nodes"] = append(ns[:i:i], ns[i+1:]...)
		add(m)
	}
	for i, w := range wiring {
		m := clone()
		ws := m["wiring"].([]any)
		m["wiring"] = append(ws[:i:i], ws[i+1:]...)
		add(m)
		el := w.(map[string]any)
		if e, ok := el["edge"].([]any); ok {
			m := clone()
			m["wiring"].([]any)[i].(map[string]any)["edge"] = []any{e[1], e[0]}
			add(m)
		}
		if when, ok := el["when"].([]any); ok {
			for j := range when {
				for _, p := range preds {
					m := clone()
					m["wiring"].([]any)[i].(map[string]any)["when"].([]any)[j].(map[string]any)["pred"] = p
					add(m)
				}
			}
		}
		if _, ok := el["join"]; ok {
			for _, mg := range merges {
				m := clone()
				m["wiring"].([]any)[i].(map[string]any)["merge"] = mg
				add(m)
			}
			in := el["inputs"].([]any)
			m := clone()
			m["wiring"].([]any)[i].(map[string]any)["inputs"] = []any{in[1], in[0]}
			add(m)
		}
	}
	if len(nodes) > 1 {
		first := nodes[0].(map[string]any)["name"].(string)
		last := nodes[len(nodes)-1].(map[string]any)["name"].(string)
		for _, a := range []string{
			"every path to a terminal passes " + first,
			"every path to " + last + " passes approval >= 1",
			"no side effect after " + first,
			last + " reachable",
		} {
			m := clone()
			m["assert"] = []any{a}
			add(m)
		}
	}
	return out
}

// InspectConfigDescribed over reg.Describe() must agree with InspectConfig over reg on every
// config, valid or broken: problems, safety, gates, assertions, topology and the digest. Only the
// spelling of types differs (see InspectConfigDescribed), which normalized removes.
func TestInspectConfigDescribedAgreesWithInspectConfig(t *testing.T) {
	total, typedOK := 0, 0
	seen := map[string]bool{}
	kinds := []string{"unknown block", "unknown predicate", "unknown merge", "connects ", "tests ", "registered but never used", "above the", "both switched-over", "approval", "mutually-exclusive", "declared more than once"}
	for _, c := range describedCases() {
		reg := c.reg(t)
		d := reg.Describe()
		for _, cfg := range variants(t, c.cfg, reg) {
			real := InspectConfig([]byte(cfg), reg)
			got := InspectConfigDescribed([]byte(cfg), d)
			if a, b := normalized(t, real, reg), normalized(t, got, reg); a != b {
				t.Fatalf("%s: the reports differ for\n%s\nreal:      %s\ndescribed: %s", c.name, cfg, a, b)
			}
			for _, p := range got.Problems {
				for _, k := range kinds {
					if strings.Contains(p.Message, k) {
						seen[k] = true
					}
				}
			}
			total++
			if real.Digest != "" {
				typedOK++
				if real.Digest != got.Digest {
					t.Fatalf("%s: digests differ for %s", c.name, cfg)
				}
			}
		}
	}
	for _, k := range kinds[:7] {
		if !seen[k] {
			t.Errorf("no config in the corpus drew the problem %q", k)
		}
	}
	if total < 250 || typedOK < len(describedCases()) {
		t.Errorf("the corpus is too thin: %d configs, %d with a digest", total, typedOK)
	}
	t.Logf("%d configs, %d typed-clean", total, typedOK)
}

// A description that is not valid is reported, not trusted; the config is then checked
// structurally.
func TestInspectConfigDescribedRefusesAnInvalidDescription(t *testing.T) {
	d := describedRegistry(t, false).Describe()
	d.Blocks[0].Kind = "wizard"
	r := InspectConfigDescribed([]byte(triageConfig), d)
	if r.Typed || !r.Errors() || !strings.HasPrefix(r.Problems[0].Message, "blocks description: block ") {
		t.Errorf("typed %v, problems %+v", r.Typed, r.Problems)
	}
}

// The limit the godoc states: two distinct types with one spelling are one type in a description.
// Load, comparing reflect types, refuses the edge; the described check accepts it.
func TestDescribedTypesAreToldApartByNameOnly(t *testing.T) {
	type T struct{ A int }
	reg := NewRegistry()
	_ = reg.RegisterStep("a", func(_ context.Context, n int) (T, error) { return T{}, nil })
	other := func() *Registry {
		type T struct{ B string }
		r := NewRegistry()
		_ = r.RegisterStep("b", func(_ context.Context, _ T) (int, error) { return 0, nil })
		return r
	}()
	// One registry cannot hold both, so describe both and merge the descriptions by hand.
	d := reg.Describe()
	d.Blocks = append(d.Blocks, other.Describe().Blocks...)
	cfg := `{"version":1,"flow":"x","entry":"a","nodes":[{"name":"a","block":"a"},{"name":"b","block":"b"}],"wiring":[{"edge":["a","b"]}]}`
	if r := InspectConfigDescribed([]byte(cfg), d); r.Errors() {
		t.Errorf("the described check refused: %+v", r.Problems)
	}
}

// Validate refuses each way a description read from a file can be wrong, and accepts what Describe
// writes.
func TestRegistryDescriptionValidate(t *testing.T) {
	good := describedRegistry(t, false).Describe()
	if err := good.Validate(); err != nil {
		t.Fatalf("a Describe result: %v", err)
	}
	long := strings.Repeat("x", 2000)
	cases := []struct {
		name   string
		edit   func(d *RegistryDescription)
		reason string
	}{
		{"version", func(d *RegistryDescription) { d.Version = 2 }, "version 2"},
		{"kind", func(d *RegistryDescription) { d.Blocks[0].Kind = "join" }, "kind"},
		{"block safety", func(d *RegistryDescription) { d.Blocks[0].Safety = "safe" }, "safety"},
		{"merge safety", func(d *RegistryDescription) { d.Merges[0].Safety = "" }, "safety"},
		{"empty name", func(d *RegistryDescription) { d.Blocks[0].Name = "" }, "no name"},
		{"control in name", func(d *RegistryDescription) { d.Blocks[0].Name = "a\nb" }, "control"},
		{"bidi in name", func(d *RegistryDescription) { d.Predicates[0].Name = "a‮b" }, "control or format"},
		{"invalid utf8", func(d *RegistryDescription) { d.Blocks[0].Name = "a\xffb" }, "control or format"},
		{"duplicate block", func(d *RegistryDescription) { d.Blocks[1].Name = d.Blocks[0].Name }, "twice"},
		{"duplicate predicate", func(d *RegistryDescription) { d.Predicates[1].Name = d.Predicates[0].Name }, "twice"},
		{"duplicate merge", func(d *RegistryDescription) { d.Merges[1].Name = d.Merges[0].Name }, "twice"},
		{"empty type", func(d *RegistryDescription) { d.Blocks[0].In = "" }, "empty"},
		{"long type", func(d *RegistryDescription) { d.Blocks[0].Out = long }, "longer"},
		{"switched type", func(d *RegistryDescription) { d.Predicates[0].Switched = "" }, "switched"},
		{"merge arity 1", func(d *RegistryDescription) { d.Merges[0].Inputs = d.Merges[0].Inputs[:1] }, "two or three"},
		{"merge arity 4", func(d *RegistryDescription) { d.Merges[0].Inputs = append(d.Merges[0].Inputs, "a", "b", "c") }, "two or three"},
		{"merge out", func(d *RegistryDescription) { d.Merges[0].Out = "" }, "output"},
		{"gate need", func(d *RegistryDescription) { d.Blocks[2].Gate.Need = 9 }, "gate"},
		{"gate approvers", func(d *RegistryDescription) { d.Blocks[2].Gate.Approvers = nil }, "gate"},
		{"gate timeout", func(d *RegistryDescription) { d.Blocks[2].Gate.Timeout = "soon" }, "timeout"},
		{"gate timeout negative", func(d *RegistryDescription) { d.Blocks[2].Gate.Timeout = "-1h" }, "timeout"},
		{"gate timeout fraction", func(d *RegistryDescription) { d.Blocks[2].Gate.Timeout = "1500us" }, "timeout"},
		{"single with approvers", func(d *RegistryDescription) { d.Blocks[5].Gate.Approvers = []string{"a"} }, "single"},
		{"too many entries", func(d *RegistryDescription) {
			for len(d.Predicates) <= maxDescribedEntries {
				d.Predicates = append(d.Predicates, PredicateDescription{Name: fmt.Sprint(len(d.Predicates)), Switched: "x"})
			}
		}, "entries"},
		{"too many approvers", func(d *RegistryDescription) {
			for i := 0; i <= maxGateApprovers; i++ {
				d.Blocks[2].Gate.Approvers = append(d.Blocks[2].Gate.Approvers, fmt.Sprint("a", i))
			}
		}, "approvers"},
		{"long approver id", func(d *RegistryDescription) { d.Blocks[2].Gate.Approvers[0] = long[:300] }, "approver id"},
		{"errors", func(d *RegistryDescription) { d.Errors = []string{`duplicate block registration "x"`} }, "reported 1 error"},
		{"errors, hostile text", func(d *RegistryDescription) { d.Errors = []string{"a\xff"} }, "(not shown)"},
	}
	for _, c := range cases {
		d := describedRegistry(t, false).Describe()
		c.edit(&d)
		err := d.Validate()
		if err == nil || !strings.Contains(err.Error(), c.reason) {
			t.Errorf("%s: %v; want an error with %q", c.name, err, c.reason)
		}
	}
}

// A description read from a file is arbitrary bytes: whatever JSON decodes into one, checking a
// config against it never panics.
func FuzzInspectConfigDescribed(f *testing.F) {
	d := describedRegistry(&testing.T{}, false).Describe()
	seed, _ := json.Marshal(d)
	f.Add(seed, describedFlowConfig)
	f.Add(seed, diamondConfig)
	f.Add([]byte(`{"version":1,"blocks":[{"name":"a","kind":"step","in":"x","out":"x","safety":"readonly"}],"predicates":[],"merges":[]}`), triageConfig)
	f.Fuzz(func(t *testing.T, desc []byte, cfg string) {
		var d RegistryDescription
		if json.Unmarshal(desc, &d) != nil {
			return
		}
		_ = InspectConfigDescribed([]byte(cfg), d)
	})
}

// A program whose Registry refused a registration does not load, so its description is refused
// with the reason: the registry's own error text reaches the reader.
func TestInspectConfigDescribedRefusesAProgramThatDoesNotLoad(t *testing.T) {
	reg := triageRegistry(t)
	if err := reg.RegisterStep("classify", cfgClassify); err == nil {
		t.Fatal("no duplicate error")
	}
	if !InspectConfig([]byte(triageConfig), reg).Errors() {
		t.Fatal("the real check passes a duplicate registration")
	}
	got := InspectConfigDescribed([]byte(triageConfig), reg.Describe())
	if got.Typed || !got.Errors() || !strings.Contains(got.Problems[0].Message, `duplicate block registration "classify"`) {
		t.Errorf("typed %v, problems %+v", got.Typed, got.Problems)
	}
}

// The Registry built from a description exists only to check: it holds nothing but stand-in types,
// so a check never compares a stand-in with a real type, and Load and Validate refuse it, so it
// cannot be run.
func TestDescribedRegistryIsInertAndHoldsOnlyStandIns(t *testing.T) {
	reg, err := registryFromDescription(describedRegistry(t, false).Describe())
	if err != nil {
		t.Fatal(err)
	}
	stand := func(ty reflect.Type) {
		t.Helper()
		if _, ok := describedTypeName(ty); !ok {
			t.Errorf("a real type %v in a described Registry", ty)
		}
	}
	for _, b := range reg.blocks {
		stand(b.inType)
		stand(b.outType)
		if b.run != nil {
			t.Error("a described block has behavior")
		}
	}
	for _, p := range reg.preds {
		stand(p.mType)
		if p.pred != nil {
			t.Error("a described predicate has behavior")
		}
	}
	for _, m := range reg.merges {
		for _, ty := range m.inTypes {
			stand(ty)
		}
		stand(m.outType)
		if m.merge != nil {
			t.Error("a described merge has behavior")
		}
	}
	if _, err := Load[dsIn, dsOut]([]byte(describedFlowConfig), reg, WithLoadedModel(&fakeModel{reply: `{}`})); err == nil || !errors.Is(err, agent.ErrConfig) {
		t.Errorf("Load of a described Registry: %v", err)
	}
	if err := Validate([]byte(describedFlowConfig), reg); err == nil {
		t.Error("Validate accepted a described Registry")
	}
}

// Whatever string a description holds as a type name, the stand-in type made for it carries exactly
// that string back and never panics, so a name cannot break out of the tag it is stored in.
func FuzzDescribedType(f *testing.F) {
	for _, s := range []string{"", "int", "a.B", `"`, "`", `\`, "\"x\" y:\"z\"", "\x00", "\xff\xfe", "\u202e", strings.Repeat("a", 5000), `name:"x"`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		ty := describedType(name)
		got, ok := describedTypeName(ty)
		if !ok || got != name {
			t.Fatalf("%q came back as %q (%v)", name, got, ok)
		}
		if describedType(name) != ty {
			t.Fatalf("%q: two stand-ins", name)
		}
		if canonicalType(ty) != name || typeName(ty) != name {
			t.Fatalf("%q written as %q / %q", name, canonicalType(ty), typeName(ty))
		}
	})
}

// A real program can register a name or a type the description's bounds refuse. Describe then says
// so in Errors, so a description without Errors is always one Validate accepts.
func TestDescribeReportsWhatValidateWouldRefuse(t *testing.T) {
	good := describedRegistry(t, false).Describe()
	if len(good.Errors) != 0 || good.Validate() != nil {
		t.Fatalf("a plain registry: %q %v", good.Errors, good.Validate())
	}
	reg := NewRegistry()
	if err := reg.RegisterStep(strings.Repeat("n", maxDescribedName+1), dsStep); err != nil {
		t.Fatal(err)
	}
	d := reg.Describe()
	if len(d.Errors) != 1 || !strings.Contains(d.Errors[0], "name is longer") || d.Validate() == nil {
		t.Errorf("long name: errors %q", d.Errors)
	}
	reg = NewRegistry()
	long := strings.Repeat("x", 1100)
	if err := reg.RegisterPredicate(long, func(dsMid) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if d := reg.Describe(); len(d.Errors) != 1 || d.Validate() == nil {
		t.Errorf("long predicate name: errors %q", d.Errors)
	}
}

// reflect.StructOf keeps what it makes, so the names a process turns into stand-ins are bounded as a
// whole: past the bound a description is refused with an error, and one whose names were all admitted
// before still checks.
func TestStandInNamesAreBoundedForTheProcess(t *testing.T) {
	standIns.Lock()
	names, bytes, mn, mb := standIns.names, standIns.bytes, maxStandInNames, maxStandInBytes
	standIns.names, standIns.bytes = map[string]bool{}, 0
	standIns.Unlock()
	defer func() {
		standIns.Lock()
		standIns.names, standIns.bytes, maxStandInNames, maxStandInBytes = names, bytes, mn, mb
		standIns.Unlock()
	}()
	maxStandInNames, maxStandInBytes = 8, 1<<20
	d := describedRegistry(t, false).Describe() // a handful of distinct type names
	if r := InspectConfigDescribed([]byte(describedFlowConfig), d); !r.Typed || r.Errors() {
		t.Fatalf("within the bound: %+v", r.Problems)
	}
	if r := InspectConfigDescribed([]byte(describedFlowConfig), d); !r.Typed || r.Errors() {
		t.Fatalf("the same names again cost nothing: %+v", r.Problems)
	}
	big := RegistryDescription{Version: 1, Blocks: []BlockDescription{}, Predicates: []PredicateDescription{}, Merges: []MergeDescription{}}
	for i := 0; i < 20; i++ {
		big.Blocks = append(big.Blocks, BlockDescription{Name: fmt.Sprint("b", i), Kind: "step", In: fmt.Sprint("t", i), Out: fmt.Sprint("u", i), Safety: "readonly"})
	}
	r := InspectConfigDescribed([]byte(triageConfig), big)
	if r.Typed || !r.Errors() || !strings.Contains(r.Problems[0].Message, "restart the process to check new type names") || r.Problems[0].Severity != "error" {
		t.Errorf("past the bound: typed %v, errors %v, %+v", r.Typed, r.Errors(), r.Problems)
	}
	maxStandInNames, maxStandInBytes = 1000, 20
	r = InspectConfigDescribed([]byte(triageConfig), big)
	if r.Typed || !strings.Contains(r.Problems[0].Message, "too many distinct type names") {
		t.Errorf("past the byte bound: typed %v, %+v", r.Typed, r.Problems)
	}
}

// Only the stand-in's own marker type makes a stand-in: a real type with the same shape written in
// another package is a real type, and is spelled as one.
func TestARealTypeWithTheStandInShapeIsNotAStandIn(t *testing.T) {
	type lookalike = struct {
		BideDescribedType struct{} `name:"payments.Refund"`
	}
	ty := reflect.TypeFor[lookalike]()
	if _, ok := describedTypeName(ty); ok {
		t.Fatal("a lookalike passes for a stand-in")
	}
	if canonicalType(ty) == "payments.Refund" || typeName(ty) == "payments.Refund" {
		t.Errorf("a real type is spelled %q / %q", canonicalType(ty), typeName(ty))
	}
}
