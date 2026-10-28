package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/internal/journalhook"
)

// TaintPolicy.SeparateApprovers (docs/design/flow-taint.md, "Disjoint approvers"): on a call's escalation instance
// the approvers whose decisions the call's own gate counted do not count, and the policy is checked at New.

func sepGate(need int, ids ...string) *ApprovalPolicy {
	return &ApprovalPolicy{Need: need, Approvers: ids}
}

func TestTaintPolicy_SeparateApproversNeedsAnMofNGate(t *testing.T) {
	for what, p := range map[string]TaintPolicy{
		"Refuse":         {Refuse: true, SeparateApprovers: true},
		"a single gate":  {Gate: SingleApproval(), SeparateApprovers: true},
		"neither":        {SeparateApprovers: true},
		"both (invalid)": {Gate: sepGate(1, "a"), Refuse: true, SeparateApprovers: true},
	} {
		if err := p.check(); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: %v, want ErrConfig", what, err)
		}
	}
	if err := (TaintPolicy{Gate: sepGate(2, "a", "b", "c"), SeparateApprovers: true}).check(); err != nil {
		t.Errorf("an m-of-n gate: %v", err)
	}
}

// The field is in the declarations digest only when set: a policy without it digests as it did.
func TestTaintPolicy_SeparateApproversIsInTheDigestOnlyWhenSet(t *testing.T) {
	mk := func(sep bool) string {
		a := &Agent{taint: &TaintPolicy{Gate: sepGate(2, "a", "b", "c"), SeparateApprovers: sep}, specs: map[string]*ToolSpec{}}
		return a.taintDigest()
	}
	if mk(true) == mk(false) {
		t.Error("separating approvers does not change the digest")
	}
	d := taintDeclarations{Format: taintDeclarationsFormat, Gate: sepGate(2, "a", "b", "c")}
	b, _ := json.Marshal(d)
	if strings.Contains(string(b), "separate") {
		t.Errorf("an unset field is in the declarations: %s", b)
	}
}

// sepTool is a tool with the own gate own (none for nil), and the options opts.
func sepTool(name string, own *ApprovalPolicy, opts ...ToolOption) Tool {
	if own != nil {
		opts = append(opts, WithApproval(own))
	}
	return MustFunc(name, "", func(context.Context, struct{}) (string, error) { return "ok", nil }, opts...)
}

func TestNew_SeparateApproversChecksEachEscalatedToolsOwnGate(t *testing.T) {
	esc := sepGate(2, "sec1", "sec2", "sec3")
	build := func(tool Tool, pol TaintPolicy) error {
		_, err := New(stubModel{}, memJournal(), WithTools(tool), WithTaintPolicy(pol), WithApproverVerifiers(fakeVerifiers("ops", "fin", "sec1", "sec2", "sec3", "SEC1")))
		return err
	}
	pol := TaintPolicy{Gate: esc, SeparateApprovers: true}
	if err := build(sepTool("pay", sepGate(2, "ops", "fin")), pol); err != nil {
		t.Errorf("disjoint gates: %v", err)
	}
	err := build(sepTool("pay", sepGate(2, "ops", "sec2")), pol)
	if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), `"pay"`) || !strings.Contains(err.Error(), "sec2") {
		t.Errorf("a shared approver: %v, want ErrConfig naming the tool and sec2", err)
	}
	if err := build(sepTool("pay", sepGate(2, "ops", "SEC1")), pol); !errors.Is(err, ErrConfig) {
		t.Errorf("a shared approver spelled in another case: %v, want ErrConfig", err)
	}
	err = build(sepTool("pay", SingleApproval()), pol)
	if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), `"pay"`) {
		t.Errorf("a single own gate: %v, want ErrConfig naming the tool", err)
	}
	// Not escalated: no own gate, read-only, or exempt.
	if err := build(sepTool("pay", nil), pol); err != nil {
		t.Errorf("no own gate: %v", err)
	}
	if err := build(sepTool("read", SingleApproval(), WithSafety(Safety{ReadOnly: true})), pol); err != nil {
		t.Errorf("a read-only tool: %v", err)
	}
	if err := build(sepTool("pay", sepGate(2, "ops", "sec2"), TaintExempt("reviewed")), pol); err != nil {
		t.Errorf("an exempt tool: %v", err)
	}
	// Without the field the same overlap is allowed.
	if err := build(sepTool("pay", sepGate(2, "ops", "sec2")), TaintPolicy{Gate: esc}); err != nil {
		t.Errorf("no separation: %v", err)
	}
}

