package plan

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// Flow taint is specified in docs/design/flow-taint.md. This file holds the declaration side: the
// NodeOptions, the per-node config key, the flow's policy, and the guard that refuses all of it until
// the engine enforces it (the next change removes the guard, and with it TestTaintGuard_*).

func stepInt(context.Context, int) (int, error) { return 1, nil }

// built is a one-node flow with the given options on its step, and the node as built.
func builtNode(t *testing.T, opts ...NodeOption) (*Builder[int, int], *node) {
	t.Helper()
	b := New[int, int]("f")
	b.Step[int, int]("s", stepInt, opts...)
	return b, b.core.byName["s"]
}

func TestNodeOptions_RecordDeclarations(t *testing.T) {
	_, n := builtNode(t, Untrusted("inbound-email"), TaintExempt("reviewed by security"))
	if got := n.taint.untrusted; len(got) != 1 || got[0] != "inbound-email" {
		t.Errorf("untrusted = %v, want [inbound-email]", got)
	}
	if n.taint.exempt != "reviewed by security" || n.taint.trusted != "" {
		t.Errorf("taint = %+v", n.taint)
	}
	_, n = builtNode(t, Trusted("the host's own table"))
	if n.taint.trusted != "the host's own table" || len(n.taint.untrusted) != 0 {
		t.Errorf("taint = %+v", n.taint)
	}
	// Two sources are both kept, sorted, once each.
	_, n = builtNode(t, Untrusted("b"), Untrusted("a"), Untrusted("b"))
	if got := strings.Join(n.taint.untrusted, ","); got != "a,b" {
		t.Errorf("untrusted = %q, want a,b", got)
	}
	// A node that declares nothing declares nothing.
	_, n = builtNode(t)
	if n.taint.any() {
		t.Errorf("an undeclared node has taint %+v", n.taint)
	}
}

func TestNodeOptions_RefuseWhatAgentRefuses(t *testing.T) {
	long := strings.Repeat("x", 300)
	cases := map[string][]NodeOption{
		"empty source":             {Untrusted("")},
		"over-long source":         {Untrusted(long)},
		"control character":        {Untrusted("a\x00b")},
		"empty reason":             {Trusted("")},
		"empty exemption":          {TaintExempt("")},
		"untrusted then trusted":   {Untrusted("a"), Trusted("mine")},
		"trusted then untrusted":   {Trusted("mine"), Untrusted("a")},
		"two different trusted":    {Trusted("one"), Trusted("two")},
		"two different exemptions": {TaintExempt("one"), TaintExempt("two")},
	}
	for what, opts := range cases {
		b := New[int, int]("f")
		b.Step[int, int]("s", stepInt, opts...)
		if _, err := b.Build(); !errors.Is(err, agent.ErrConfig) || strings.Contains(err.Error(), "not enforced") {
			t.Errorf("%s: Build = %v, want an ErrConfig about the option, not the guard", what, err)
		}
	}
	// The same declaration twice is not a conflict.
	b, _ := builtNode(t, Trusted("mine"), Trusted("mine"), TaintExempt("r"), TaintExempt("r"))
	if len(b.core.errs) != 0 {
		t.Errorf("a repeated identical declaration recorded %v", b.core.errs)
	}
}

