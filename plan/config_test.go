package plan

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// The rung-2 test domain: a tiny triage flow. classify maps an Order to an
// Assessment; a switch on the Assessment routes rush orders to reserve (then
// finalize) and everything else to decline. Both finalize and decline produce a
// Receipt, the flow output.
type cfgOrder struct {
	ID   int  `json:"id"`
	Rush bool `json:"rush"`
}
type cfgAssessment struct {
	ID   int  `json:"id"`
	Rush bool `json:"rush"`
}
type cfgReservation struct {
	ID int `json:"id"`
}
type cfgReceipt struct {
	ID     int    `json:"id"`
	Status string `json:"status"`
}

func cfgClassify(_ context.Context, o cfgOrder) (cfgAssessment, error) {
	return cfgAssessment{ID: o.ID, Rush: o.Rush}, nil
}
func cfgReserve(_ context.Context, a cfgAssessment) (cfgReservation, error) {
	return cfgReservation{ID: a.ID}, nil
}
func cfgFinalize(_ context.Context, r cfgReservation) (cfgReceipt, error) {
	return cfgReceipt{ID: r.ID, Status: "reserved"}, nil
}
func cfgDecline(_ context.Context, a cfgAssessment) (cfgReceipt, error) {
	return cfgReceipt{ID: a.ID, Status: "declined"}, nil
}

// triageRegistry registers the full valid block/predicate set for the triage flow.
func triageRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	must(RegisterStep(reg, "classify", cfgClassify))
	must(RegisterStep(reg, "reserve", cfgReserve))
	must(RegisterStep(reg, "finalize", cfgFinalize))
	must(RegisterStep(reg, "decline", cfgDecline))
	must(RegisterPredicate(reg, "rush", func(a cfgAssessment) bool { return a.Rush }))
	return reg
}

const triageConfig = `{
  "flow": "triage",
  "in": "plan.cfgOrder",
  "out": "plan.cfgReceipt",
  "entry": "classify",
  "nodes": [
    {"name": "classify", "block": "classify"},
    {"name": "reserve",  "block": "reserve"},
    {"name": "finalize", "block": "finalize"},
    {"name": "decline",  "block": "decline"}
  ],
  "wiring": [
    {"switch": "classify", "when": [{"pred": "rush", "to": "reserve"}], "else": "decline"},
    {"edge": ["reserve", "finalize"]}
  ]
}`

// buildTriageByHand builds the SAME topology with the rung-1 builder, so its
// Digest can be compared against a Load of triageConfig, proving config
// conformance and digest stability.
func buildTriageByHand(t *testing.T) *Flow[cfgOrder, cfgReceipt] {
	t.Helper()
	b := New[cfgOrder, cfgReceipt]("triage")
	classify := b.Step("classify", cfgClassify)
	reserve := b.Step("reserve", cfgReserve)
	finalize := b.Step("finalize", cfgFinalize)
	decline := b.Step("decline", cfgDecline)
	b.Switch(classify,
		When(func(a cfgAssessment) bool { return a.Rush }, reserve).Named("rush"),
		Else(decline),
	)
	b.Edge(reserve, finalize)
	flow, err := b.Build()
	if err != nil {
		t.Fatalf("hand build: %v", err)
	}
	return flow
}