// ownTally is a terminal tally record of the gate on subject own that counted these approvers.
func ownTally(own string, approvedBy, deniedBy []string) Record {
	b, _ := json.Marshal(ApprovalTally{Need: 2, Approvers: append(append([]string{}, approvedBy...), deniedBy...), Approved: len(approvedBy), Denied: len(deniedBy), ApprovedBy: approvedBy, DeniedBy: deniedBy})
	return Record{Name: ApprovalTallyStep(own), Kind: StepValue, Result: b}
}

func instanceDecision(subject ApprovalSubject, approver string, approved bool) Record {
	at := "2026-10-13T12:14:03.123Z"
	return Record{Name: "approval:" + subject.ToolUseID + ":" + approver, Kind: StepApproval, ToolUseID: subject.ToolUseID, Approved: approved, ApproverSignature: &ApproverSignature{
		Approver: approver, ApproverAlg: fakeAlg, Signature: fakeSign(approver, decisionBytes(subject, approver, approved, at)), DecidedAt: at}}
}

// The instance's tally leaves out the approvers the own gate counted, and the tally says so.
func TestTally_AnInstanceExcludesTheApproversTheOwnGateCounted(t *testing.T) {
	inst := ApprovalSubject{RunID: "r1", ToolUseID: "c1#taint", ToolName: "pay"}
	pol := sepGate(2, "ops", "fin", "sec1", "sec2")
	vf := fakeVerifiers("ops", "fin", "sec1", "sec2")
	recs := []Record{
		ownTally("c1", []string{"ops", "fin"}, nil),
		instanceDecision(inst, "ops", true), instanceDecision(inst, "sec1", true), instanceDecision(inst, "sec2", true),
	}
	// Without the separation all three count.
	plain, _ := tallyApprovals(recs, recs, inst, *pol, vf)
	if plain.Approved != 3 || len(plain.ExcludedBySibling) != 0 {
		t.Fatalf("plain: %+v", plain)
	}
	sep := pol.separatedFrom("c1")
	got, checks := tallyApprovals(recs, recs, inst, *sep, vf)
	if got.Approved != 2 || !got.Passed() || strings.Join(got.ApprovedBy, ",") != "sec1,sec2" {
		t.Fatalf("separated: %+v", got)
	}
	if strings.Join(got.ExcludedBySibling, ",") != "ops,fin" || strings.Join(got.Excluded, ",") != "ops,fin" {
		t.Errorf("ExcludedBySibling %v, Excluded %v; want ops,fin in policy order", got.ExcludedBySibling, got.Excluded)
	}
	if checks[0].Approver != "ops" || checks[0].Counted || checks[0].Reason != ReasonSibling {
		t.Errorf("the excluded approver's decision: %+v", checks[0])
	}
	// A denier of the own gate is excluded too.
	recs[0] = ownTally("c1", []string{"ops"}, []string{"fin"})
	got, _ = tallyApprovals(recs, recs, inst, *sep, vf)
	if strings.Join(got.ExcludedBySibling, ",") != "ops,fin" {
		t.Errorf("a denier of the own gate: %v", got.ExcludedBySibling)
	}
}

// When the approvers left cannot reach Need, the instance is a denial, not a wait.
func TestTally_AnInstanceWithTooFewApproversLeftIsUnreachable(t *testing.T) {
	inst := ApprovalSubject{RunID: "r1", ToolUseID: "c1#taint", ToolName: "pay"}
	pol := sepGate(2, "ops", "fin", "sec1")
	vf := fakeVerifiers("ops", "fin", "sec1")
	recs := []Record{ownTally("c1", []string{"ops", "fin"}, nil), instanceDecision(inst, "sec1", true)}
	got, _ := tallyApprovals(recs, recs, inst, *pol.separatedFrom("c1"), vf)
	if got.Passed() || !got.Unreachable() {
		t.Errorf("one approver left for a need of 2: %+v, want unreachable", got)
	}
	plain, _ := tallyApprovals(recs, recs, inst, *pol, vf)
	if plain.Unreachable() {
		t.Errorf("the same count without separation is unreachable: %+v", plain)
	}
}

