package plan

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	amodel "github.com/bide-ai/bide/plan/internal/digesttypes/a/model"
	bmodel "github.com/bide-ai/bide/plan/internal/digesttypes/b/model"
)

// DigestV1 is the digest this package recorded before v2, byte for byte, so a proof of a
// flow:digest record journaled by an earlier version can still be checked. These values were
// computed by Digest on the commit before v2.
func TestDigestV1_IsTheEarlierDigest(t *testing.T) {
	f, err := Load[cfgOrder, cfgReceipt]([]byte(triageConfig), triageRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	d, err := buildDiamond()
	if err != nil {
		t.Fatal(err)
	}
	l, err := buildCountdownLoop(10, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ got, want string }{
		"triage":  {f.DigestV1(), "214f9dd3f266ae37d791f234afe74597be830d67aa6b0b07fbc89891580483e9"},
		"diamond": {d.DigestV1(), "abff47b96d371ae093b0eab9c7d3007c630fe3a29283a3264c6be00668a01124"},
		"loop":    {l.DigestV1(), "be9f490258e2902c964fba16ff13ab9f9edd2ed4f40c789d46ee8bbbc4d60eec"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: DigestV1 = %s, want the v1 digest %s", name, tc.got, tc.want)
		}
	}
	if f.Digest() == f.DigestV1() {
		t.Error("Digest equals DigestV1")
	}
}

// v1RecordedRun journals a flow:digest record holding digest, as a run started by an earlier
// version would have, and returns the store.
func v1RecordedRun(t *testing.T, runID, digest string) *agent.MemStore {
	t.Helper()
	store := agent.NewMemStore()
	b, _ := json.Marshal(digest)
	if _, err := store.Do(context.Background(), runID, flowDigestStep, func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: b}, nil
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

// A run journaled under v1 is not resumed, even when its v1 digest matches this flow: v1 does
// not commit to block or predicate names, so it cannot show the run started under this flow.
// The error names the version so an operator knows why.
func TestRun_RefusesARunRecordedUnderV1(t *testing.T) {
	f := loadNames(t, "rush", "approve")
	store := v1RecordedRun(t, "old", f.DigestV1())
	_, err := f.Run(context.Background(), store, "old", cfgOrder{ID: 1, Rush: true})
	if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "v1") {
		t.Fatalf("Run of a v1 run: err = %v, want ErrConfig naming v1", err)
	}
	ok, diffs, err := f.Conform(context.Background(), store, "old")
	if err != nil {
		t.Fatal(err)
	}
	if ok || len(diffs) != 1 || !strings.Contains(diffs[0], "v1") {
		t.Errorf("Conform of a v1 run = %v %v, want one divergence naming v1", ok, diffs)
	}
	// A digest that is neither this flow's v2 nor its v1 is the other divergence.
	other := v1RecordedRun(t, "other", strings.Repeat("0", 64))
	if _, err := f.Run(context.Background(), other, "other", cfgOrder{ID: 1}); !errors.Is(err, agent.ErrConfig) || strings.Contains(err.Error(), "v1") {
		t.Errorf("Run of a foreign digest: err = %v, want ErrConfig not naming v1", err)
	}
	ok, diffs, _ = f.Conform(context.Background(), other, "other")
	if ok || len(diffs) != 1 || !strings.Contains(diffs[0], "different topology") {
		t.Errorf("Conform of a foreign digest = %v %v", ok, diffs)
	}
}

// In Go, Arm.Named and BlockName give a flow the names a config gives it: naming a predicate
// or a block changes the digest, and the same names give the same digest.
func TestDigest_GoNamesParticipate(t *testing.T) {
	build := func(pred string, opts ...NodeOption) string {
		b := New[int, int]("g")
		s := b.Step("s", func(_ context.Context, n int) (int, error) { return n, nil }, opts...)
		yes := b.Step("yes", func(_ context.Context, n int) (int, error) { return n, nil })
		no := b.Step("no", func(_ context.Context, n int) (int, error) { return n, nil })
		arm := When(func(n int) bool { return n > 0 }, yes)
		if pred != "" {
			arm = arm.Named(pred)
		}
		b.Switch(s, arm, Else(no))
		f, err := b.Build()
		if err != nil {
			t.Fatal(err)
		}
		return f.Digest()
	}
	unnamed := build("")
	if build("") != unnamed || build("pos") != build("pos") {
		t.Fatal("digest not deterministic")
	}
	if build("pos") == unnamed || build("pos") == build("neg") {
		t.Error("a predicate name does not change the digest")
	}
	if build("", BlockName("s")) != unnamed {
		t.Error("a node's block did not default to its name")
	}
	if build("", BlockName("other")) == unnamed {
		t.Error("BlockName does not change the digest")
	}
}

// canonicalType spells every named type with its package path, through every kind of
// composite type, so two types that print alike are told apart wherever they appear. Its exact
// spelling is part of the pinned v2 digest, so a few spellings are checked verbatim.
func TestCanonicalType_QualifiesEveryNamedPart(t *testing.T) {
	type pair struct {
		a, b reflect.Type
	}
	for name, p := range map[string]pair{
		"named":            {reflect.TypeFor[amodel.Req](), reflect.TypeFor[bmodel.Req]()},
		"pointer":          {reflect.TypeFor[*amodel.Req](), reflect.TypeFor[*bmodel.Req]()},
		"slice":            {reflect.TypeFor[[]amodel.Req](), reflect.TypeFor[[]bmodel.Req]()},
		"array":            {reflect.TypeFor[[2]amodel.Req](), reflect.TypeFor[[2]bmodel.Req]()},
		"map key":          {reflect.TypeFor[map[amodel.Req]int](), reflect.TypeFor[map[bmodel.Req]int]()},
		"map value":        {reflect.TypeFor[map[int]amodel.Req](), reflect.TypeFor[map[int]bmodel.Req]()},
		"chan":             {reflect.TypeFor[chan amodel.Req](), reflect.TypeFor[chan bmodel.Req]()},
		"struct":           {reflect.TypeFor[struct{ R amodel.Req }](), reflect.TypeFor[struct{ R bmodel.Req }]()},
		"func in":          {reflect.TypeFor[func(amodel.Req)](), reflect.TypeFor[func(bmodel.Req)]()},
		"func out":         {reflect.TypeFor[func() amodel.Req](), reflect.TypeFor[func() bmodel.Req]()},
		"variadic":         {reflect.TypeFor[func(...amodel.Req)](), reflect.TypeFor[func(...bmodel.Req)]()},
		"interface":        {reflect.TypeFor[interface{ M(amodel.Req) }](), reflect.TypeFor[interface{ M(bmodel.Req) }]()},
		"unexported field": {reflect.TypeFor[amodel.Anon](), reflect.TypeFor[bmodel.Anon]()},
	} {
		if p.a.String() != p.b.String() {
			t.Fatalf("%s: fixture types print differently: %s, %s", name, p.a, p.b)
		}
		if canonicalType(p.a) == canonicalType(p.b) {
			t.Errorf("%s: both spelled %s", name, canonicalType(p.a))
		}
	}
	// Other distinctions a spelling must keep.
	for name, p := range map[string]pair{
		"array len":     {reflect.TypeFor[[2]int](), reflect.TypeFor[[3]int]()},
		"chan dir recv": {reflect.TypeFor[<-chan int](), reflect.TypeFor[chan int]()},
		"chan dir send": {reflect.TypeFor[chan<- int](), reflect.TypeFor[chan int]()},
		"field name":    {reflect.TypeFor[struct{ A int }](), reflect.TypeFor[struct{ B int }]()},
		"field tag": {reflect.TypeFor[struct {
			A int `json:"a"`
		}](), reflect.TypeFor[struct {
			A int `json:"b"`
		}]()},
		"embedded":      {reflect.TypeFor[struct{ amodel.Req }](), reflect.TypeFor[struct{ Req amodel.Req }]()},
		"field count":   {reflect.TypeFor[struct{ A, B int }](), reflect.TypeFor[struct{ A int }]()},
		"variadic flag": {reflect.TypeFor[func(...int)](), reflect.TypeFor[func([]int)]()},
		"func arity":    {reflect.TypeFor[func(int, int)](), reflect.TypeFor[func(int)]()},
		"func results":  {reflect.TypeFor[func() (int, int)](), reflect.TypeFor[func() int]()},
		"method name":   {reflect.TypeFor[interface{ M() }](), reflect.TypeFor[interface{ N() }]()},
		"method count": {reflect.TypeFor[interface {
			M()
			N()
		}](), reflect.TypeFor[interface{ M() }]()},
		"unexported": {reflect.TypeFor[struct{ a int }](), reflect.TypeFor[struct{ A int }]()},
	} {
		if canonicalType(p.a) == canonicalType(p.b) {
			t.Errorf("%s: %s and %s both spelled %s", name, p.a, p.b, canonicalType(p.a))
		}
	}
	for typ, want := range map[reflect.Type]string{
		nil:                            "?",
		reflect.TypeFor[int]():         "int",
		reflect.TypeFor[amodel.Req]():  "github.com/bide-ai/bide/plan/internal/digesttypes/a/model.Req",
		reflect.TypeFor[[]*int]():      "[]*int",
		reflect.TypeFor[map[int]int](): "map[int]int",
		reflect.TypeFor[struct {
			A int `json:"a"`
			B string
		}](): `struct{A int "json:\"a\""; B string}`,
		reflect.TypeFor[func(int, ...string) (bool, error)](): "func(int, ...string) (bool, error)",
		reflect.TypeFor[interface{ M(int) }]():                "interface{M func(int) ()}",
		reflect.TypeFor[<-chan int]():                         "<-chan int",
	} {
		if got := canonicalType(typ); got != want {
			t.Errorf("canonicalType(%v) = %q, want %q", typ, got, want)
		}
	}
}

// The v2 digest is pinned: Run refuses to resume a run whose recorded digest differs, so any
// change to what v2 hashes or how would strand every run in flight. A deliberate change is a new
// version with its own domain tag.
func TestDigest_V2IsPinned(t *testing.T) {
	f, err := Load[cfgOrder, cfgReceipt]([]byte(triageConfig), triageRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	d, err := Load[int, string]([]byte(diamondConfig), diamondRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	l, err := buildCountdownLoop(10, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A join whose ports take a package's type, so the pin covers how join port types are spelled.
	jb := New[amodel.Req, amodel.Req]("typed-join")
	s := jb.Step("s", func(_ context.Context, r amodel.Req) (amodel.Req, error) { return r, nil })
	x := jb.Step("x", func(_ context.Context, r amodel.Req) (amodel.Req, error) { return r, nil })
	y := jb.Step("y", func(_ context.Context, r amodel.Req) (amodel.Req, error) { return r, nil })
	jb.Edge(s, x)
	jb.Edge(s, y)
	jb.Join2("j", x, y, func(_ context.Context, a, b amodel.Req) (amodel.Req, error) { return a, nil })
	j, err := jb.Build()
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ got, want string }{
		"typed join": {j.Digest(), "767461e6cd5079abb974e1675a8d9d47d4c4841218040c79c8e04dd568dbd10e"},
		"triage":     {f.Digest(), "33ee7c0fec5c34cd049036d4aa2f3539844b611c436043933c1bed35a1ae57aa"},
		"diamond":    {d.Digest(), "8c39e1f6f0801ce0ba98abd43732a2562524b118662f026b29ff10489727a988"},
		"loop":       {l.Digest(), "2c61743ebff8f64a5ee9e04fc8d582c5a7e8b77cd1dbd37a1a195b391edea228"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: Digest = %s, want the pinned v2 digest %s", name, tc.got, tc.want)
		}
	}
}
