package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// PR 10 of the taint build: marks that cross a run boundary (a sub-agent's run, a session's turns).

// inheritRig is a parent run that reads an untrusted source and then delegates to a sub-agent whose model
// asks for a write at once. The parent and the sub-agent each have a policy that refuses a tainted
// turn's side effect, so a write that ran was not gated.
type inheritRig struct {
	t         *testing.T
	writes    counter
	parentJ   *Journal
	childJ    *Journal
	sameStore bool
	writeOpts []ToolOption // the child write tool's options
}

func (r *inheritRig) run(parentTurns [][]Emit, parentOpts []Option, childOpts []Option) error {
	r.t.Helper()
	r.childJ = memJournal()
	if r.sameStore {
		r.childJ = r.parentJ
	}
	child := mustNew(&scriptModel{turns: [][]Emit{toolTurn("w1", "write", `{}`), textTurn("child done")}}, r.childJ,
		append([]Option{WithTools(r.writes.tool("write", r.writeOpts...))}, childOpts...)...)
	read := func(name, src string) Tool {
		return MustFunc(name, name, func(context.Context, struct{}) (string, error) { return "text", nil },
			WithSafety(Safety{ReadOnly: true}), Untrusted(src))
	}
	opts := append([]Option{WithTools(read("read_a", "a-src"), read("read_b", "b-src"), MustSubAgent("delegate", "delegate", child, TaintExempt("delegation is gated inside the child")))}, parentOpts...)
	_, err := mustNew(&scriptModel{turns: parentTurns}, r.parentJ, opts...).Run(context.Background(), "r", UserText("go"))
	return err
}

func newInheritRig(t *testing.T) *inheritRig { return &inheritRig{t: t, parentJ: memJournal()} }

var refuseTaint = []Option{WithTaintPolicy(TaintPolicy{Refuse: true})}

func TestTaintInherit_AChildOfATaintedTurnStartsTainted(t *testing.T) {
	for _, same := range []bool{false, true} {
		name := "child in another store"
		if same {
			name = "child in the parent's store"
		}
		t.Run(name, func(t *testing.T) {
			r := newInheritRig(t)
			r.sameStore = same
			err := r.run([][]Emit{toolTurn("c1", "read_a", `{}`), toolTurn("d1", "delegate", `{"task":"go"}`), textTurn("done")}, refuseTaint, refuseTaint)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := r.writes.n.Load(); got != 0 {
				t.Fatalf("the child's write ran %d times: its first turn was not tainted by what the parent read", got)
			}
			start := startOf(t, r.childJ, SubRunID("r", "d1"))
			got := start.inheritedMarks()
			if len(got) != 1 || got[0].Source != "a-src" || !strings.HasPrefix(got[0].Ref, "parent:tool:read_a/c1") {
				t.Fatalf("the child's inherited marks = %+v, want a-src read by read_a/c1", got)
			}
		})
	}
}

func TestTaintInherit_ATurnThatReadNothingPassesNothing(t *testing.T) {
	r := newInheritRig(t)
	// The read and the delegation are one turn: the delegation was decided before the read returned.
	turn := []Emit{
		{Event: ToolCallDelta{Index: 0, ID: "c1", Name: "read_a", ArgsFragment: []byte(`{}`)}},
		{Event: ToolCallDelta{Index: 1, ID: "d1", Name: "delegate", ArgsFragment: []byte(`{"task":"go"}`)}},
		{Event: Finish{Reason: "tool_use"}},
	}
	if err := r.run([][]Emit{turn, textTurn("done")}, refuseTaint, refuseTaint); err != nil {
		t.Fatal(err)
	}
	if got := r.writes.n.Load(); got != 1 {
		t.Fatalf("the child's write ran %d times, want 1 (the delegation's turn had read nothing)", got)
	}
	start := startOf(t, r.childJ, SubRunID("r", "d1"))
	if l := start.inheritedMarks(); l != nil {
		t.Fatalf("inherited marks = %+v, want none", l)
	}
	if _, has := start.Ext[extTaintInherited]; has {
		t.Error("an empty list was recorded: a run that inherits no marks must be byte for byte as before")
	}
	if start.taintDigest() == "" {
		t.Error("the child did not inherit the status")
	}
}

func TestTaintInherit_MarksPassOnlyWhenTheParentIsUnderTaint(t *testing.T) {
	// A parent started with no policy reads an untrusted source: it is not under taint, so the child
	// inherits neither the status nor the marks, and a policy of the child's own finds nothing to gate.
	r := newInheritRig(t)
	if err := r.run([][]Emit{toolTurn("c1", "read_a", `{}`), toolTurn("d1", "delegate", `{"task":"go"}`), textTurn("done")}, nil, refuseTaint); err != nil {
		t.Fatal(err)
	}
	if got := r.writes.n.Load(); got != 1 {
		t.Fatalf("the child's write ran %d times, want 1 (the parent was not under taint)", got)
	}
	start := startOf(t, r.childJ, SubRunID("r", "d1"))
	if start.inheritedMarks() != nil || start.taintDigest() == "" {
		t.Fatalf("child start: marks %+v, digest %q; want no marks and its own agent's digest", start.inheritedMarks(), start.taintDigest())
	}
}