// A gate whose own gate has no recorded outcome cannot be separated from it: nobody counts.
func TestTally_AnInstanceWithNoOwnOutcomeCountsNobody(t *testing.T) {
	inst := ApprovalSubject{RunID: "r1", ToolUseID: "c1#taint", ToolName: "pay"}
	pol := sepGate(1, "sec1", "sec2")
	vf := fakeVerifiers("sec1", "sec2")
	recs := []Record{instanceDecision(inst, "sec1", true)}
	got, checks := tallyApprovals(recs, recs, inst, *pol.separatedFrom("c1"), vf)
	if got.Passed() || checks[0].Counted || checks[0].Reason != ReasonNoSibling {
		t.Errorf("no own outcome: %+v %+v", got, checks)
	}
}

// The count the gate records is the separated one, and replays as recorded; with approvers left it waits.
func TestTallyGate_RecordsTheSeparatedCount(t *testing.T) {
	ctx := context.Background()
	inst := ApprovalSubject{RunID: "r1", ToolUseID: "c1#taint", ToolName: "pay"}
	run := func(pol *ApprovalPolicy, counted []string) (*Journal, ApprovalTally, bool) {
		j := memJournal()
		for _, r := range []Record{ownTally("c1", counted, nil), instanceDecision(inst, "ops", true)} {
			if _, _, err := j.putNew(ctx, "r1", r.Name, r); err != nil {
				t.Fatal(err)
			}
		}
		tally, final, err := tallyGate(ctx, j, "r1", inst, pol.separatedFrom("c1"), fakeVerifiers("ops", "fin", "sec1"), "")
		if err != nil {
			t.Fatal(err)
		}
		return j, tally, final
	}
	// Every approver of the instance was counted by the own gate: the instance is a recorded denial.
	j, tally, final := run(sepGate(1, "ops", "fin"), []string{"ops", "fin"})
	if tally.Passed() || !tally.Unreachable() || !final || strings.Join(tally.ExcludedBySibling, ",") != "ops,fin" {
		t.Errorf("tally %+v final %v; want a recorded denial that names both excluded", tally, final)
	}
	again, final, err := tallyGate(ctx, j, "r1", inst, sepGate(1, "ops", "fin").separatedFrom("c1"), fakeVerifiers("ops", "fin"), "")
	if err != nil || !final || again.Passed() || strings.Join(again.ExcludedBySibling, ",") != "ops,fin" {
		t.Errorf("replay: %+v %v %v", again, final, err)
	}
	// An approver left: the approval of the excluded one does not pass it, and it waits.
	_, tally, final = run(sepGate(1, "ops", "sec1"), []string{"ops"})
	if tally.Passed() || tally.Unreachable() || final || strings.Join(tally.Pending, ",") != "sec1" {
		t.Errorf("tally %+v final %v; want a wait for sec1", tally, final)
	}
}

// escalate keeps the instance apart from the own gate when the policy says so and the call has one.
func TestEscalate_SeparatesTheInstanceFromTheOwnGate(t *testing.T) {
	ctx := withUnderTaint(context.Background(), parentDigest)
	for _, c := range []struct {
		name string
		sep  bool
		own  *ApprovalPolicy
		want string
	}{
		{"separate with an own gate", true, sepGate(2, "ops", "fin"), "c1"},
		{"separate, no own gate", true, nil, ""},
		{"no separation, an own gate", false, sepGate(2, "ops", "fin"), ""},
	} {
		a := &Agent{store: memJournal(), taint: &TaintPolicy{Gate: sepGate(2, "sec1", "sec2"), SeparateApprovers: c.sep}}
		spec := &ToolSpec{Name: "pay", Approval: c.own}
		e, err := a.escalate(ctx, "r1", ToolUse{ID: "c1", Name: "pay"}, spec, []taintMark{{Source: "inbox", Ref: "turn"}})
		if err != nil || e.gate == nil {
			t.Fatalf("%s: %+v, %v", c.name, e, err)
		}
		if e.gate.sibling != c.want {
			t.Errorf("%s: sibling %q, want %q", c.name, e.gate.sibling, c.want)
		}
		if a.taint.Gate.sibling != "" {
			t.Errorf("%s: the policy's own Gate was changed", c.name)
		}
	}
}