func TestToolNode_TakesItsSpecsDeclarations(t *testing.T) {
	cases := map[string]struct {
		tool agent.Tool
		want nodeTaint
	}{
		"Untrusted":        {agent.MustFunc("t", "", stepInt, agent.Untrusted("crm-notes")), nodeTaint{untrusted: []string{"crm-notes"}}},
		"Safety source":    {agent.MustFunc("t", "", stepInt, agent.WithSafety(agent.Safety{Untrusted: "inbox"})), nodeTaint{untrusted: []string{"inbox"}}},
		"TaintExempt":      {agent.MustFunc("t", "", stepInt, agent.TaintExempt("reviewed")), nodeTaint{exempt: "reviewed"}},
		"Trusted":          {agent.MustFunc("t", "", stepInt, agent.Trusted("our table")), nodeTaint{trusted: "our table"}},
		"declares nothing": {agent.MustFunc("t", "", stepInt), nodeTaint{}},
	}
	for what, c := range cases {
		b := New[int, int]("f")
		b.Tool[int, int]("t", c.tool)
		if len(b.core.errs) != 0 {
			t.Errorf("%s: Tool recorded %v", what, b.core.errs)
			continue
		}
		got := b.core.byName["t"].taint
		if strings.Join(got.untrusted, ",") != strings.Join(c.want.untrusted, ",") || got.trusted != c.want.trusted || got.exempt != c.want.exempt {
			t.Errorf("%s: taint = %+v, want %+v", what, got, c.want)
		}
	}
	// An option adds to a tool's source, and cannot contradict its other declarations.
	b := New[int, int]("f")
	b.Tool[int, int]("t", agent.MustFunc("t", "", stepInt, agent.Untrusted("a")), Untrusted("b"))
	if got := strings.Join(b.core.byName["t"].taint.untrusted, ","); got != "a,b" {
		t.Errorf("untrusted = %q, want a,b", got)
	}
	b = New[int, int]("f")
	b.Tool[int, int]("t", agent.MustFunc("t", "", stepInt, agent.Untrusted("a")), Trusted("mine"))
	if len(b.core.errs) == 0 {
		t.Error("a Trusted option on an Untrusted tool was accepted")
	}
	b = New[int, int]("f")
	b.Tool[int, int]("t", agent.MustFunc("t", "", stepInt, agent.TaintExempt("one")), TaintExempt("two"))
	if len(b.core.errs) == 0 {
		t.Error("a TaintExempt option that disagrees with the tool's was accepted")
	}
}

func TestRegistry_BlocksCarryDeclarations(t *testing.T) {
	reg := NewRegistry()
	if err := reg.RegisterStep[int, int]("fetch", stepInt, Untrusted("inbound-email")); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterModel[int, int]("ask", "p", Trusted("fixed prompt")); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterTool[int, int]("look", agent.MustFunc("look", "", stepInt, agent.Untrusted("crm")), Untrusted("web")); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterJoin2[int, int, int]("merge", func(_ context.Context, a, b int) (int, error) { return a + b, nil }, TaintExempt("sums")); err != nil {
		t.Fatal(err)
	}
	if got := reg.blocks["fetch"].taint.untrusted; len(got) != 1 || got[0] != "inbound-email" {
		t.Errorf("fetch = %v", got)
	}
	if reg.blocks["ask"].taint.trusted != "fixed prompt" {
		t.Errorf("ask = %+v", reg.blocks["ask"].taint)
	}
	if got := strings.Join(reg.blocks["look"].taint.untrusted, ","); got != "crm,web" {
		t.Errorf("look = %q, want crm,web", got)
	}
	if reg.merges["merge"].taint.exempt != "sums" {
		t.Errorf("merge = %+v", reg.merges["merge"].taint)
	}
	// A bad option is recorded on the Registry, as a bad gate is, so Load reports it.
	reg = NewRegistry()
	if err := reg.RegisterStep[int, int]("fetch", stepInt, Untrusted("a"), Trusted("b")); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("RegisterStep with a contradiction = %v, want ErrConfig", err)
	}
	if len(reg.errs) != 1 {
		t.Errorf("the Registry recorded %d errors, want 1", len(reg.errs))
	}
}

