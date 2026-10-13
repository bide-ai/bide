package plan

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// payConfig is a structural fixture: intake routes a big payment through review (a 2-of-3 gate)
// to charge, and any other straight to charge; charge then notifies, and the run ends at done.
// orphan is declared and never wired, and audit is wired after done with no stated safety.
const payConfig = `{
  "version": 1,
  "flow": "pay",
  "entry": "intake",
  "nodes": [
    {"name": "intake", "block": "intake", "safety": "readonly"},
    {"name": "review", "block": "review", "safety": "readonly", "approval": {"need": 2, "approvers": ["ops", "finance", "risk"]}},
    {"name": "charge", "block": "charge", "safety": "side_effect"},
    {"name": "notify", "block": "notify", "safety": "idempotent"},
    {"name": "done",   "block": "done",   "safety": "readonly"},
    {"name": "orphan", "block": "orphan", "safety": "readonly"}
  ],
  "wiring": [
    {"switch": "intake", "when": [{"pred": "big", "to": "review"}], "else": "charge"},
    {"edge": ["review", "charge"]},
    {"edge": ["charge", "notify"]},
    {"edge": ["notify", "done"]}
  ],
  "assert": ASSERT
}`

// withAsserts is config with its ASSERT placeholder replaced by lines.
func withAsserts(t *testing.T, config string, lines ...string) []byte {
	t.Helper()
	b, err := json.Marshal(lines)
	if err != nil {
		t.Fatal(err)
	}
	return []byte(strings.Replace(config, "ASSERT", string(b), 1))
}

// verdict is the one assertion a single-line config yields, failing the test on any other count.
func verdict(t *testing.T, data []byte, reg *Registry) (AssertionResult, ConfigReport) {
	t.Helper()
	r := InspectConfig(data, reg)
	if len(r.Assertions) != 1 {
		t.Fatalf("assertions = %+v; want one (problems %+v)", r.Assertions, r.Problems)
	}
	return r.Assertions[0], r
}

// TestAssert_EachKind: every kind of assertion, held and violated, with the shortest
// counterexample for a violated one and an error problem attached to the path's last node.
func TestAssert_EachKind(t *testing.T) {
	cases := []struct {
		line string
		path []string // nil: holds
	}{
		{"every path to charge passes approval >= 2", []string{"intake", "charge"}},
		{"every path to review passes approval >= 2 of 3", nil}, // review's own gate guards it
		{"every path to done passes charge", nil},
		{"every path to done passes review", []string{"intake", "charge", "notify", "done"}},
		{"every path to a terminal passes notify", nil},
		{"every path to a terminal passes review", []string{"intake", "charge", "notify", "done"}},
		{"no side effect after charge", nil},
		{"no side effect after intake", []string{"intake", "charge"}},
		{"orphan unreachable", nil},
		{"charge unreachable", []string{"intake", "charge"}},
		{"done reachable", nil},
		{`"done"   reachable`, nil}, // quoted, spaces collapsed
	}
	for _, c := range cases {
		res, r := verdict(t, withAsserts(t, payConfig, c.line), nil)
		if res.Invalid || res.Undecided || res.Holds != (c.path == nil) || !slices.Equal(res.Path, c.path) {
			t.Errorf("%q: %+v; want holds=%v path %v", c.line, res, c.path == nil, c.path)
			continue
		}
		if c.path == nil {
			if r.Errors() {
				t.Errorf("%q holds but problems %+v", c.line, r.Problems)
			}
			continue
		}
		if !res.Violated() {
			t.Errorf("%q: Violated() = false for %+v", c.line, res)
		}
		ps := problemAt(r, c.path[len(c.path)-1], "", "")
		if len(ps) != 1 || ps[0].Severity != "error" || !strings.Contains(ps[0].Message, "is violated") || !strings.Contains(ps[0].Message, strings.Join(c.path, " -> ")) {
			t.Errorf("%q: problems at %q = %+v", c.line, c.path[len(c.path)-1], ps)
		}
	}
}