// A flow node's instance gate is kept apart from the node's own gate when the declarations say so and the node has one.
func TestFlowTaintNode_SeparatesTheInstanceFromTheNodesOwnGate(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name string
		sep  bool
		own  bool
		want string
	}{
		{"separate, an own gate", true, true, "node:pay"},
		{"separate, no own gate", true, false, ""},
		{"no separation, an own gate", false, true, ""},
	} {
		j, _ := beginNested(t, taintedCtx(taintMark{Source: "s", Ref: "node:x"}), hookDecls(func(d *journalhook.FlowTaint) { d.SeparateApprovers = c.sep }), nil)
		d, err := flowTaintNode(ctx, j, "r1", "node:pay", Safety{}, journalhook.FlowTaintUnder, c.own)
		if err != nil || d.Gate == nil {
			t.Fatalf("%s: %+v, %v", c.name, d, err)
		}
		if got := d.Gate.(*ApprovalPolicy).sibling; got != c.want {
			t.Errorf("%s: sibling %q, want %q", c.name, got, c.want)
		}
	}
}

// The declarations a run records hold the field, and a drive by a flow that dropped it is refused.
func TestFlowTaintEnter_RefusesAFlowThatDroppedSeparateApprovers(t *testing.T) {
	ctx := context.Background()
	sep := func(d *journalhook.FlowTaint) { d.SeparateApprovers = true }
	j, _ := beginNested(t, context.Background(), hookDecls(sep), nil)
	if _, err := flowTaintEnter(ctx, j, "r1", hookDecls(sep), nil, nil); err != nil {
		t.Errorf("the same declarations: %v", err)
	}
	if _, err := flowTaintEnter(ctx, j, "r1", hookDecls(nil), nil, nil); !errors.Is(err, ErrConfig) {
		t.Errorf("a flow that dropped SeparateApprovers: %v, want ErrConfig", err)
	}
	// A flow that adds it is not the one that started the run either.
	j2, _ := beginNested(t, context.Background(), hookDecls(nil), nil)
	if _, err := flowTaintEnter(ctx, j2, "r1", hookDecls(sep), nil, nil); !errors.Is(err, ErrConfig) {
		t.Errorf("a flow that added SeparateApprovers: %v, want ErrConfig", err)
	}
}

// The instance reads the own gate's terminal tally and never writes it: counting the instance (a pass, or a denial
// for want of approvers) leaves every record of the own gate as it was, so an own gate that passed never flips
// because of the instance (no circular exclusion).
func TestTallyGate_AnInstanceNeverWritesTheOwnGatesRecords(t *testing.T) {
	ctx := context.Background()
	inst := ApprovalSubject{RunID: "r1", ToolUseID: "c1#taint", ToolName: "pay"}
	for name, pol := range map[string]*ApprovalPolicy{
		"a pass":   sepGate(1, "ops", "sec1"),
		"a denial": sepGate(1, "ops"),
	} {
		j := memJournal()
		own := ownTally("c1", []string{"ops"}, nil)
		for _, r := range []Record{own, instanceDecision(inst, "ops", true), instanceDecision(inst, "sec1", true)} {
			if _, _, err := j.putNew(ctx, "r1", r.Name, r); err != nil {
				t.Fatal(err)
			}
		}
		before := ownRecords(t, j)
		if _, _, err := tallyGate(ctx, j, "r1", inst, pol.separatedFrom("c1"), fakeVerifiers("ops", "sec1"), ""); err != nil {
			t.Fatal(err)
		}
		after := ownRecords(t, j)
		if strings.Join(before, "|") != strings.Join(after, "|") {
			t.Errorf("%s: the own gate's records changed:\n%v\n%v", name, before, after)
		}
	}
}

// ownRecords is the records of the own gate on c1 (not its instance), by name and bytes.
func ownRecords(t *testing.T, j *Journal) []string {
	t.Helper()
	recs, err := j.History(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range recs {
		if strings.Contains(r.Name, "c1") && !strings.Contains(r.Name, "taint") {
			out = append(out, r.Name+"="+string(r.Result))
		}
	}
	return out
}