// taintGuard is the one place a flow with taint declarations or a policy is refused until the engine
// enforces them. The test names the shape of the refusal, so the change that enforces taint removes
// it on purpose: it must fail there, and be deleted with the guard.
func TestTaintGuard_RefusesDeclarationsAndPolicyUntilEnforced(t *testing.T) {
	policy := agent.TaintPolicy{Refuse: true}
	cases := map[string]func() error{
		"a step's Untrusted": func() error {
			b := New[int, int]("f")
			b.Step[int, int]("s", stepInt, Untrusted("inbox"))
			_, err := b.Build()
			return err
		},
		"a step's Trusted": func() error {
			b := New[int, int]("f")
			b.Step[int, int]("s", stepInt, Trusted("ours"))
			_, err := b.Build()
			return err
		},
		"a step's TaintExempt": func() error {
			b := New[int, int]("f")
			b.Step[int, int]("s", stepInt, TaintExempt("ok"))
			_, err := b.Build()
			return err
		},
		"a tool's Untrusted": func() error {
			b := New[int, int]("f")
			b.Tool[int, int]("s", agent.MustFunc("s", "", stepInt, agent.Untrusted("crm")))
			_, err := b.Build()
			return err
		},
		"a policy alone": func() error {
			b := New[int, int]("f")
			b.Step[int, int]("s", stepInt)
			_, err := b.WithTaintPolicy(policy).Build()
			return err
		},
	}
	for what, build := range cases {
		err := build()
		if !errors.Is(err, agent.ErrConfig) || err == nil || !strings.Contains(err.Error(), "flow taint is not enforced yet") {
			t.Errorf("%s: Build = %v, want ErrConfig \"flow taint is not enforced yet\"", what, err)
		}
	}
	// A flow that declares nothing and sets no policy is not touched by it.
	b := New[int, int]("f")
	b.Step[int, int]("s", stepInt)
	if _, err := b.Build(); err != nil {
		t.Errorf("a flow with no taint declarations: Build = %v", err)
	}
	// The refusal names the node, so an author can find it.
	b = New[int, int]("f")
	b.Step[int, int]("fetch_mail", stepInt, Untrusted("inbox"))
	if _, err := b.Build(); err == nil || !strings.Contains(err.Error(), "fetch_mail") {
		t.Errorf("the refusal does not name the node: %v", err)
	}
}

func TestWithTaintPolicy_ChecksThePolicyAsAgentDoes(t *testing.T) {
	bad := map[string]agent.TaintPolicy{
		"neither a gate nor Refuse": {},
		"a gate and Refuse":         {Gate: agent.SingleApproval(), Refuse: true},
		"an invalid gate":           {Gate: &agent.ApprovalPolicy{Need: 3, Approvers: []string{"a"}}},
	}
	for what, p := range bad {
		b := New[int, int]("f")
		b.Step[int, int]("s", stepInt)
		_, err := b.WithTaintPolicy(p).Build()
		if !errors.Is(err, agent.ErrConfig) || err == nil || strings.Contains(err.Error(), "not enforced") {
			t.Errorf("%s: Build = %v, want ErrConfig about the policy, not the guard", what, err)
		}
	}
	// A valid policy reaches the guard, which is where it stops for now.
	b := New[int, int]("f")
	b.Step[int, int]("s", stepInt)
	if _, err := b.WithTaintPolicy(agent.TaintPolicy{Gate: agent.SingleApproval()}).Build(); err == nil || !strings.Contains(err.Error(), "not enforced") {
		t.Errorf("a valid policy: Build = %v, want the guard", err)
	}
}

const taintConfig = `{
  "version": 1,
  "flow": "f",
  "nodes": [{"name": "fetch", "block": "fetch"%s}],
  "wiring": []
}`