// TestAssert_OrphanReachable: "X reachable" that does not hold has no counterexample path and an
// error on X.
func TestAssert_OrphanReachable(t *testing.T) {
	res, r := verdict(t, withAsserts(t, payConfig, "orphan reachable"), nil)
	if !res.Violated() || res.Path != nil || len(problemAt(r, "orphan", "", "")) != 1 {
		t.Fatalf("%+v; problems %+v", res, r.Problems)
	}
}

// TestAssert_GateStrictness: a gate counts toward "approval >= K of N" only when it needs K or
// more decisions from at most N approvers.
func TestAssert_GateStrictness(t *testing.T) {
	gated := strings.Replace(payConfig, `"else": "charge"`, `"else": "review"`, 1)
	for line, holds := range map[string]bool{
		"every path to charge passes approval >= 1":      true,
		"every path to charge passes approval >= 2":      true,
		"every path to charge passes approval >= 2 of 3": true,
		"every path to charge passes approval >= 2 of 5": true,
		"every path to charge passes approval >= 3":      false, // review needs only 2
		"every path to charge passes approval >= 2 of 2": false, // review has 3 approvers
	} {
		res, _ := verdict(t, withAsserts(t, gated, line), nil)
		if res.Holds != holds {
			t.Errorf("%q: %+v; want holds=%v", line, res, holds)
		}
		if !holds && !slices.Equal(res.Path, []string{"intake", "review", "charge"}) {
			t.Errorf("%q: path %v", line, res.Path)
		}
	}
}

// assertLoopConfig: draft and check form a bounded loop (check's loop-back arm returns to draft), and
// send follows the loop.
const assertLoopConfig = `{
  "version": 1,
  "flow": "loop",
  "entry": "plan",
  "nodes": [
    {"name": "plan",  "block": "plan",  "safety": "readonly"},
    {"name": "draft", "block": "draft", "safety": "DRAFT"},
    {"name": "check", "block": "check", "safety": "readonly"},
    {"name": "send",  "block": "send",  "safety": "side_effect"}
  ],
  "wiring": [
    {"edge": ["plan", "draft"]},
    {"edge": ["draft", "check"]},
    {"switch": "check", "when": [{"pred": "again", "to": "draft", "loop_max": 3}], "else": "send"}
  ],
  "assert": ASSERT
}`

// TestAssert_Loops: a loop is decided by reachability (it terminates and visits the loop once),
// and a side effect a loop returns to counts as after itself.
func TestAssert_Loops(t *testing.T) {
	readonly := strings.Replace(assertLoopConfig, "DRAFT", "readonly", 1)
	side := strings.Replace(assertLoopConfig, "DRAFT", "side_effect", 1)
	cases := []struct {
		config, line string
		path         []string
	}{
		{readonly, "every path to send passes check", nil},
		{readonly, "every path to draft passes plan", nil},
		{readonly, "every path to a terminal passes draft", nil},
		{readonly, "no side effect after draft", []string{"draft", "check", "send"}},
		{readonly, "no side effect after send", nil},
		{side, "no side effect after draft", []string{"draft", "check", "draft"}},
		{side, "every path to send passes approval >= 1", []string{"plan", "draft", "check", "send"}},
	}
	for _, c := range cases {
		res, _ := verdict(t, withAsserts(t, c.config, c.line), nil)
		if res.Holds != (c.path == nil) || !slices.Equal(res.Path, c.path) {
			t.Errorf("%q: %+v; want path %v", c.line, res, c.path)
		}
	}
}

// TestAssert_Undecided: "no side effect after X" with a node of unknown safety after X and no
// known side effect is undecided, and an error; a known side effect decides it.
func TestAssert_Undecided(t *testing.T) {
	unknown := strings.Replace(payConfig, `{"name": "notify", "block": "notify", "safety": "idempotent"}`, `{"name": "notify", "block": "notify"}`, 1)
	res, r := verdict(t, withAsserts(t, unknown, "no side effect after charge"), nil)
	if !res.Undecided || res.Holds || res.Violated() || !strings.Contains(res.Message, `"notify"`) || !r.Errors() {
		t.Fatalf("%+v; problems %+v", res, r.Problems)
	}
	if res, _ := verdict(t, withAsserts(t, unknown, "no side effect after intake"), nil); !res.Violated() {
		t.Fatalf("a known side effect after intake: %+v", res)
	}
}

