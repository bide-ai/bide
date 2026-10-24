package agent

import (
	"context"
	"errors"
	"iter"
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

// failLoadAt fails the n-th Load of one run (n counted from 1; 0 never) with a storage error, and counts them.
type failLoadAt struct {
	Store
	run    string
	failAt int
	loads  *int
}

func (f failLoadAt) Unwrap() Store { return f.Store }
func (f failLoadAt) Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error] {
	if runID == f.run {
		*f.loads++
		if *f.loads == f.failAt {
			return func(yield func(Entry, error) bool) {
				yield(Entry{}, errors.New("injected: the child's journal cannot be read"))
			}
		}
	}
	return f.Store.Load(ctx, runID, after)
}

// If the child's journal cannot be read for its mark, the parent does not finish the call unmarked: the
// call has no outcome yet (subRunUnfinished), the parent stops with a storage error, and its next turn
// does not run. A retry reads the journal and marks the answer.
func TestTaintInherit_AnUnreadableChildJournalFailsClosed(t *testing.T) {
	ctx := context.Background()
	const sub = "r>d1"
	run := func(parentJ *Journal, childStore Store, writes *counter, turns ...[]Emit) error {
		childJ := mustJournal(childStore)
		child := mustNew(&scriptModel{turns: [][]Emit{toolTurn("r1", "read_a", `{}`), textTurn("child answer")}}, childJ, WithTools(
			MustFunc("read_a", "read", func(context.Context, struct{}) (string, error) { return "text", nil }, WithSafety(Safety{ReadOnly: true}), Untrusted("a-src"))))
		parent := mustNew(&scriptModel{turns: turns}, parentJ,
			WithTools(MustSubAgent("delegate", "delegate", child), writes.tool("write")), WithTaintPolicy(TaintPolicy{Refuse: true}))
		_, err := parent.Run(ctx, "r", UserText("go"))
		return err
	}
	// A clean run counts the loads of the child's journal; the last one is the read of its mark.
	var total int
	var w0 counter
	if err := run(memJournal(), failLoadAt{Store: NewMemStore(), run: sub, loads: &total}, &w0, toolTurn("d1", "delegate", `{"task":"go"}`), toolTurn("w1", "write", `{}`), textTurn("done")); err != nil {
		t.Fatal(err)
	}
	if total == 0 || w0.n.Load() != 0 {
		t.Fatalf("control run: %d loads of the child, %d writes; want loads and no write (the child's answer taints the parent)", total, w0.n.Load())
	}
	// The same run with that last read failing.
	var seen int
	var w counter
	parentJ := memJournal()
	childStore := failLoadAt{Store: NewMemStore(), run: sub, failAt: total, loads: &seen}
	err := run(parentJ, childStore, &w, toolTurn("d1", "delegate", `{"task":"go"}`), toolTurn("w1", "write", `{}`), textTurn("done"))
	if !errors.Is(err, ErrStorage) {
		t.Fatalf("with the mark unreadable the parent returned %v, want a storage error", err)
	}
	if w.n.Load() != 0 {
		t.Fatalf("the parent's write ran %d times after an unreadable child journal", w.n.Load())
	}
	recs, _ := parentJ.History(ctx, "r")
	for _, r := range recs {
		if r.Kind == StepToolResult && r.ToolUseID == "d1" {
			t.Fatalf("the parent recorded a result for the sub-agent call whose mark it could not read: %+v", r.Safety)
		}
	}
	// A retry (the read works now) finishes the call with its mark, and the write is refused.
	seen = 1 << 20
	if err := run(parentJ, childStore, &w, toolTurn("w1", "write", `{}`), textTurn("done")); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if w.n.Load() != 0 {
		t.Fatalf("after the retry the write ran %d times; the answer must taint the parent's next turn", w.n.Load())
	}
}

// viaRig is a parent whose tool "via" starts a programmatic sub-run (WithSubRuns, RunInfo.SubRunFor) and
// returns its answer, and then asks for a write.
type viaRig struct {
	t       *testing.T
	writes  counter
	parentJ *Journal
}

// run drives the parent; childReads makes the child read an untrusted source; childStore is the child's
// store; failAfter makes via fail after its sub-run finished.
func (r *viaRig) run(childReads bool, childStore Store, failAfter bool, turns ...[]Emit) error {
	var tools []Tool
	childTurns := [][]Emit{textTurn("child answer")}
	if childReads {
		tools = append(tools, MustFunc("read_c", "read", func(context.Context, struct{}) (string, error) { return "text", nil }, WithSafety(Safety{ReadOnly: true}), Untrusted("c-src")))
		childTurns = [][]Emit{toolTurn("r1", "read_c", `{}`), textTurn("child answer")}
	}
	child := mustNew(&scriptModel{turns: childTurns}, mustJournal(childStore), append([]Option{WithTools(tools...)}, refuseTaint...)...)
	via := MustFunc("via", "via", func(ctx context.Context, _ struct{}) (string, error) {
		info, _ := RunInfoFrom(ctx)
		res, err := child.Run(ctx, info.SubRunFor("child"), UserText("work"))
		if err != nil {
			return "", err
		}
		if failAfter {
			return res.Message.Text(), errors.New("via failed after its sub-run")
		}
		return res.Message.Text(), nil
	}, WithSafety(Safety{ReadOnly: true}), WithSubRuns(func(string) *Agent { return child }))
	parent := mustNew(&scriptModel{turns: turns}, r.parentJ, WithTools(via, r.writes.tool("write")), WithTaintPolicy(TaintPolicy{Refuse: true}))
	_, err := parent.Run(context.Background(), "r", UserText("go"))
	return err
}