func taintRegistry(t *testing.T, opts ...NodeOption) *Registry {
	t.Helper()
	reg := NewRegistry()
	if err := reg.RegisterStep[int, int]("fetch", stepInt, opts...); err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestConfigKey_Untrusted(t *testing.T) {
	// The key adds a source to the node, in the loaded flow's node.
	cfg := []byte(strings.Replace(taintConfig, "%s", `, "untrusted": "inbound-email"`, 1))
	core, err := assemble(mustParse(t, cfg), taintRegistry(t), false)
	if err != nil {
		t.Fatal(err)
	}
	if got := core.byName["fetch"].taint.untrusted; len(got) != 1 || got[0] != "inbound-email" {
		t.Errorf("untrusted = %v", got)
	}
	// It is added to the block's own source, never replacing it.
	core, err = assemble(mustParse(t, cfg), taintRegistry(t, Untrusted("crm")), false)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(core.byName["fetch"].taint.untrusted, ","); got != "crm,inbound-email" {
		t.Errorf("untrusted = %q, want crm,inbound-email", got)
	}
	// Omitted, it leaves the block's declarations as they are and changes nothing.
	core, err = assemble(mustParse(t, []byte(strings.Replace(taintConfig, "%s", "", 1))), taintRegistry(t, Untrusted("crm")), false)
	if err != nil {
		t.Fatal(err)
	}
	if got := core.byName["fetch"].taint.untrusted; len(got) != 1 || got[0] != "crm" {
		t.Errorf("untrusted = %v", got)
	}
	// The key cannot contradict the block's Trusted, or carry an invalid name.
	for what, c := range map[string]struct {
		key  string
		opts []NodeOption
	}{
		"the block is Trusted": {`, "untrusted": "inbox"`, []NodeOption{Trusted("ours")}},
		"a control character":  {`, "untrusted": "a\u0000b"`, nil},
		"over-long":            {`, "untrusted": "` + strings.Repeat("x", 300) + `"`, nil},
	} {
		_, err := assemble(mustParse(t, []byte(strings.Replace(taintConfig, "%s", c.key, 1))), taintRegistry(t, c.opts...), false)
		if !errors.Is(err, agent.ErrConfig) || err == nil || !strings.Contains(err.Error(), "fetch") {
			t.Errorf("%s: assemble = %v, want ErrConfig naming the node", what, err)
		}
	}
	// A value that is not a string is a parse error, as any other key's type is.
	var c config
	if err := parseConfig([]byte(strings.Replace(taintConfig, "%s", `, "untrusted": 3`, 1)), &c); err == nil {
		t.Error("a numeric untrusted parsed")
	}
}

func TestConfigKey_UntrustedOnAJoin(t *testing.T) {
	reg := NewRegistry()
	for _, n := range []string{"a", "b"} {
		if err := reg.RegisterStep[int, int](n, stepInt); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.RegisterJoin2[int, int, int]("merge", func(_ context.Context, x, y int) (int, error) { return x + y, nil }); err != nil {
		t.Fatal(err)
	}
	cfg := `{"version":1,"flow":"f","nodes":[{"name":"a","block":"a"},{"name":"b","block":"b"}],
 "wiring":[{"join":"j","inputs":["a","b"],"merge":"merge","untrusted":"feed"}]}`
	core, err := assemble(mustParse(t, []byte(cfg)), reg, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := core.byName["j"].taint.untrusted; len(got) != 1 || got[0] != "feed" {
		t.Errorf("join untrusted = %v", got)
	}
}

func TestTaintGuard_AppliesToLoadAndCheck(t *testing.T) {
	cfg := []byte(strings.Replace(taintConfig, "%s", `, "untrusted": "inbound-email"`, 1))
	// Load refuses a config that declares a source.
	if _, err := Load[int, int](cfg, taintRegistry(t)); !errors.Is(err, agent.ErrConfig) || err == nil || !strings.Contains(err.Error(), "not enforced yet") {
		t.Errorf("Load = %v, want the guard", err)
	}
	// So does a block's declaration with a plain config, and a policy through the load option.
	plain := []byte(strings.Replace(taintConfig, "%s", "", 1))
	if _, err := Load[int, int](plain, taintRegistry(t, Untrusted("crm"))); err == nil || !strings.Contains(err.Error(), "not enforced yet") {
		t.Errorf("Load with a declared block = %v, want the guard", err)
	}
	if _, err := Load[int, int](plain, taintRegistry(t), WithLoadedTaintPolicy(agent.TaintPolicy{Refuse: true})); err == nil || !strings.Contains(err.Error(), "not enforced yet") {
		t.Errorf("Load with a policy = %v, want the guard", err)
	}
	// An invalid policy through the load option is the policy's error.
	if _, err := Load[int, int](plain, taintRegistry(t), WithLoadedTaintPolicy(agent.TaintPolicy{})); !errors.Is(err, agent.ErrConfig) || err == nil || strings.Contains(err.Error(), "not enforced") {
		t.Errorf("Load with an empty policy = %v, want the policy's ErrConfig", err)
	}
	// A config with none of it loads as before.
	if _, err := Load[int, int](plain, taintRegistry(t)); err != nil {
		t.Errorf("Load with no taint = %v", err)
	}
	// bide check agrees with Load: the report carries the refusal as an error, so CI and Load do not differ.
	r := InspectConfig(cfg, taintRegistry(t))
	if !r.Errors() {
		t.Error("InspectConfig of a config Load refuses reports no error")
	}
}

func mustParse(t *testing.T, data []byte) *config {
	t.Helper()
	var c config
	if err := parseConfig(data, &c); err != nil {
		t.Fatal(err)
	}
	return &c
}
