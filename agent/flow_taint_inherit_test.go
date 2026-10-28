package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/bide-ai/bide/internal/journalhook"
)

// A flow started inside a run under taint (docs/design/flow-taint.md, "Nested flows and inherited taint"): it
// mirrors an agent child. With its own policy it records its declarations and the marks it inherited; with none
// it records the parent's status alone and may run only nodes that cannot have a side effect.

const parentDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func taintedCtx(marks ...taintMark) context.Context {
	return withCallMarks(withUnderTaint(context.Background(), parentDigest), marks)
}

func flowStart() RunStart {
	return RunStart{Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}, Input: UserText("1")}
}

func TestFlowTaintPrepare_Inherits(t *testing.T) {
	ctx := taintedCtx(taintMark{Source: "inbound-email", Ref: "node:fetch"}, taintMark{Source: "crm", Ref: "node:look"})
	j := memJournal()
	// A flow with its own policy: its own declarations, strictly paired, and the inherited marks, canonical.
	got, err := flowTaintPrepare(ctx, j, "r1", flowStart(), hookDecls(nil), []string{"pay"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := got.(RunStart)
	if _, ok, err := s.flowTaintDecls(); err != nil || !ok || s.inheritedOnly() {
		t.Fatalf("own declarations: ok %v, err %v, inheritedOnly %v", ok, err, s.inheritedOnly())
	}
	l, err := checkInheritedMarks(s)
	want := []inheritedMark{{Source: "crm", Ref: "parent:node:look"}, {Source: "inbound-email", Ref: "parent:node:fetch"}}
	if err != nil || !slices.Equal(l, want) {
		t.Errorf("inherited marks %v, %v; want %v", l, err, want)
	}
	if s.taintDigest() == parentDigest {
		t.Error("a flow with its own policy recorded the parent's digest as its own")
	}
	// A flow with no policy: the parent's status and the marks, and no declarations.
	got, err = flowTaintPrepare(ctx, j, "r2", flowStart(), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	s = got.(RunStart)
	if !s.inheritedOnly() || s.taintDigest() != parentDigest {
		t.Errorf("policyless: inheritedOnly %v, digest %q", s.inheritedOnly(), s.taintDigest())
	}
	if l, err := checkInheritedMarks(s); err != nil || !slices.Equal(l, want) {
		t.Errorf("policyless marks %v, %v", l, err)
	}
	// Outside a tainted run nothing is recorded for a policyless flow.
	got, err = flowTaintPrepare(context.Background(), j, "r3", flowStart(), nil, []string{"pay"}, nil)
	if err != nil || got.(RunStart).taintDigest() != "" || len(got.(RunStart).Ext) != 0 {
		t.Errorf("not under taint: %v, %+v", err, got.(RunStart).Ext)
	}
}

func TestFlowTaintPrepare_PolicylessWithASideEffectIsRefusedAtBegin(t *testing.T) {
	ctx := taintedCtx(taintMark{Source: "s", Ref: "node:x"})
	j := memJournal()
	_, err := flowTaintPrepare(ctx, j, "r1", flowStart(), nil, []string{"fetch", "pay"}, nil)
	if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "fetch, pay") {
		t.Fatalf("a policyless flow with side-effecting steps: %v, want ErrConfig naming them", err)
	}
	if recs, _ := j.History(context.Background(), "r1"); len(recs) != 0 {
		t.Errorf("the refused run wrote %d records", len(recs))
	}
	// A flow of read-only nodes runs.
	if _, err := flowTaintPrepare(ctx, j, "r2", flowStart(), nil, nil, nil); err != nil {
		t.Errorf("a policyless flow of read-only nodes: %v", err)
	}
	// A run that has started is not refused here: it keeps its recorded start, and Enter judges the drive.
	got, _ := flowTaintPrepare(ctx, j, "r3", flowStart(), nil, nil, nil)
	if _, _, _, err := beginRun(ctx, j, "r3", got.(RunStart)); err != nil {
		t.Fatal(err)
	}
	if _, err := flowTaintPrepare(ctx, j, "r3", flowStart(), nil, []string{"pay"}, nil); err != nil {
		t.Errorf("a resume of a started run: %v", err)
	}
}

func TestFlowTaintPrepare_RequireProvenanceIsNotAskedOfANestedFlow(t *testing.T) {
	req := hookDecls(func(d *journalhook.FlowTaint) { d.RequireProvenance = true })
	if _, err := flowTaintPrepare(taintedCtx(), memJournal(), "r1", flowStart(), req, nil, nil); err != nil {
		t.Errorf("a flow started inside a tainted run: %v", err)
	}
	if _, err := flowTaintPrepare(context.Background(), memJournal(), "r1", flowStart(), req, nil, nil); !errors.Is(err, ErrConfig) {
		t.Errorf("a top-level new run stating nothing: %v, want ErrConfig", err)
	}
}

// begin starts a flow run in a fresh journal under ctx, as package plan does.
func beginNested(t *testing.T, ctx context.Context, d *journalhook.FlowTaint, tracked []string) (*Journal, context.Context) {
	t.Helper()
	j := memJournal()
	got, err := flowTaintPrepare(ctx, j, "r1", flowStart(), d, tracked, nil)
	if err != nil {
		t.Fatal(err)
	}
	bctx, _, _, err := beginRun(ctx, j, "r1", got.(RunStart))
	if err != nil {
		t.Fatal(err)
	}
	return j, bctx
}

func TestFlowTaintEnter_HoldsTheInheritedMarks(t *testing.T) {
	m1 := taintMark{Source: "inbound-email", Ref: "node:fetch"}
	j, _ := beginNested(t, taintedCtx(m1), hookDecls(nil), nil)
	enter := func(ctx context.Context, d *journalhook.FlowTaint) (int, error) {
		return flowTaintEnter(ctx, j, "r1", d, nil, nil)
	}
	if st, err := enter(taintedCtx(m1), hookDecls(nil)); err != nil || st != journalhook.FlowTaintUnder {
		t.Errorf("the same marks: %d, %v", st, err)
	}
	if st, err := enter(context.Background(), hookDecls(nil)); err != nil || st != journalhook.FlowTaintUnder {
		t.Errorf("a drive that carries none (a recovery pass) runs under the recorded ones: %d, %v", st, err)
	}
	if _, err := enter(taintedCtx(taintMark{Source: "web", Ref: "node:x"}), hookDecls(nil)); !errors.Is(err, ErrConfig) {
		t.Errorf("other marks: %v, want ErrConfig", err)
	}
	if _, err := enter(taintedCtx(m1, taintMark{Source: "web", Ref: "node:x"}), hookDecls(nil)); !errors.Is(err, ErrConfig) {
		t.Errorf("more marks: %v, want ErrConfig", err)
	}
	// A recorded list that is not canonical is a storage fault, not marks to read a set from.
	bad := RunStart{Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}, Input: UserText("1")}.withFlowTaint(flowTaintDecls{Policy: flowTaintPolicy{Refuse: true}})
	bad.Ext[extTaintInherited] = json.RawMessage(`[{"source":"b","ref":"parent:x"},{"source":"a","ref":"parent:x"}]`)
	jb := memJournal()
	if _, _, _, err := beginRun(context.Background(), jb, "r1", bad); err != nil {
		t.Fatal(err)
	}
	if _, err := flowTaintEnter(context.Background(), jb, "r1", &journalhook.FlowTaint{Refuse: true}, nil, nil); !errors.Is(err, ErrStorage) {
		t.Errorf("a list not sorted: %v, want ErrStorage", err)
	}
	for what, raw := range map[string]string{
		"a repeated source":           `[{"source":"a","ref":"parent:x"},{"source":"a","ref":"parent:y"}]`,
		"a ref with no parent prefix": `[{"source":"a","ref":"node:x"}]`,
		"an empty source":             `[{"source":"","ref":"parent:x"}]`,
		"an unknown member":           `[{"source":"a","ref":"parent:x","extra":1}]`,
	} {
		s := bad
		s.Ext = map[string]json.RawMessage{extTaintInherited: json.RawMessage(raw)}
		if _, err := checkInheritedMarks(s); !errors.Is(err, ErrStorage) {
			t.Errorf("%s: %v, want ErrStorage", what, err)
		}
	}
}

func TestFlowTaintEnter_InheritedPolicylessStanding(t *testing.T) {
	m1 := taintMark{Source: "s", Ref: "node:x"}
	j, _ := beginNested(t, taintedCtx(m1), nil, nil)
	if st, err := flowTaintEnter(context.Background(), j, "r1", nil, nil, nil); err != nil || st != journalhook.FlowTaintPolicyless {
		t.Errorf("the flow that started it: %d, %v; want the policyless standing", st, err)
	}
	// A flow that has a policy now is not the one that started it.
	if _, err := flowTaintEnter(context.Background(), j, "r1", hookDecls(nil), nil, nil); !errors.Is(err, ErrConfig) {
		t.Errorf("a flow with a policy now: %v, want ErrConfig", err)
	}
}

func TestFlowTaintNode_InheritedMarksGateAndAreBound(t *testing.T) {
	ctx := context.Background()
	m1 := taintMark{Source: "inbound-email", Ref: "node:fetch"}
	// An own-policy flow whose journal holds no source of its own: the inherited mark alone gates a side effect.
	j, _ := beginNested(t, taintedCtx(m1), hookDecls(func(d *journalhook.FlowTaint) { d.Nodes = nil }), nil)
	d, err := flowTaintNode(ctx, j, "r1", "node:pay", Safety{}, journalhook.FlowTaintUnder)
	if err != nil || d.Gate == nil {
		t.Fatalf("decision %+v, %v; want the gate", d, err)
	}
	rec, _ := nodeReasons(t, j, "node:pay")
	if len(rec.Reasons) != 1 || rec.Reasons[0].Source != "inbound-email" || rec.Reasons[0].Ref != "parent:node:fetch" {
		t.Errorf("reasons %+v, want the inherited mark with its parent ref", rec.Reasons)
	}
	// The body's context carries the run's status and the read set, canonical: the inherited mark keeps its
	// ref, and a source of the run's own is added once, never twice, whatever it is read through.
	d2 := hookDecls(nil)
	jj, _ := beginNested(t, taintedCtx(m1, taintMark{Source: "crm", Ref: "node:look"}), d2, nil)
	put(t, jj, "node:fetch") // declares inbound-email again, in this run
	dec, err := flowTaintNode(ctx, jj, "r1", "node:pay", Safety{}, journalhook.FlowTaintUnder)
	if err != nil || dec.Ctx == nil {
		t.Fatalf("decision %+v, %v", dec, err)
	}
	c, ok := callMarksFrom(dec.Ctx)
	wantList := []inheritedMark{{Source: "crm", Ref: "parent:node:look"}, {Source: "inbound-email", Ref: "parent:node:fetch"}}
	if !ok || !c.strict || !slices.Equal(c.list, wantList) {
		t.Errorf("bound marks %+v, %v; want %v (one per source, the first ref kept, sorted)", c, ok, wantList)
	}
	if underTaintFrom(dec.Ctx) == "" {
		t.Error("the status is not bound")
	}
	// A policyless inherited run: read-only nodes get the bound context and no gate, side effects are refused.
	jp, _ := beginNested(t, taintedCtx(m1), nil, nil)
	dr, err := flowTaintNode(ctx, jp, "r1", "node:look", Safety{ReadOnly: true}, journalhook.FlowTaintPolicyless)
	if err != nil || dr.Gate != nil || underTaintFrom(dr.Ctx) != parentDigest {
		t.Errorf("a read-only node of a policyless inherited run: %+v, %v", dr, err)
	}
	if c, ok := callMarksFrom(dr.Ctx); !ok || len(c.list) != 1 || c.list[0].Ref != "parent:node:fetch" {
		t.Errorf("its bound marks: %+v, %v", c, ok)
	}
	if _, err := flowTaintNode(ctx, jp, "r1", "node:pay", Safety{}, journalhook.FlowTaintPolicyless); !errors.Is(err, ErrConfig) {
		t.Errorf("a side effect of a policyless inherited run: %v, want ErrConfig", err)
	}
}

// The list passed down through a chain does not grow: it is the canonical form of the read set, whose inherited
// marks keep their first ref, and canonicalInherited is idempotent over it.
func TestCanonicalInherited_ChainDoesNotGrow(t *testing.T) {
	level1 := canonicalInherited([]taintMark{{Source: "s", Ref: "node:fetch"}})
	level2 := canonicalInherited(append(inheritedAsMarks(level1), taintMark{Source: "s", Ref: "node:other"}, taintMark{Source: "t", Ref: "node:look"}))
	level3 := canonicalInherited(append(inheritedAsMarks(level2), taintMark{Source: "s", Ref: "node:third"}))
	want := []inheritedMark{{Source: "s", Ref: "parent:node:fetch"}, {Source: "t", Ref: "parent:node:look"}}
	if !slices.Equal(level2, want) || !slices.Equal(level3, want) {
		t.Errorf("level 2 %v, level 3 %v; want %v", level2, level3, want)
	}
}