// TestLoadValidConfigRunsAndConforms loads the valid config, runs it against a
// MemStore, asserts the output, that Conform is ok, and that the loaded flow's
// Digest equals the hand-built flow of the same topology.
func TestLoadValidConfigRunsAndConforms(t *testing.T) {
	reg := triageRegistry(t)
	flow, err := Load[cfgOrder, cfgReceipt]([]byte(triageConfig), reg)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Digest equals the same topology built by hand.
	hand := buildTriageByHand(t)
	if flow.Digest() != hand.Digest() {
		t.Errorf("config digest %s != hand-built digest %s", flow.Digest(), hand.Digest())
	}

	// Run a rush order: classify -> (rush) reserve -> finalize -> Receipt{reserved}.
	ctx := context.Background()
	store := agent.NewMemStore()
	out, err := flow.Run(ctx, store, "run-rush", cfgOrder{ID: 7, Rush: true})
	if err != nil {
		t.Fatalf("Run rush: %v", err)
	}
	if out.ID != 7 || out.Status != "reserved" {
		t.Errorf("rush output = %+v, want {7 reserved}", out)
	}
	ok, diffs, err := flow.Conform(ctx, store, "run-rush")
	if err != nil {
		t.Fatalf("Conform: %v", err)
	}
	if !ok {
		t.Errorf("rush run did not conform: %v", diffs)
	}

	// Run a non-rush order: classify -> (else) decline -> Receipt{declined}.
	store2 := agent.NewMemStore()
	out2, err := flow.Run(ctx, store2, "run-plain", cfgOrder{ID: 9, Rush: false})
	if err != nil {
		t.Fatalf("Run plain: %v", err)
	}
	if out2.ID != 9 || out2.Status != "declined" {
		t.Errorf("plain output = %+v, want {9 declined}", out2)
	}
	ok2, diffs2, err := flow.Conform(ctx, store2, "run-plain")
	if err != nil {
		t.Fatalf("Conform plain: %v", err)
	}
	if !ok2 {
		t.Errorf("plain run did not conform: %v", diffs2)
	}
}

// TestDigestStableAcrossLoads loads the same config bytes twice and asserts the
// two flows have the same Digest, the property cryptographic conformance depends on.
func TestDigestStableAcrossLoads(t *testing.T) {
	f1, err := Load[cfgOrder, cfgReceipt]([]byte(triageConfig), triageRegistry(t))
	if err != nil {
		t.Fatalf("Load 1: %v", err)
	}
	f2, err := Load[cfgOrder, cfgReceipt]([]byte(triageConfig), triageRegistry(t))
	if err != nil {
		t.Fatalf("Load 2: %v", err)
	}
	if f1.Digest() != f2.Digest() {
		t.Errorf("digest not stable across loads: %s != %s", f1.Digest(), f2.Digest())
	}
}