func TestTaintInherit_TheListIsOneMarkPerSourceInAFixedOrder(t *testing.T) {
	r := newInheritRig(t)
	turns := [][]Emit{toolTurn("c1", "read_b", `{}`), toolTurn("c2", "read_a", `{}`), toolTurn("c3", "read_b", `{}`), toolTurn("d1", "delegate", `{"task":"go"}`), textTurn("done")}
	if err := r.run(turns, refuseTaint, refuseTaint); err != nil {
		t.Fatal(err)
	}
	var sources []string
	for _, m := range startOf(t, r.childJ, SubRunID("r", "d1")).inheritedMarks() {
		sources = append(sources, m.Source)
	}
	if !slices.Equal(sources, []string{"a-src", "b-src"}) {
		t.Fatalf("inherited sources = %v, want [a-src b-src] (sorted, once each)", sources)
	}
}

func TestTaintInherit_ADriveWithOtherMarksIsAConfigError(t *testing.T) {
	r := newInheritRig(t)
	r.writeOpts = []ToolOption{WithApproval(SingleApproval())} // the child pauses at its write, so its run is still open
	if err := r.run([][]Emit{toolTurn("c1", "read_a", `{}`), toolTurn("d1", "delegate", `{"task":"go"}`), textTurn("done")}, refuseTaint, refuseTaint); !IsPause(err) {
		t.Fatalf("run = %v, want the child's pause", err)
	}
	child := mustNew(&scriptModel{turns: [][]Emit{textTurn("again")}}, r.childJ, WithTools(r.writes.tool("write")), WithTaintPolicy(TaintPolicy{Refuse: true}))
	other := withCallMarks(withRunContext(context.Background(), r.parentJ, "r", "d1", false), []taintMark{{Source: "b-src", Ref: "tool:read_b/c9"}})
	task := UserText("go")
	_, _, _, err := child.drive(other, SubRunID("r", "d1"), &driveSpec{input: &task, strictSaga: true})
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("a drive with other inherited marks = %v, want ErrConfig", err)
	}
	if !strings.Contains(err.Error(), "inherited taint marks") {
		t.Fatalf("ErrConfig for another reason: %v", err)
	}
}

// upRig is a parent that delegates to a sub-agent and then asks for a write. The child reads an untrusted
// source (or not) in its own journal.
type upRig struct {
	t         *testing.T
	writes    counter
	parentJ   *Journal
	childJ    *Journal
	sameStore bool
}

func (r *upRig) run(childReads bool, childPolicy bool, delegate string) error {
	r.t.Helper()
	r.childJ = memJournal()
	if r.sameStore {
		r.childJ = r.parentJ
	}
	var childTools []Tool
	childTurns := [][]Emit{textTurn("child answer")}
	if childReads {
		childTools = append(childTools, MustFunc("read_a", "read", func(context.Context, struct{}) (string, error) { return "text", nil },
			WithSafety(Safety{ReadOnly: true}), Untrusted("a-src")))
		childTurns = [][]Emit{toolTurn("r1", "read_a", `{}`), textTurn("child answer")}
	}
	var copts []Option
	if childPolicy {
		copts = refuseTaint
	}
	child := mustNew(&scriptModel{turns: childTurns}, r.childJ, append([]Option{WithTools(childTools...)}, copts...)...)
	parent := mustNew(&scriptModel{turns: [][]Emit{toolTurn("d1", delegate, `{"task":"go"}`), toolTurn("w1", "write", `{}`), textTurn("done")}}, r.parentJ,
		WithTools(MustSubAgent(delegate, "delegate", child), r.writes.tool("write")), WithTaintPolicy(TaintPolicy{Refuse: true}))
	_, err := parent.Run(context.Background(), "r", UserText("go"))
	return err
}

func TestTaintInherit_ATaintedChildTaintsTheParentsNextTurn(t *testing.T) {
	for _, same := range []bool{false, true} {
		name := "child in another store"
		if same {
			name = "child in the parent's store"
		}
		t.Run(name, func(t *testing.T) {
			r := &upRig{t: t, parentJ: memJournal(), sameStore: same}
			if err := r.run(true, false, "delegate"); err != nil {
				t.Fatal(err)
			}
			if got := r.writes.n.Load(); got != 0 {
				t.Fatalf("the parent's write ran %d times after a tainted child's answer: the content was laundered", got)
			}
			recs, _ := r.parentJ.History(context.Background(), "r")
			var found bool
			for _, rec := range recs {
				if rec.Kind == StepToolResult && rec.ToolUseID == "d1" {
					found = rec.Safety != nil && rec.Safety.Untrusted == "subagent:delegate"
				}
			}
			if !found {
				t.Fatal("the parent's tool_result for the sub-agent call does not carry the mark subagent:delegate")
			}
		})
	}
}

func TestTaintInherit_ACleanChildLeavesTheParentsNextTurnClean(t *testing.T) {
	r := &upRig{t: t, parentJ: memJournal()}
	if err := r.run(false, false, "delegate"); err != nil {
		t.Fatal(err)
	}
	if got := r.writes.n.Load(); got != 1 {
		t.Fatalf("the parent's write ran %d times, want 1 (the child read nothing untrusted)", got)
	}
	recs, _ := r.parentJ.History(context.Background(), "r")
	for _, rec := range recs {
		if rec.Kind == StepToolResult && rec.ToolUseID == "d1" && rec.Safety != nil && rec.Safety.Untrusted != "" {
			t.Fatalf("a clean child marked its parent's result: %q", rec.Safety.Untrusted)
		}
	}
}

func TestTaintInherit_TheMarkOfAChildCarriesThroughAGrandchild(t *testing.T) {
	ctx := context.Background()
	var writes counter
	grand := mustNew(&scriptModel{turns: [][]Emit{toolTurn("r1", "read_a", `{}`), textTurn("grand answer")}}, memJournal(), WithTools(
		MustFunc("read_a", "read", func(context.Context, struct{}) (string, error) { return "text", nil }, WithSafety(Safety{ReadOnly: true}), Untrusted("a-src"))), WithTaintPolicy(TaintPolicy{Refuse: true}))
	child := mustNew(&scriptModel{turns: [][]Emit{toolTurn("g1", "grand", `{"task":"go"}`), textTurn("child answer")}}, memJournal(), WithTools(MustSubAgent("grand", "grand", grand, TaintExempt("test"))), WithTaintPolicy(TaintPolicy{Refuse: true}))
	parent := mustNew(&scriptModel{turns: [][]Emit{toolTurn("d1", "child", `{"task":"go"}`), toolTurn("w1", "write", `{}`), textTurn("done")}}, memJournal(),
		WithTools(MustSubAgent("child", "child", child, TaintExempt("test")), writes.tool("write")), WithTaintPolicy(TaintPolicy{Refuse: true}))
	if _, err := parent.Run(ctx, "r", UserText("go")); err != nil {
		t.Fatal(err)
	}
	if got := writes.n.Load(); got != 0 {
		t.Fatalf("the parent's write ran %d times: a grandchild's content reached it unmarked", got)
	}
}

func TestSubAgentMarkSource_IsCappedOnARuneBoundary(t *testing.T) {
	long := strings.Repeat("é", 100) // 200 bytes
	got := subAgentMarkSource(long)
	if len(got) > maxProvenanceName || !strings.HasPrefix(got, "subagent:") || !utf8.ValidString(got) {
		t.Fatalf("source = %q (%d bytes), want valid UTF-8 within %d bytes", got, len(got), maxProvenanceName)
	}
	if again := subAgentMarkSource(long); again != got {
		t.Fatal("the cap is not deterministic")
	}
	if err := checkProvenanceName("test", got); err != nil {
		t.Fatalf("the capped source is not a valid provenance name: %v", err)
	}
	if subAgentMarkSource("delegate") != "subagent:delegate" {
		t.Fatal("a short name is changed")
	}
}

// A child that read nothing itself but started tainted (it inherited its parent's marks) ended tainted:
// its answer is marked, as it was shaped by what the parent's turn had been shown.
func TestTaintInherit_AChildThatStartedTaintedEndsTainted(t *testing.T) {
	r := newInheritRig(t)
	r.writeOpts = nil
	if err := r.run([][]Emit{toolTurn("c1", "read_a", `{}`), toolTurn("d1", "delegate", `{"task":"go"}`), textTurn("done")}, refuseTaint, refuseTaint); err != nil {
		t.Fatal(err)
	}
	recs, _ := r.parentJ.History(context.Background(), "r")
	for _, rec := range recs {
		if rec.Kind == StepToolResult && rec.ToolUseID == "d1" {
			if rec.Safety == nil || rec.Safety.Untrusted != "subagent:delegate" {
				t.Fatalf("the answer of a child that inherited marks is not marked: %+v", rec.Safety)
			}
			return
		}
	}
	t.Fatal("no tool_result for d1")
}