// TestAssert_Invalid: an ill-formed assertion, an unknown one, and one naming an undeclared node
// are each an invalid result and an error problem, and the others still decide.
func TestAssert_Invalid(t *testing.T) {
	lines := []string{
		"every path to charge passes approval >= 0",
		"every path to charge passes approval >= 3 of 2",
		"charge is safe",
		"every path to chrage passes review",
		"no side effect after nowhere",
		"done reachable",
	}
	r := InspectConfig(withAsserts(t, payConfig, lines...), nil)
	if len(r.Assertions) != len(lines) {
		t.Fatalf("assertions = %+v", r.Assertions)
	}
	errs := 0
	for _, p := range r.Problems {
		if p.Severity == "error" && strings.HasPrefix(p.Message, "assert[") {
			errs++
		}
	}
	for i, a := range r.Assertions[:5] {
		if !a.Invalid || a.Holds || a.Violated() || a.Message == "" {
			t.Errorf("assert[%d] %+v; want invalid", i, a)
		}
	}
	if !r.Assertions[5].Holds || errs != 5 {
		t.Fatalf("last %+v; %d assert errors in %+v", r.Assertions[5], errs, r.Problems)
	}
	if !strings.Contains(r.Assertions[3].Message, `"chrage"`) {
		t.Fatalf("undeclared node message: %q", r.Assertions[3].Message)
	}
}