// TestDriftUnknownBlockAndPredicate asserts an unknown block and an unknown
// predicate are BOTH reported in one error, each named.
func TestDriftUnknownBlockAndPredicate(t *testing.T) {
	reg := NewRegistry()
	// Deliberately omit "reserve" (unknown block) and "rush" (unknown predicate).
	_ = RegisterStep(reg, "classify", cfgClassify)
	_ = RegisterStep(reg, "finalize", cfgFinalize)
	_ = RegisterStep(reg, "decline", cfgDecline)

	_, err := Load[cfgOrder, cfgReceipt]([]byte(triageConfig), reg)
	if err == nil {
		t.Fatal("expected drift error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, `unknown block "reserve"`) {
		t.Errorf("error does not name unknown block: %s", msg)
	}
	if !strings.Contains(msg, `unknown predicate "rush"`) {
		t.Errorf("error does not name unknown predicate: %s", msg)
	}
}

// TestDriftSuggestsNearMiss asserts a typo'd block name gets a near-miss suggestion.
func TestDriftSuggestsNearMiss(t *testing.T) {
	reg := triageRegistry(t)
	// A config whose classify node references "classfy" (a one-edit typo).
	cfg := strings.Replace(triageConfig, `{"name": "classify", "block": "classify"}`,
		`{"name": "classify", "block": "classfy"}`, 1)
	_, err := Load[cfgOrder, cfgReceipt]([]byte(cfg), reg)
	if err == nil {
		t.Fatal("expected unknown-block error, got nil")
	}
	if !strings.Contains(err.Error(), `did you mean "classify"?`) {
		t.Errorf("expected near-miss suggestion, got: %s", err.Error())
	}
}

// TestPredicateTypeMismatch asserts a predicate whose M differs from the switched
// node's output type is a load error.
func TestPredicateTypeMismatch(t *testing.T) {
	reg := triageRegistry(t)
	// Re-register the rush predicate over the WRONG type (cfgOrder, not cfgAssessment)
	// under a distinct name, then point the switch at it.
	if err := RegisterPredicate(reg, "wrongrush", func(o cfgOrder) bool { return o.Rush }); err != nil {
		t.Fatalf("register: %v", err)
	}
	cfg := strings.Replace(triageConfig, `"pred": "rush"`, `"pred": "wrongrush"`, 1)
	_, err := Load[cfgOrder, cfgReceipt]([]byte(cfg), reg)
	if err == nil {
		t.Fatal("expected predicate-type mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "predicate") || !strings.Contains(err.Error(), "switched node") {
		t.Errorf("unexpected error: %s", err.Error())
	}
}

// TestEdgeTypeMismatch asserts an edge whose endpoints' types differ is a load error.
func TestEdgeTypeMismatch(t *testing.T) {
	// An edge classify(-> cfgAssessment) to finalize(cfgReservation -> ) does not
	// type-check: cfgAssessment != cfgReservation. Rewire so classify edges to
	// finalize (and drop the switch to keep the union valid) by loading a bespoke
	// config.
	cfg := `{
      "flow": "bad",
      "entry": "classify",
      "nodes": [
        {"name": "classify", "block": "classify"},
        {"name": "finalize", "block": "finalize"}
      ],
      "wiring": [ {"edge": ["classify", "finalize"]} ]
    }`
	// finalize is used, reserve/decline are not; that would also be flagged, so use a
	// registry with just the two blocks to isolate the type-mismatch error.
	reg := NewRegistry()
	_ = RegisterStep(reg, "classify", cfgClassify)
	_ = RegisterStep(reg, "finalize", cfgFinalize)
	_, err := Load[cfgOrder, cfgReceipt]([]byte(cfg), reg)
	if err == nil {
		t.Fatal("expected edge-type mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "connects") {
		t.Errorf("unexpected error: %s", err.Error())
	}
}

// TestEntryInTypeMismatch asserts entry.inType != In is a load error.
func TestEntryInTypeMismatch(t *testing.T) {
	reg := triageRegistry(t)
	// Load with In = cfgAssessment, but entry classify consumes cfgOrder. Drop the in
	// doc string so this isolates the entry.inType check (the in doc would mismatch
	// first, which a separate assertion covers).
	cfg := strings.Replace(triageConfig, "\"in\": \"plan.cfgOrder\",\n  ", "", 1)
	_, err := Load[cfgAssessment, cfgReceipt]([]byte(cfg), reg)
	if err == nil {
		t.Fatal("expected entry-in mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "entry node") || !strings.Contains(err.Error(), "flow input") {
		t.Errorf("unexpected error: %s", err.Error())
	}
}

// TestTerminalOutTypeMismatch asserts a terminal.outType != Out is a load error.
func TestTerminalOutTypeMismatch(t *testing.T) {
	reg := triageRegistry(t)
	// Load with Out = cfgReservation, but both terminals (finalize, decline) produce
	// cfgReceipt. The in doc string would also mismatch Out, so drop the out doc by
	// using a config without it; entry still consumes cfgOrder so In stays valid.
	cfg := strings.Replace(triageConfig, "\"out\": \"plan.cfgReceipt\",\n  ", "", 1)
	_, err := Load[cfgOrder, cfgReservation]([]byte(cfg), reg)
	if err == nil {
		t.Fatal("expected terminal-out mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "terminal node") || !strings.Contains(err.Error(), "flow output") {
		t.Errorf("unexpected error: %s", err.Error())
	}
}

// TestNodeSwitchedAndEdged asserts a node that is both switched-over and has an
// outgoing edge is rejected.
func TestNodeSwitchedAndEdged(t *testing.T) {
	reg := triageRegistry(t)
	// Add an edge out of classify, which is already switched-over.
	cfg := strings.Replace(triageConfig,
		`{"edge": ["reserve", "finalize"]}`,
		`{"edge": ["reserve", "finalize"]}, {"edge": ["classify", "decline"]}`, 1)
	_, err := Load[cfgOrder, cfgReceipt]([]byte(cfg), reg)
	if err == nil {
		t.Fatal("expected switched-and-edged error, got nil")
	}
	if !strings.Contains(err.Error(), "both switched-over and has an outgoing edge") {
		t.Errorf("unexpected error: %s", err.Error())
	}
}

// TestWiringUnionBothOrNeither asserts a wiring element setting both or neither of
// switch/edge is a load error.
func TestWiringUnionBothOrNeither(t *testing.T) {
	reg := triageRegistry(t)

	// More than one: an element with an edge AND a switch.
	both := strings.Replace(triageConfig,
		`{"edge": ["reserve", "finalize"]}`,
		`{"edge": ["reserve", "finalize"], "switch": "reserve"}`, 1)
	_, err := Load[cfgOrder, cfgReceipt]([]byte(both), reg)
	if err == nil || !strings.Contains(err.Error(), "sets more than one of edge/switch/join") {
		t.Errorf("expected more-than-one error, got: %v", err)
	}

	// Neither: an empty wiring element.
	neither := strings.Replace(triageConfig,
		`{"edge": ["reserve", "finalize"]}`,
		`{}`, 1)
	_, err = Load[cfgOrder, cfgReceipt]([]byte(neither), triageRegistry(t))
	if err == nil || !strings.Contains(err.Error(), "sets none of edge/switch/join") {
		t.Errorf("expected none-set error, got: %v", err)
	}
}

// TestValidateCatchesDrift asserts the non-generic Validate catches resolution
// drift (unknown block) without needing In/Out, and passes on a clean config.
func TestValidateCatchesDrift(t *testing.T) {
	// Clean config validates.
	if err := Validate([]byte(triageConfig), triageRegistry(t)); err != nil {
		t.Errorf("Validate clean config: %v", err)
	}
	// Drifted registry (missing reserve block) fails.
	reg := NewRegistry()
	_ = RegisterStep(reg, "classify", cfgClassify)
	_ = RegisterStep(reg, "finalize", cfgFinalize)
	_ = RegisterStep(reg, "decline", cfgDecline)
	_ = RegisterPredicate(reg, "rush", func(a cfgAssessment) bool { return a.Rush })
	if err := Validate([]byte(triageConfig), reg); err == nil {
		t.Error("Validate expected drift error, got nil")
	}
}

// TestDuplicateRegistration asserts a duplicate block registration is an error at
// register time and surfaces at Load rather than overwriting.
func TestDuplicateRegistration(t *testing.T) {
	reg := NewRegistry()
	if err := RegisterStep(reg, "classify", cfgClassify); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := RegisterStep(reg, "classify", cfgClassify); err == nil {
		t.Error("expected duplicate-registration error, got nil")
	}
	// The duplicate is also collected and surfaces at Load.
	if err := Validate([]byte(triageConfig), reg); err == nil || !strings.Contains(err.Error(), "duplicate block registration") {
		t.Errorf("expected duplicate surfaced at Load, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Join in config (fan-in). The config diamond mirrors join_test.go's buildDiamond
// exactly, so its Digest must equal the hand-built fan-in flow (config == code).
// ---------------------------------------------------------------------------

// diamondRegistry registers the split/y/z steps and the two-input merge for the
// canonical fan-in diamond, matching join_test.go's buildDiamond bodies so a loaded
// diamond and the hand-built one share one Digest and one output.
func diamondRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	must(RegisterStep(reg, "split", func(_ context.Context, n int) (int, error) { return n * 2, nil }))
	must(RegisterStep(reg, "y", func(_ context.Context, n int) (int, error) { return n + 1, nil }))
	must(RegisterStep(reg, "z", func(_ context.Context, n int) (string, error) { return fmt.Sprintf("z%d", n), nil }))
	must(RegisterJoin2(reg, "mergeBlock", func(_ context.Context, a int, s string) (string, error) {
		return fmt.Sprintf("%s+%d", s, a), nil
	}))
	return reg
}

// diamondConfig is the fan-in diamond as data: split fans out to y and z (two
// edges), and a join named "merge" fans them back in via the mergeBlock. The node
// and edge insertion order (split, y, z, then the two fan-out edges, then the join
// which registers "merge" and appends y->merge, z->merge) matches buildDiamond, so
// the two Digests must be equal.
const diamondConfig = `{
  "flow": "diamond",
  "in": "int",
  "out": "string",
  "entry": "split",
  "nodes": [
    {"name": "split", "block": "split"},
    {"name": "y",     "block": "y"},
    {"name": "z",     "block": "z"}
  ],
  "wiring": [
    {"edge": ["split", "y"]},
    {"edge": ["split", "z"]},
    {"join": "merge", "inputs": ["y", "z"], "merge": "mergeBlock"}
  ]
}`

// TestLoadJoinConfigRunsConformsAndMatchesHandBuilt loads the diamond config, runs it
// to the merged output, conforms, and asserts its Digest EQUALS join_test.go's
// hand-built buildDiamond of the same fan-in topology (config == code for a diamond).
func TestLoadJoinConfigRunsConformsAndMatchesHandBuilt(t *testing.T) {
	flow, err := Load[int, string]([]byte(diamondConfig), diamondRegistry(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Digest equals the hand-built diamond of the same shape.
	hand, err := buildDiamond(BlockName("mergeBlock"))
	if err != nil {
		t.Fatalf("hand build: %v", err)
	}
	if flow.Digest() != hand.Digest() {
		t.Errorf("config digest %s != hand-built diamond digest %s", flow.Digest(), hand.Digest())
	}

	// Runs to the merged output: split(3)=6; y=7; z="z6"; merge="z6+7".
	ctx := context.Background()
	store := agent.NewMemStore()
	out, err := flow.Run(ctx, store, "diamond-cfg", 3)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "z6+7" {
		t.Errorf("merged output = %q, want %q", out, "z6+7")
	}
	ok, diffs, err := flow.Conform(ctx, store, "diamond-cfg")
	if err != nil {
		t.Fatalf("Conform: %v", err)
	}
	if !ok {
		t.Errorf("loaded diamond run did not conform: %v", diffs)
	}
}

// TestLoadJoinUnknownMergeIsCollected asserts an unknown merge block is a load error
// naming the offending join and merge, collected with the rest of the drift.
func TestLoadJoinUnknownMergeIsCollected(t *testing.T) {
	reg := diamondRegistry(t)
	cfg := strings.Replace(diamondConfig, `"merge": "mergeBlock"`, `"merge": "ghostMerge"`, 1)
	_, err := Load[int, string]([]byte(cfg), reg)
	if err == nil {
		t.Fatal("expected unknown-merge error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown merge") || !strings.Contains(err.Error(), "ghostMerge") {
		t.Errorf("error does not name the unknown merge: %s", err.Error())
	}
}

// TestLoadJoinArityMismatchIsError asserts a join whose declared input count differs
// from the merge block's arity is a load error naming the join and both counts.
func TestLoadJoinArityMismatchIsError(t *testing.T) {
	reg := diamondRegistry(t)
	// The mergeBlock has arity 2; give the join three inputs.
	cfg := strings.Replace(diamondConfig, `"inputs": ["y", "z"]`, `"inputs": ["y", "z", "y"]`, 1)
	_, err := Load[int, string]([]byte(cfg), reg)
	if err == nil {
		t.Fatal("expected join-arity mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "merge") || !strings.Contains(err.Error(), "expects 2") {
		t.Errorf("error does not name the arity mismatch: %s", err.Error())
	}
}

// ---------------------------------------------------------------------------
// Loop (LoopBack) in config: a switch When arm with a positive loopMax is a bounded
// back-edge. The config countdown loop mirrors loop_test.go's buildCountdownLoop, so
// it iterates then exits, conforms, and its Digest equals the hand-built loop.
// ---------------------------------------------------------------------------

// loopRegistry registers the seed/refine/check/done steps and the loop predicate for
// the countdown loop, matching loop_test.go's buildCountdownLoop bodies.
func loopRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	must(RegisterStep(reg, "seed", func(_ context.Context, n int) (loopState, error) {
		return loopState{N: n, Trace: "seed"}, nil
	}))
	must(RegisterStep(reg, "refine", func(_ context.Context, s loopState) (loopState, error) {
		return loopState{N: s.N - 1, Trace: s.Trace + "|refine"}, nil
	}))
	must(RegisterStep(reg, "check", func(_ context.Context, s loopState) (loopState, error) { return s, nil }))
	must(RegisterStep(reg, "done", func(_ context.Context, s loopState) (string, error) {
		return fmt.Sprintf("done N=%d trace=%s", s.N, s.Trace), nil
	}))
	must(RegisterPredicate(reg, "again", func(s loopState) bool { return s.N > 0 }))
	return reg
}

// loopConfig is the bounded countdown loop as data: seed -> refine (head) -> check
// (switch); the switch has a When arm with loopMax 10 routing BACK to refine while
// N>0, and an Else exit to done. It mirrors buildCountdownLoop(10) exactly (same node
// order, same edges, same arm order and bound), so the two Digests must be equal.
const loopConfig = `{
  "flow": "countdown",
  "in": "int",
  "out": "string",
  "entry": "seed",
  "nodes": [
    {"name": "seed",   "block": "seed"},
    {"name": "refine", "block": "refine"},
    {"name": "check",  "block": "check"},
    {"name": "done",   "block": "done"}
  ],
  "wiring": [
    {"edge": ["seed", "refine"]},
    {"edge": ["refine", "check"]},
    {"switch": "check", "when": [{"pred": "again", "to": "refine", "loopMax": 10}], "else": "done"}
  ]
}`

// TestLoadLoopConfigIteratesExitsConformsAndMatchesHandBuilt loads the loop config,
// asserts its Digest equals the hand-built buildCountdownLoop(10), runs it (input 3
// iterates refine three times then exits at N=0), and conforms.
func TestLoadLoopConfigIteratesExitsConformsAndMatchesHandBuilt(t *testing.T) {
	flow, err := Load[int, string]([]byte(loopConfig), loopRegistry(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	hand, err := buildCountdownLoop(10, nil)
	if err != nil {
		t.Fatalf("hand build: %v", err)
	}
	if flow.Digest() != hand.Digest() {
		t.Errorf("config digest %s != hand-built loop digest %s", flow.Digest(), hand.Digest())
	}

	ctx := context.Background()
	store := agent.NewMemStore()
	out, err := flow.Run(ctx, store, "loop-cfg", 3)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.HasPrefix(out, "done N=0 ") {
		t.Errorf("terminal output = %q, want it to report N=0", out)
	}
	if want := "seed|refine|refine|refine"; !strings.Contains(out, want) {
		t.Errorf("terminal trace does not show three body passes: %q (want %q)", out, want)
	}
	ok, diffs, err := flow.Conform(ctx, store, "loop-cfg")
	if err != nil {
		t.Fatalf("Conform: %v", err)
	}
	if !ok {
		t.Errorf("loaded loop run did not conform: %v", diffs)
	}
}

// TestLoadLoopUnknownPredicateIsCollected asserts an unknown loop-back predicate is a
// load error naming the offending arm and predicate, collected with the rest of the
// drift (a loop-back arm's predicate is an ordinary registered predicate).
func TestLoadLoopUnknownPredicateIsCollected(t *testing.T) {
	reg := loopRegistry(t)
	cfg := strings.Replace(loopConfig, `"pred": "again"`, `"pred": "ghostPred"`, 1)
	_, err := Load[int, string]([]byte(cfg), reg)
	if err == nil {
		t.Fatal("expected unknown-predicate error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown predicate") || !strings.Contains(err.Error(), "ghostPred") {
		t.Errorf("error does not name the unknown loop predicate: %s", err.Error())
	}
}

// TestLoadLoopNonAncestorHeadIsBuildError asserts the existing loop validation runs on
// a loaded loop: a loopMax back-edge whose target is not an ancestor of the switch is
// rejected. Here the arm loops back to "done", which the switch does not reach forward.
func TestLoadLoopNonAncestorHeadIsBuildError(t *testing.T) {
	reg := loopRegistry(t)
	// Route the loop-back to "done" (not an ancestor of check) and exit via a second
	// When arm so the switch still has a non-loop-back exit; done consumes loopState so
	// the arm types unify.
	cfg := strings.Replace(loopConfig,
		`{"switch": "check", "when": [{"pred": "again", "to": "refine", "loopMax": 10}], "else": "done"}`,
		`{"switch": "check", "when": [{"pred": "again", "to": "done", "loopMax": 10}], "else": "done"}`, 1)
	_, err := Load[int, string]([]byte(cfg), reg)
	if err == nil {
		t.Fatal("expected non-ancestor loop error, got nil")
	}
	if !strings.Contains(err.Error(), "ancestor") {
		t.Errorf("error does not mention the ancestor requirement: %s", err.Error())
	}
}

// ---------------------------------------------------------------------------
// Safety in config: an explicit "safety" on a node overrides the registered block's
// default, so a node that would halt on the ambiguous-crash window re-runs instead.
// ---------------------------------------------------------------------------

// safetyNodeConfig is a one-node flow whose entry "read" carries safety "readonly".
// The read block itself registers with no Safety (the default halt), so the config
// "safety" is what opts it into re-run on the ambiguous crash.
const safetyNodeConfig = `{
  "flow": "read-flow",
  "in": "int",
  "out": "int",
  "entry": "read",
  "nodes": [
    {"name": "read", "block": "read", "safety": "readonly"}
  ],
  "wiring": []
}`

// loadReadFlow loads the one-node read flow, registering the read block with the given
// reads counter and value and NO Go-side Safety, so the config "safety" is the only
// source of the node's retry-on-resume classification.
func loadReadFlow(t *testing.T, reads *int, value int, safety string) (*Flow[int, int], error) {
	t.Helper()
	reg := NewRegistry()
	if err := RegisterStep(reg, "read", func(context.Context, int) (int, error) { *reads++; return value, nil }); err != nil {
		t.Fatalf("register: %v", err)
	}
	cfg := safetyNodeConfig
	if safety != "readonly" {
		cfg = strings.Replace(cfg, `, "safety": "readonly"`, safetySuffix(safety), 1)
	}
	return Load[int, int]([]byte(cfg), reg)
}

// safetySuffix renders the JSON fragment for a given safety value, or drops the field
// entirely for the empty string (the default-halt case).
func safetySuffix(safety string) string {
	if safety == "" {
		return ""
	}
	return fmt.Sprintf(`, "safety": %q`, safety)
}

// TestLoadSafetyConfigRerunsOnAmbiguousCrash proves an explicit config "safety":
// "readonly" makes a loaded node RE-RUN its body on the ambiguous-crash window (attempt
// marker persisted, result lost) and COMPLETE, where the SAME node without the config
// safety would halt. It reuses the crashFlowStore DST harness from flow_dst_test.go.
func TestLoadSafetyConfigRerunsOnAmbiguousCrash(t *testing.T) {
	// Find the crash landing on the read node's result write, with the readonly config.
	var mem agent.Durable
	var readsAtCrash int
	for crashAt := 1; crashAt <= 32; crashAt++ {
		reads := 0
		m := agent.NewMemStore()
		store := &crashFlowStore{inner: m, crashAt: crashAt}
		flow, err := loadReadFlow(t, &reads, 42, "readonly")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		_, err = flow.Run(context.Background(), store, "cfg-safety", 0)
		if !errors.Is(err, errCrash) {
			continue
		}
		recs, hErr := m.History(context.Background(), "cfg-safety")
		if hErr != nil {
			t.Fatalf("History: %v", hErr)
		}
		var haveAttempt, haveResult bool
		for _, r := range recs {
			switch r.Name {
			case "attempt:read":
				haveAttempt = true
			case "read":
				haveResult = true
			}
		}
		if haveAttempt && !haveResult && reads >= 1 {
			mem = m
			readsAtCrash = reads
			break
		}
	}
	if mem == nil {
		t.Fatal("no crash point produced the entry-result ambiguous window")
	}

	// Resume with no further crash. The config safety "readonly" opts the node into
	// re-run, so the flow completes rather than returning *HaltAmbiguous.
	reads := readsAtCrash
	flow, err := loadReadFlow(t, &reads, 42, "readonly")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	out, err := flow.Run(context.Background(), mem, "cfg-safety", 0)
	var halt *HaltAmbiguous
	if errors.As(err, &halt) {
		t.Fatalf("config-safety readonly node halted at %q; want re-run and completion", halt.Step)
	}
	if err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	if out != 42 {
		t.Fatalf("output = %d, want 42", out)
	}
	if reads < 2 {
		t.Fatalf("body ran %d times total, want >= 2 (a re-run happened on resume)", reads)
	}
}

// TestLoadSafetyDefaultHalts is the regression guard: the SAME one-node flow loaded
// with NO config safety keeps the conservative default and HALTS on the ambiguous
// crash, proving the config safety is what changed the behavior above.
func TestLoadSafetyDefaultHalts(t *testing.T) {
	var mem agent.Durable
	var readsAtCrash int
	for crashAt := 1; crashAt <= 32; crashAt++ {
		reads := 0
		m := agent.NewMemStore()
		store := &crashFlowStore{inner: m, crashAt: crashAt}
		flow, err := loadReadFlow(t, &reads, 42, "")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		_, err = flow.Run(context.Background(), store, "cfg-default", 0)
		if !errors.Is(err, errCrash) {
			continue
		}
		recs, _ := m.History(context.Background(), "cfg-default")
		var haveAttempt, haveResult bool
		for _, r := range recs {
			switch r.Name {
			case "attempt:read":
				haveAttempt = true
			case "read":
				haveResult = true
			}
		}
		if haveAttempt && !haveResult && reads >= 1 {
			mem = m
			readsAtCrash = reads
			break
		}
	}
	if mem == nil {
		t.Fatal("no crash point produced the entry-result ambiguous window")
	}

	reads := readsAtCrash
	flow, err := loadReadFlow(t, &reads, 42, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err = flow.Run(context.Background(), mem, "cfg-default", 0)
	var halt *HaltAmbiguous
	if !errors.As(err, &halt) {
		t.Fatalf("default node did not halt: err = %v; want *HaltAmbiguous", err)
	}
	if halt.Step != "read" {
		t.Fatalf("halt named %q, want %q", halt.Step, "read")
	}
	if reads != readsAtCrash {
		t.Fatalf("default node re-ran its body on resume (reads %d -> %d); it must halt", readsAtCrash, reads)
	}
}

// TestLoadUnknownSafetyStringIsError asserts an unknown safety string on a node is a
// load error naming the node and the bad value.
func TestLoadUnknownSafetyStringIsError(t *testing.T) {
	reg := NewRegistry()
	if err := RegisterStep(reg, "read", func(_ context.Context, n int) (int, error) { return n, nil }); err != nil {
		t.Fatalf("register: %v", err)
	}
	cfg := strings.Replace(safetyNodeConfig, `"safety": "readonly"`, `"safety": "sometimes"`, 1)
	_, err := Load[int, int]([]byte(cfg), reg)
	if err == nil {
		t.Fatal("expected unknown-safety error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown safety") || !strings.Contains(err.Error(), "read") || !strings.Contains(err.Error(), "sometimes") {
		t.Errorf("error does not name the node and bad safety value: %s", err.Error())
	}
}

// TestLoadWiringUnionRejectsMoreThanOneIncludingJoin asserts the three-way union check
// rejects a wiring element that sets more than one of edge/switch/join (here an edge
// AND a join), naming its index.
func TestLoadWiringUnionRejectsMoreThanOneIncludingJoin(t *testing.T) {
	reg := diamondRegistry(t)
	// Turn the second edge into an element that ALSO carries a join.
	cfg := strings.Replace(diamondConfig,
		`{"edge": ["split", "z"]}`,
		`{"edge": ["split", "z"], "join": "merge2", "inputs": ["y", "z"], "merge": "mergeBlock"}`, 1)
	_, err := Load[int, string]([]byte(cfg), reg)
	if err == nil {
		t.Fatal("expected more-than-one union error, got nil")
	}
	if !strings.Contains(err.Error(), "sets more than one of edge/switch/join") {
		t.Errorf("error does not report the union violation: %s", err.Error())
	}
}
