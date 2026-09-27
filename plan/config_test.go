package plan

import (
	"context"
	"strings"
	"testing"

	agent "github.com/dayna/go-agents"
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

func cfgClassify(o cfgOrder) (cfgAssessment, error) {
	return cfgAssessment{ID: o.ID, Rush: o.Rush}, nil
}
func cfgReserve(a cfgAssessment) (cfgReservation, error) {
	return cfgReservation{ID: a.ID}, nil
}
func cfgFinalize(r cfgReservation) (cfgReceipt, error) {
	return cfgReceipt{ID: r.ID, Status: "reserved"}, nil
}
func cfgDecline(a cfgAssessment) (cfgReceipt, error) {
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
		When(func(a cfgAssessment) bool { return a.Rush }, reserve),
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
	reg := triageRegistry(t)
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
	reg = NewRegistry()
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

	// Both: an element with an edge AND a switch.
	both := strings.Replace(triageConfig,
		`{"edge": ["reserve", "finalize"]}`,
		`{"edge": ["reserve", "finalize"], "switch": "reserve"}`, 1)
	_, err := Load[cfgOrder, cfgReceipt]([]byte(both), reg)
	if err == nil || !strings.Contains(err.Error(), "sets both edge and switch") {
		t.Errorf("expected both-set error, got: %v", err)
	}

	// Neither: an empty wiring element.
	neither := strings.Replace(triageConfig,
		`{"edge": ["reserve", "finalize"]}`,
		`{}`, 1)
	_, err = Load[cfgOrder, cfgReceipt]([]byte(neither), triageRegistry(t))
	if err == nil || !strings.Contains(err.Error(), "sets neither edge nor switch") {
		t.Errorf("expected neither-set error, got: %v", err)
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