// TestAssert_LoadIgnoresAssert: Load accepts a config with an assert list, even a violated one,
// and the digest does not commit to it.
func TestAssert_LoadIgnoresAssert(t *testing.T) {
	plain := triageConfig
	asserted := strings.Replace(triageConfig, `"wiring": [`, `"assert": ["decline unreachable"], "wiring": [`, 1)
	a, err := Load[cfgOrder, cfgReceipt]([]byte(plain), triageRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Load[cfgOrder, cfgReceipt]([]byte(asserted), triageRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest() != b.Digest() {
		t.Fatal("the digest commits to assert")
	}
	if r := InspectConfig([]byte(asserted), triageRegistry(t)); !r.Errors() || r.Digest != a.Digest() {
		t.Fatalf("the violated assertion: errors %v digest %q", r.Errors(), r.Digest)
	}
}

// riskRegistry registers the triage blocks with a class each: classify read-only, reserve a side
// effect behind a 2-of-2 gate declared in Go, finalize a side effect, decline idempotent.
func riskRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	for _, err := range []error{
		reg.RegisterStep("classify", cfgClassify, ReadOnly()),
		reg.RegisterStep("reserve", cfgReserve, Approval(&agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance"}})),
		reg.RegisterStep("finalize", cfgFinalize),
		reg.RegisterStep("decline", cfgDecline, Idempotent()),
		reg.RegisterPredicate("rush", func(a cfgAssessment) bool { return a.Rush }),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

// TestInspectConfig_Safety: with the Registry each node's class is the registered Safety, lowered
// by the config's; without it a class is known only where the config states it.
func TestInspectConfig_Safety(t *testing.T) {
	data := []byte(strings.Replace(triageConfig, `{"name": "decline",  "block": "decline"}`, `{"name": "decline",  "block": "decline", "safety": "side_effect"}`, 1))
	typed := InspectConfig([]byte(triageConfig), riskRegistry(t))
	if typed.Errors() {
		t.Fatal(typed.Problems)
	}
	want := map[string]NodeSafety{"classify": SafetyReadOnly, "reserve": SafetySideEffect, "finalize": SafetySideEffect, "decline": SafetyIdempotent}
	for n, s := range want {
		if typed.Safety[n] != s {
			t.Errorf("typed %s: %q, want %q", n, typed.Safety[n], s)
		}
	}
	if lowered := InspectConfig(data, riskRegistry(t)); lowered.Safety["decline"] != SafetySideEffect {
		t.Errorf("a config safety lowers the registered class: %q", lowered.Safety["decline"])
	}
	standalone := InspectConfig(data, nil)
	for n, s := range map[string]NodeSafety{"classify": SafetyUnknown, "reserve": SafetyUnknown, "decline": SafetySideEffect} {
		if standalone.Safety[n] != s {
			t.Errorf("standalone %s: %q, want %q", n, standalone.Safety[n], s)
		}
	}
	if !SafetyReadOnly.RerunsOnCrash() || !SafetyIdempotent.RerunsOnCrash() || SafetySideEffect.RerunsOnCrash() || SafetyUnknown.RerunsOnCrash() {
		t.Error("RerunsOnCrash")
	}
}

// TestAssert_RegistryGate: a gate a block declares in Go counts with the Registry; without it only
// the config's gates count, so the same assertion is violated.
func TestAssert_RegistryGate(t *testing.T) {
	data := []byte(strings.Replace(triageConfig, `"wiring": [`, `"assert": ["every path to finalize passes approval >= 2 of 2"], "wiring": [`, 1))
	if res, r := verdict(t, data, riskRegistry(t)); !res.Holds || r.Errors() {
		t.Fatalf("typed: %+v; problems %+v", res, r.Problems)
	}
	if res, _ := verdict(t, data, nil); !res.Violated() || !slices.Equal(res.Path, []string{"classify", "reserve", "finalize"}) {
		t.Fatalf("standalone: %+v", res)
	}
}

// TestAssert_GatedEntry: the entry is on every path, so a gated entry guards every node, and
// Gated reports every node; an ungated entry leaves the nodes before a gate ungated.
func TestAssert_GatedEntry(t *testing.T) {
	gatedEntry := strings.Replace(payConfig, `{"name": "intake", "block": "intake", "safety": "readonly"}`, `{"name": "intake", "block": "intake", "safety": "readonly", "approval": {"need": 2, "approvers": ["ops", "finance"]}}`, 1)
	res, r := verdict(t, withAsserts(t, gatedEntry, "every path to done passes approval >= 2"), nil)
	if !res.Holds {
		t.Fatalf("%+v", res)
	}
	for _, n := range []string{"intake", "charge", "done"} {
		if !r.Gated[n] {
			t.Errorf("gated entry: %s not gated", n)
		}
	}
	_, r = verdict(t, withAsserts(t, payConfig, "done reachable"), nil)
	for n, want := range map[string]bool{"intake": false, "review": true, "charge": false, "done": false} {
		if r.Gated[n] != want {
			t.Errorf("%s: gated %v, want %v", n, r.Gated[n], want)
		}
	}
}

// TestAssert_SingleApproval: a one-decision gate (declared in Go) counts for "approval >= 1" and
// for no "of N" or higher K.
func TestAssert_SingleApproval(t *testing.T) {
	reg := NewRegistry()
	for _, err := range []error{
		reg.RegisterStep("classify", cfgClassify, ReadOnly()),
		reg.RegisterStep("reserve", cfgReserve, Approval(agent.SingleApproval())),
		reg.RegisterStep("finalize", cfgFinalize),
		reg.RegisterStep("decline", cfgDecline),
		reg.RegisterPredicate("rush", func(a cfgAssessment) bool { return a.Rush }),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	for line, holds := range map[string]bool{
		"every path to finalize passes approval >= 1":      true,
		"every path to finalize passes approval >= 1 of 5": false,
		"every path to finalize passes approval >= 2":      false,
	} {
		data := []byte(strings.Replace(triageConfig, `"wiring": [`, `"assert": [`+strconvQuote(line)+`], "wiring": [`, 1))
		if res, _ := verdict(t, data, reg); res.Holds != holds {
			t.Errorf("%q: %+v; want holds=%v", line, res, holds)
		}
	}
}

func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }
