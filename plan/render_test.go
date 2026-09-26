package plan

import (
	"strings"
	"testing"
)

// fixtureCore builds a small reified spec directly (no exported constructors,
// which Agent B owns) so renderMermaid can be exercised in isolation. The graph
// is: parse -> route, where route is a Switch that branches to summarize (When)
// and reject (Else). This gives a DAG shape with one straight edge and two arm
// edges, plus a diamond node for the switch.
func fixtureCore() *builderCore {
	nParse := &node{name: "parse", kind: kindStep, inType: typeOf[string](), outType: typeOf[int]()}
	nRoute := &node{name: "route", kind: kindSwitch, inType: typeOf[int](), outType: typeOf[int]()}
	nSum := &node{name: "summarize", kind: kindModel, inType: typeOf[int](), outType: typeOf[string]()}
	nRej := &node{name: "reject", kind: kindTool, inType: typeOf[int](), outType: typeOf[bool]()}

	c := &builderCore{
		flowName: "triage",
		entry:    "parse",
		inType:   typeOf[string](),
		outType:  typeOf[string](),
		nodes:    []*node{nParse, nRoute, nSum, nRej},
		byName: map[string]*node{
			"parse": nParse, "route": nRoute, "summarize": nSum, "reject": nRej,
		},
		edges: []edge{{from: "parse", to: "route"}},
		branches: []branch{{
			over: "route",
			arms: []arm{
				{isElse: false, pred: func(v any) bool { return true }, target: "summarize"},
				{isElse: true, target: "reject"},
			},
		}},
	}
	return c
}

func TestRenderMermaid_ContainsHeaderAndStart(t *testing.T) {
	got := fixtureCore().renderMermaid()
	if !strings.HasPrefix(got, "flowchart TD\n") {
		t.Fatalf("output must start with the Mermaid header, got:\n%s", got)
	}
	if !strings.Contains(got, "start([user])") {
		t.Errorf("output must contain the start node, got:\n%s", got)
	}
	// Entry is wired from start to the first node's id (n0).
	if !strings.Contains(got, "start --> n0") {
		t.Errorf("output must connect start to the entry node n0, got:\n%s", got)
	}
}

func TestRenderMermaid_NodeLabels(t *testing.T) {
	got := fixtureCore().renderMermaid()

	// Each node is labelled "name : InType -> OutType" with reflect type names.
	wantBoxes := []string{
		`n0["parse : string -> int"]`,
		`n2["summarize : int -> string"]`,
		`n3["reject : int -> bool"]`,
	}
	for _, w := range wantBoxes {
		if !strings.Contains(got, w) {
			t.Errorf("missing box node label %q in:\n%s", w, got)
		}
	}
	// The Switch node renders as a diamond, not a box.
	wantDiamond := `n1{"route : int -> int"}`
	if !strings.Contains(got, wantDiamond) {
		t.Errorf("switch node must render as a diamond %q in:\n%s", wantDiamond, got)
	}
	if strings.Contains(got, `n1["route`) {
		t.Errorf("switch node must not render as a box in:\n%s", got)
	}
}

func TestRenderMermaid_Edges(t *testing.T) {
	got := fixtureCore().renderMermaid()

	// Straight edge parse (n0) -> route (n1).
	if !strings.Contains(got, "n0 --> n1") {
		t.Errorf("missing straight edge n0 --> n1 in:\n%s", got)
	}
	// Branch arm edges from the switched node (n1), labelled by arm.
	if !strings.Contains(got, "n1 -->|When| n2") {
		t.Errorf("missing When arm edge n1 -->|When| n2 in:\n%s", got)
	}
	if !strings.Contains(got, "n1 -->|Else| n3") {
		t.Errorf("missing Else arm edge n1 -->|Else| n3 in:\n%s", got)
	}
}

func TestRenderMermaid_Deterministic(t *testing.T) {
	c := fixtureCore()
	first := c.renderMermaid()
	second := c.renderMermaid()
	if first != second {
		t.Errorf("output must be stable across calls:\nfirst:\n%s\nsecond:\n%s", first, second)
	}

	// Insertion order must be respected: parse's edge appears before the branch
	// arm edges, and the When arm appears before the Else arm.
	iEdge := strings.Index(first, "n0 --> n1")
	iWhen := strings.Index(first, "n1 -->|When| n2")
	iElse := strings.Index(first, "n1 -->|Else| n3")
	if iEdge < 0 || iWhen < 0 || iElse < 0 {
		t.Fatalf("expected all edges present, got:\n%s", first)
	}
	if !(iEdge < iWhen && iWhen < iElse) {
		t.Errorf("edges must follow insertion order (edge < When < Else), got positions %d, %d, %d in:\n%s", iEdge, iWhen, iElse, first)
	}
}

func TestRenderMermaid_NilTypeRendersQuestionMark(t *testing.T) {
	// A node with an unpinned side renders "?" rather than panicking.
	c := &builderCore{
		flowName: "partial",
		entry:    "only",
		nodes:    []*node{{name: "only", kind: kindStep, inType: typeOf[string](), outType: nil}},
		byName:   map[string]*node{"only": {name: "only", kind: kindStep}},
	}
	got := c.renderMermaid()
	if !strings.Contains(got, `n0["only : string -> ?"]`) {
		t.Errorf("nil out type must render as ?, got:\n%s", got)
	}
}