func (r *viaRig) markOf(call string) (string, bool) {
	recs, _ := r.parentJ.History(context.Background(), "r")
	for _, rec := range recs {
		if rec.Kind == StepToolResult && rec.ToolUseID == call {
			if rec.Safety == nil {
				return "", true
			}
			return rec.Safety.Untrusted, true
		}
	}
	return "", false
}

var viaTurns = [][]Emit{toolTurn("p1", "via", `{}`), toolTurn("p2", "write", `{}`), textTurn("done")}

// A tool that starts a programmatic sub-run and returns its answer is a way for hostile content to reach
// the next turn like a sub-agent's: the answer carries the mark of what the sub-run read.
func TestTaintInherit_AProgrammaticSubRunMarksTheAnswerToo(t *testing.T) {
	r := &viaRig{t: t, parentJ: memJournal()}
	if err := r.run(true, NewMemStore(), false, viaTurns...); err != nil {
		t.Fatal(err)
	}
	if src, ok := r.markOf("p1"); !ok || src != "subagent:via" {
		t.Fatalf("via's result carries mark %q (found %v), want subagent:via", src, ok)
	}
	if r.writes.n.Load() != 0 {
		t.Fatalf("the write ran %d times after a sub-run read untrusted content: laundered", r.writes.n.Load())
	}
	clean := &viaRig{t: t, parentJ: memJournal()}
	if err := clean.run(false, NewMemStore(), false, viaTurns...); err != nil {
		t.Fatal(err)
	}
	if src, _ := clean.markOf("p1"); src != "" || clean.writes.n.Load() != 1 {
		t.Fatalf("a clean sub-run: mark %q, %d writes; want none and 1", src, clean.writes.n.Load())
	}
}

// A call that failed after its sub-run, and whose sub-run's journal cannot be read for the mark, is marked
// all the same: the error text the model reads may carry what the sub-run read.
func TestTaintInherit_AFailedCallWithAnUnreadableSubRunJournalIsMarked(t *testing.T) {
	// Learn which run id the sub-run has and how many times it is loaded: run once, counting every Load.
	all := &loadsByRun{Store: NewMemStore(), n: map[string]int{}}
	probe := &viaRig{t: t, parentJ: memJournal()}
	if err := probe.run(true, all, true, viaTurns...); err != nil {
		t.Fatal(err)
	}
	var childRun string
	for id := range all.n {
		if strings.HasPrefix(id, "r>") { // the sub-run, not a control run such as @stop
			childRun = id
		}
	}
	if childRun == "" {
		t.Fatal("the sub-run did not journal to the child's store")
	}
	seen := 0
	r := &viaRig{t: t, parentJ: memJournal()}
	err := r.run(true, failLoadAt{Store: NewMemStore(), run: childRun, failAt: all.n[childRun], loads: &seen}, true, viaTurns...)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if src, ok := r.markOf("p1"); !ok || src == "" {
		t.Fatalf("a failed call with an unreadable sub-run journal is unmarked (mark %q, found %v)", src, ok)
	}
	if r.writes.n.Load() != 0 {
		t.Fatalf("the write ran %d times after the unmarked failure", r.writes.n.Load())
	}
}

// loadsByRun counts the Loads of each run.
type loadsByRun struct {
	Store
	n map[string]int
}

func (l *loadsByRun) Unwrap() Store { return l.Store }
func (l *loadsByRun) Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error] {
	l.n[runID]++
	return l.Store.Load(ctx, runID, after)
}

// A resume under changed declarations. The marks a run recorded stay; a declaration is read as the larger
// of the recorded and the current one (never lower), and nothing rewrites what the journal holds.
func TestTaintInherit_AResumeUnderChangedDeclarations(t *testing.T) {
	ctx := context.Background()
	drive := func(j *Journal, writes *counter, untrusted bool, turns ...[]Emit) error {
		var ropts []ToolOption
		ropts = append(ropts, WithSafety(Safety{ReadOnly: true}))
		if untrusted {
			ropts = append(ropts, Untrusted("a-src"))
		}
		read := MustFunc("read_a", "read", func(context.Context, struct{}) (string, error) { return "text", nil }, ropts...)
		a := mustNew(&scriptModel{turns: turns}, j, WithTools(read, writes.tool("write", WithApproval(SingleApproval()))), WithTaintPolicy(TaintPolicy{Refuse: true}))
		_, err := a.Run(ctx, "r", UserText("go"))
		return err
	}
	readThenWrite := [][]Emit{toolTurn("c1", "read_a", `{}`), toolTurn("w1", "write", `{}`), textTurn("done")}
	readMark := func(j *Journal) string {
		recs, _ := j.History(ctx, "r")
		for _, r := range recs {
			if r.Kind == StepToolResult && r.ToolUseID == "c1" && r.Safety != nil {
				return r.Safety.Untrusted
			}
		}
		return "?"
	}
	for _, tc := range []struct {
		name         string
		first, again bool // read_a is declared Untrusted in the first drive, in the resume
		wantWrites   int32
		wantRecorded string
	}{
		{"lost: the recorded mark stays and gates the write", true, false, 0, "a-src"},
		{"gained: the current declaration counts, the journal is not rewritten", false, true, 0, ""},
		{"unchanged and undeclared: nothing gates the write", false, false, 1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j, writes := memJournal(), &counter{}
			if err := drive(j, writes, tc.first, readThenWrite...); !IsPause(err) {
				t.Fatalf("first drive = %v, want a pause at the write's own gate", err)
			}
			if err := Approve(ctx, j, "r", "w1", true); err != nil {
				t.Fatal(err)
			}
			if err := drive(j, writes, tc.again, textTurn("done")); err != nil {
				t.Fatalf("resume: %v", err)
			}
			if got := writes.n.Load(); got != tc.wantWrites {
				t.Fatalf("writes = %d, want %d", got, tc.wantWrites)
			}
			if got := readMark(j); got != tc.wantRecorded {
				t.Fatalf("the read's recorded mark = %q, want %q (declarations do not rewrite the journal)", got, tc.wantRecorded)
			}
		})
	}
}

// A chain of programmatic sub-runs: the grandchild read an untrusted source, so the child's result for the
// call that started it is marked, so the parent's result for the call that started the child is.
func TestTaintInherit_AProgrammaticChainMarksEachLevel(t *testing.T) {
	ctx := context.Background()
	var writes counter
	sub := func(name string, run func(ctx context.Context, info RunInfo) (string, error), agentFor func() *Agent) Tool {
		return MustFunc(name, name, func(ctx context.Context, _ struct{}) (string, error) {
			info, _ := RunInfoFrom(ctx)
			return run(ctx, info)
		}, WithSafety(Safety{ReadOnly: true}), WithSubRuns(func(string) *Agent { return agentFor() }))
	}
	grand := mustNew(&scriptModel{turns: [][]Emit{toolTurn("r1", "read_c", `{}`), textTurn("grand answer")}}, memJournal(), append([]Option{WithTools(
		MustFunc("read_c", "read", func(context.Context, struct{}) (string, error) { return "text", nil }, WithSafety(Safety{ReadOnly: true}), Untrusted("c-src")))}, refuseTaint...)...)
	childJ := memJournal()
	via2 := sub("via2", func(ctx context.Context, info RunInfo) (string, error) {
		res, err := grand.Run(ctx, info.SubRunFor("grand"), UserText("work"))
		if err != nil {
			return "", err
		}
		return res.Message.Text(), nil
	}, func() *Agent { return grand })
	child := mustNew(&scriptModel{turns: [][]Emit{toolTurn("g1", "via2", `{}`), textTurn("child answer")}}, childJ, append([]Option{WithTools(via2)}, refuseTaint...)...)
	via1 := sub("via1", func(ctx context.Context, info RunInfo) (string, error) {
		res, err := child.Run(ctx, info.SubRunFor("child"), UserText("work"))
		if err != nil {
			return "", err
		}
		return res.Message.Text(), nil
	}, func() *Agent { return child })
	parentJ := memJournal()
	parent := mustNew(&scriptModel{turns: [][]Emit{toolTurn("p1", "via1", `{}`), toolTurn("p2", "write", `{}`), textTurn("done")}}, parentJ,
		WithTools(via1, writes.tool("write")), WithTaintPolicy(TaintPolicy{Refuse: true}))
	if _, err := parent.Run(ctx, "r", UserText("go")); err != nil {
		t.Fatal(err)
	}
	markIn := func(j *Journal, run, call string) string {
		recs, _ := j.History(ctx, run)
		for _, r := range recs {
			if r.Kind == StepToolResult && r.ToolUseID == call && r.Safety != nil {
				return r.Safety.Untrusted
			}
		}
		return "?"
	}
	childRun := RunInfo{RunID: "r", ToolUseID: "p1"}.SubRunFor("child")
	if got := markIn(childJ, childRun, "g1"); got != "subagent:via2" {
		t.Fatalf("the child's result for the call that started the grandchild = %q, want subagent:via2", got)
	}
	if got := markIn(parentJ, "r", "p1"); got != "subagent:via1" {
		t.Fatalf("the parent's result for the call that started the child = %q, want subagent:via1", got)
	}
	if writes.n.Load() != 0 {
		t.Fatalf("the write ran %d times: the grandchild's content was laundered through the chain", writes.n.Load())
	}
}
