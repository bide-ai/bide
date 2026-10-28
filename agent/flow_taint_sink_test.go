package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/internal/journalhook"
)

// The pieces of taint coming up out of a nested flow (flow_taint_sink.go): the child link, the sink in a context,
// the marks of a run's own, and the fold a ResolveHalt does.

func TestTaintChild_IsWrittenOnceAndReadStrictly(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	for range 2 {
		if err := putTaintChild(ctx, j, "p1", "node:call", "c1", childKindFlow); err != nil {
			t.Fatal(err)
		}
	}
	recs, _ := j.History(ctx, "p1")
	var link Record
	n := 0
	for _, r := range recs {
		if strings.HasPrefix(r.Name, taintChildPrefix) {
			link, n = r, n+1
		}
	}
	if n != 1 {
		t.Fatalf("%d taint-child records, want 1", n)
	}
	if rec, err := decodeTaintChild(link); err != nil || rec.NodeKey != "node:call" || rec.RunID != "c1" {
		t.Errorf("decode = %+v, %v", rec, err)
	}
	for what, r := range map[string]Record{
		"another format":     {Name: link.Name, Result: []byte(`{"format":"x","node_key":"node:call","run_id":"c1","kind":"flow"}`)},
		"a name not its own": {Name: taintChildStep("node:other", "c1"), Result: link.Result},
		"an empty run":       {Name: taintChildStep("node:call", ""), Result: []byte(`{"format":"` + taintChildFormat + `","node_key":"node:call","run_id":"","kind":"flow"}`)},
		"a field too many":   {Name: link.Name, Result: []byte(`{"format":"` + taintChildFormat + `","node_key":"node:call","run_id":"c1","kind":"flow","x":1}`)},
		"an unknown kind":    {Name: link.Name, Result: []byte(`{"format":"` + taintChildFormat + `","node_key":"node:call","run_id":"c1","kind":"x"}`)},
		"no kind":            {Name: link.Name, Result: []byte(`{"format":"` + taintChildFormat + `","node_key":"node:call","run_id":"c1"}`)},
		"not json":           {Name: link.Name, Result: []byte(`nope`)},
	} {
		if _, err := decodeTaintChild(r); !errors.Is(err, ErrStorage) {
			t.Errorf("%s: %v, want ErrStorage", what, err)
		}
	}
	if err := putTaintChild(ctx, j, "p1", "not a node key", "c1", childKindFlow); !errors.Is(err, ErrConfig) {
		t.Errorf("a link for a name that is not a node key: %v, want ErrConfig", err)
	}
	if !IsReservedStepName(taintChildPrefix + "x") {
		t.Error("taint-child: is not a reserved step name")
	}
	if c, _, _ := classOf(taintChildPrefix + "x"); c != classProtected {
		t.Errorf("taint-child: is class %v, want protected", c)
	}
}

func TestLinkChild_HoldsTheChildToTheSinksJournal(t *testing.T) {
	j := memJournal()
	bound := withMarkSink(context.Background(), j, "p1", "node:call")
	if err := linkChild(context.Background(), j, "c1", childKindFlow); err != nil {
		t.Errorf("no sink: %v", err)
	}
	if err := linkChild(bound, memJournal(), "c1", childKindFlow); !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "journal") {
		t.Errorf("another journal: %v, want ErrConfig naming the journal", err)
	}
	if err := linkChild(bound, j, "p1", childKindFlow); !errors.Is(err, ErrConfig) {
		t.Errorf("the parent's own ID: %v, want ErrConfig", err)
	}
	if err := linkChild(bound, j, "c1", childKindFlow); err != nil {
		t.Fatal(err)
	}
	recs, _ := j.History(context.Background(), "p1")
	if !hasValueStepNamed(recs, taintChildStep("node:call", "c1")) {
		t.Error("the link was not written")
	}
	// A journal over the same store is the same journal: the marks go into one store.
	jb, err := NewJournal(j.Store())
	if err != nil {
		t.Fatal(err)
	}
	if err := linkChild(bound, jb, "c2", childKindFlow); err != nil {
		t.Errorf("a journal over the same store: %v", err)
	}
}

func hasValueStepNamed(recs []Record, name string) bool {
	for _, r := range recs {
		if r.Kind == StepValue && r.Name == name {
			return true
		}
	}
	return false
}

// startTainted begins a flow run under taint with the declarations d in j.
func startTainted(t *testing.T, j *Journal, run string, d flowTaintDecls) {
	t.Helper()
	if _, _, _, err := beginRun(context.Background(), j, run, flowStart().withFlowTaint(d)); err != nil {
		t.Fatal(err)
	}
}

func recordNode(t *testing.T, j *Journal, run, key string) {
	t.Helper()
	if _, _, err := j.putNew(context.Background(), run, key, Record{Kind: StepValue, Result: []byte(`1`)}); err != nil {
		t.Fatal(err)
	}
}

func marksFor(t *testing.T, j *Journal, run, node string) []string {
	t.Helper()
	recs, _ := j.History(context.Background(), run)
	var out []string
	for _, r := range recs {
		if strings.HasPrefix(r.Name, taintMarkPrefix) {
			rec, err := decodeTaintMark(r)
			if err != nil {
				t.Fatal(err)
			}
			if rec.NodeKey == node {
				out = append(out, rec.Source)
			}
		}
	}
	return out
}

func TestOwnMarkSources_AreTheNodesDeclaredSourcesAndWhatItsChildrenBroughtUp(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	d := flowTaintDecls{Policy: flowTaintPolicy{Refuse: true}, Nodes: map[string]flowNodeTaint{"a": {Untrusted: []string{"s2", "s1"}}, "b": {Untrusted: []string{"s3"}}}}
	startTainted(t, j, "c1", d)
	if _, err := putTaintMark(ctx, j, "c1", "node:a", "deep"); err != nil {
		t.Fatal(err)
	}
	if _, err := putTaintMark(ctx, j, "c1", "node:b", "other"); err != nil {
		t.Fatal(err)
	}
	recs, _ := j.History(ctx, "c1")
	// Before a's result is recorded only the mark its child brought up is a's.
	if got, err := ownMarkSources(recs, d.nodeUntrusted(), "node:a"); err != nil || strings.Join(got, ",") != "deep" {
		t.Errorf("before the result: %v, %v; want [deep]", got, err)
	}
	recordNode(t, j, "c1", "node:a")
	recs, _ = j.History(ctx, "c1")
	if got, _ := ownMarkSources(recs, d.nodeUntrusted(), "node:a"); strings.Join(got, ",") != "deep,s1,s2" {
		t.Errorf("after the result: %v; want deep,s1,s2 (sorted, b's marks not among them)", got)
	}
	// Another node's declared source is that node's once its result is recorded, not a's.
	recordNode(t, j, "c1", "node:b")
	recs, _ = j.History(ctx, "c1")
	if got, _ := ownMarkSources(recs, d.nodeUntrusted(), "node:a"); strings.Join(got, ",") != "deep,s1,s2" {
		t.Errorf("after b's result: %v; want deep,s1,s2 (not s3, which is b's)", got)
	}
	if got, _ := ownMarkSources(recs, d.nodeUntrusted(), ""); strings.Join(got, ",") != "deep,other,s1,s2,s3" {
		t.Errorf("every node: %v; want deep,other,s1,s2,s3", got)
	}
	// A taint-mark record that does not read is a fault, never no mark.
	bad := append(recs, Record{Kind: StepValue, Name: taintMarkPrefix + "x", Result: []byte(`{}`)})
	if _, err := ownMarkSources(bad, d.nodeUntrusted(), "node:a"); !errors.Is(err, ErrStorage) {
		t.Errorf("an unreadable mark: %v, want ErrStorage", err)
	}
}

func TestFlowTaintSink_NeedsASinkAndARunUnderTaint(t *testing.T) {
	ctx := context.Background()
	parent, child := memJournal(), memJournal()
	d := flowTaintDecls{Policy: flowTaintPolicy{Refuse: true}, Nodes: map[string]flowNodeTaint{"a": {Untrusted: []string{"s"}}}}
	startTainted(t, child, "c1", d)
	recordNode(t, child, "c1", "node:a")
	if err := flowTaintSink(ctx, child, "c1", "node:a"); err != nil {
		t.Errorf("no sink: %v", err)
	}
	sink := withMarkSink(ctx, parent, "p1", "node:call")
	if err := flowTaintSink(sink, child, "c1", "node:a"); err != nil {
		t.Fatal(err)
	}
	if got := marksFor(t, parent, "p1", "node:call"); strings.Join(got, ",") != "s" {
		t.Errorf("sent %v, want [s]", got)
	}
	if err := flowTaintSink(sink, child, "c1", "node:a"); err != nil || len(marksFor(t, parent, "p1", "node:call")) != 1 {
		t.Errorf("sending again: %v, %v marks; want insert-once", err, marksFor(t, parent, "p1", "node:call"))
	}
	// A run that is not under taint has nothing to send.
	plain := memJournal()
	recordNode(t, plain, "c2", "node:a")
	if _, err := putTaintMark(ctx, plain, "c2", "node:a", "planted"); err != nil {
		t.Fatal(err)
	}
	if err := flowTaintSink(withMarkSink(ctx, parent, "p2", "node:call"), plain, "c2", "node:a"); err != nil || len(marksFor(t, parent, "p2", "node:call")) != 0 {
		t.Errorf("a run not under taint: %v, %v", err, marksFor(t, parent, "p2", "node:call"))
	}
	if err := flowTaintSink(sink, "not a journal", "c1", "node:a"); !errors.Is(err, ErrConfig) {
		t.Errorf("a journal that is not one: %v, want ErrConfig", err)
	}
}

func TestFoldChildMarks_FollowsLinksReadsEveryRecordAndIsBounded(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	d := flowTaintDecls{Policy: flowTaintPolicy{Refuse: true}, Nodes: map[string]flowNodeTaint{"a": {Untrusted: []string{"s"}}}}
	startTainted(t, j, "p1", flowTaintDecls{Policy: flowTaintPolicy{Refuse: true}})
	startTainted(t, j, "c1", d)
	startTainted(t, j, "g1", d)
	recordNode(t, j, "c1", "node:a")
	recordNode(t, j, "g1", "node:a")
	if err := putTaintChild(ctx, j, "p1", "node:call", "c1", childKindFlow); err != nil {
		t.Fatal(err)
	}
	if err := putTaintChild(ctx, j, "p1", "node:other", "g1", childKindFlow); err != nil {
		t.Fatal(err)
	}
	// A link back to the parent does not loop.
	if err := putTaintChild(ctx, j, "c1", "node:a", "p1", childKindFlow); err != nil {
		t.Fatal(err)
	}
	if err := foldChildMarks(ctx, j, "p1", "node:call"); err != nil {
		t.Fatal(err)
	}
	if got := marksFor(t, j, "p1", "node:call"); strings.Join(got, ",") != "s" {
		t.Errorf("folded %v, want [s]", got)
	}
	if got := marksFor(t, j, "p1", "node:other"); len(got) != 0 {
		t.Errorf("another node's child was folded: %v", got)
	}
	// A run that is not under taint folds nothing, and a missing child run has no marks.
	plain := memJournal()
	startTainted(t, plain, "c9", d)
	recordNode(t, plain, "c9", "node:a")
	if err := putTaintChild(ctx, plain, "p9", "node:call", "c9", childKindFlow); err != nil {
		t.Fatal(err)
	}
	if err := foldChildMarks(ctx, plain, "p9", "node:call"); err != nil || len(marksFor(t, plain, "p9", "node:call")) != 0 {
		t.Errorf("a run not under taint: %v, %v", err, marksFor(t, plain, "p9", "node:call"))
	}
	// A child that is not under taint holds no mark of its own, whatever its journal holds.
	startTainted(t, j, "p4", flowTaintDecls{Policy: flowTaintPolicy{Refuse: true}})
	if _, err := putTaintMark(ctx, j, "c4", "node:a", "planted"); err != nil {
		t.Fatal(err)
	}
	if err := putTaintChild(ctx, j, "p4", "node:call", "c4", childKindFlow); err != nil {
		t.Fatal(err)
	}
	if err := foldChildMarks(ctx, j, "p4", "node:call"); err != nil || len(marksFor(t, j, "p4", "node:call")) != 0 {
		t.Errorf("a child not under taint: %v, %v", err, marksFor(t, j, "p4", "node:call"))
	}
	startTainted(t, j, "p2", flowTaintDecls{Policy: flowTaintPolicy{Refuse: true}})
	if err := putTaintChild(ctx, j, "p2", "node:call", "never-started", childKindFlow); err != nil {
		t.Fatal(err)
	}
	if err := foldChildMarks(ctx, j, "p2", "node:call"); err != nil || len(marksFor(t, j, "p2", "node:call")) != 0 {
		t.Errorf("a child that never started: %v", err)
	}
	// A link that does not read is a fault.
	startTainted(t, j, "p3", flowTaintDecls{Policy: flowTaintPolicy{Refuse: true}})
	if _, _, err := j.putNew(ctx, "p3", taintChildPrefix+"junk", Record{Kind: StepValue, Result: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := foldChildMarks(ctx, j, "p3", "node:call"); !errors.Is(err, ErrStorage) {
		t.Errorf("an unreadable link: %v, want ErrStorage", err)
	}
	// A chain deeper than the bound is refused, not followed without end.
	startTainted(t, j, "d0", flowTaintDecls{Policy: flowTaintPolicy{Refuse: true}})
	prev := "d0"
	for i := 1; i <= maxChildDepth+2; i++ {
		id := "d" + strings.Repeat("x", i)
		startTainted(t, j, id, d)
		node := "node:a"
		if err := putTaintChild(ctx, j, prev, node, id, childKindFlow); err != nil {
			t.Fatal(err)
		}
		prev = id
	}
	if err := foldChildMarks(ctx, j, "d0", "node:a"); !errors.Is(err, ErrConfig) {
		t.Errorf("a chain deeper than %d: %v, want ErrConfig", maxChildDepth, err)
	}
}

func TestFlowEndedTainted_NeedsARunUnderTaint(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	d := flowTaintDecls{Policy: flowTaintPolicy{Refuse: true}, Nodes: map[string]flowNodeTaint{"a": {Untrusted: []string{"s"}}}}
	startTainted(t, j, "c1", d)
	if got, err := flowEndedTainted(ctx, j, "c1"); err != nil || got {
		t.Errorf("no result yet: %v, %v", got, err)
	}
	recordNode(t, j, "c1", "node:a")
	if got, err := flowEndedTainted(ctx, j, "c1"); err != nil || !got {
		t.Errorf("a source recorded: %v, %v", got, err)
	}
	// A run that is not under taint holds no mark, whatever its journal holds.
	if _, err := putTaintMark(ctx, j, "c2", "node:a", "planted"); err != nil {
		t.Fatal(err)
	}
	if got, err := flowEndedTainted(ctx, j, "c2"); err != nil || got {
		t.Errorf("a run not under taint: %v, %v", got, err)
	}
}

// nodeUntrusted is the sources each node of the declarations declares, as ownMarkSources takes them.
func (d flowTaintDecls) nodeUntrusted() map[string][]string {
	out := map[string][]string{}
	for name, n := range d.normalized().Nodes {
		if len(n.Untrusted) > 0 {
			out[name] = n.Untrusted
		}
	}
	return out
}

// A flow with no policy that declares sources records them in run:start, and only then: a flow that declares none
// starts byte for byte as it did.
func TestFlowTaintPrepare_APolicylessFlowRecordsItsDeclaredSources(t *testing.T) {
	src := map[string][]string{"fetch": {"crm", "inbox"}}
	j := memJournal()
	got, err := flowTaintPrepare(context.Background(), j, "r1", flowStart(), nil, nil, src)
	if err != nil {
		t.Fatal(err)
	}
	ds, err := got.(RunStart).declaredSources()
	if err != nil || !sameSources(ds, src) {
		t.Fatalf("recorded %v, %v; want %v", ds, err, src)
	}
	if _, has := got.(RunStart).Ext[extTaintDigest]; has {
		t.Error("a flow with no policy that is not under taint recorded a taint status")
	}
	for _, none := range []map[string][]string{nil, {}} {
		got, err = flowTaintPrepare(context.Background(), j, "r2", flowStart(), nil, nil, none)
		if err != nil || len(got.(RunStart).Ext) != 0 {
			t.Errorf("no sources: %v, ext %v; want the start unchanged", err, got.(RunStart).Ext)
		}
	}
	// Under a tainted parent it records the parent's status and its sources.
	got, err = flowTaintPrepare(taintedCtx(taintMark{Source: "a", Ref: "node:x"}), j, "r3", flowStart(), nil, nil, src)
	if err != nil {
		t.Fatal(err)
	}
	if ds, _ := got.(RunStart).declaredSources(); !sameSources(ds, src) || got.(RunStart).taintDigest() == "" {
		t.Errorf("under a tainted parent: sources %v, digest %q", ds, got.(RunStart).taintDigest())
	}
	// A flow with a policy does not record them apart from its declarations.
	got, err = flowTaintPrepare(context.Background(), j, "r4", flowStart(), hookDecls(nil), nil, src)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := got.(RunStart).Ext[extTaintSources]; has {
		t.Error("a flow with a policy recorded taint_sources")
	}
}

func TestFlowTaintEnter_HoldsAPolicylessFlowToItsSources(t *testing.T) {
	ctx := context.Background()
	src := map[string][]string{"fetch": {"inbox"}}
	start := func(s map[string][]string) *Journal {
		j := memJournal()
		got, err := flowTaintPrepare(ctx, j, "r1", flowStart(), nil, nil, s)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := beginRun(ctx, j, "r1", got.(RunStart)); err != nil {
			t.Fatal(err)
		}
		return j
	}
	j := start(src)
	if _, err := flowTaintEnter(ctx, j, "r1", nil, nil, src); err != nil {
		t.Errorf("the same sources: %v", err)
	}
	for what, other := range map[string]map[string][]string{
		"other sources": {"fetch": {"web"}}, "more sources": {"fetch": {"inbox", "web"}}, "another node": {"look": {"inbox"}}, "none": nil,
	} {
		if _, err := flowTaintEnter(ctx, j, "r1", nil, nil, other); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: %v, want ErrConfig", what, err)
		}
	}
	// A run that started with no sources (an earlier release's, say), driven by a flow that declares some now.
	if _, err := flowTaintEnter(ctx, start(nil), "r1", nil, nil, src); !errors.Is(err, ErrConfig) {
		t.Errorf("a start with none, a flow that declares some: %v, want ErrConfig", err)
	}
	if _, err := flowTaintEnter(ctx, start(nil), "r1", nil, nil, nil); err != nil {
		t.Errorf("a start with none, a flow that declares none: %v", err)
	}
	// A flow with a policy now is not retro-gated.
	if st, err := flowTaintEnter(ctx, j, "r1", hookDecls(nil), nil, nil); err != nil || st != journalhook.FlowTaintNone {
		t.Errorf("a policy added after the start: %d, %v", st, err)
	}
}

func TestDeclaredSources_AreReadStrictly(t *testing.T) {
	for what, raw := range map[string]string{
		"not a map":     `[1]`,
		"empty":         `{}`,
		"an empty list": `{"a":[]}`,
		"unsorted":      `{"a":["z","b"]}`,
		"repeated":      `{"a":["b","b"]}`,
		"a bad name":    `{"a":["x\u0000y"]}`,
		"an empty node": `{"":["b"]}`,
		"a field":       `{"a":["b"],"c":1}`,
	} {
		s := RunStart{Ext: map[string]json.RawMessage{extTaintSources: json.RawMessage(raw)}}
		if _, err := s.declaredSources(); !errors.Is(err, ErrStorage) {
			t.Errorf("%s: %v, want ErrStorage", what, err)
		}
	}
	s := RunStart{Ext: map[string]json.RawMessage{extTaintSources: json.RawMessage(`{"a":["b","c"]}`)}}
	if got, err := s.declaredSources(); err != nil || !sameSources(got, map[string][]string{"a": {"b", "c"}}) {
		t.Errorf("a good member: %v, %v", got, err)
	}
}

func TestFlowEndedTainted_ForAFlowThatDeclaresSourcesAndHasNoPolicy(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	got, _ := flowTaintPrepare(ctx, j, "c1", flowStart(), nil, nil, map[string][]string{"a": {"s"}})
	if _, _, _, err := beginRun(ctx, j, "c1", got.(RunStart)); err != nil {
		t.Fatal(err)
	}
	if t1, err := flowEndedTainted(ctx, j, "c1"); err != nil || t1 {
		t.Errorf("before its source node: %v, %v", t1, err)
	}
	recordNode(t, j, "c1", "node:a")
	if t1, err := flowEndedTainted(ctx, j, "c1"); err != nil || !t1 {
		t.Errorf("after its source node: %v, %v", t1, err)
	}
}

func TestFlowTaintEnter_AnUnreadableSourcesMemberIsAStorageFault(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	bad := flowStart()
	bad.Ext = map[string]json.RawMessage{extTaintSources: json.RawMessage(`{"a":["z","b"]}`)}
	if _, _, _, err := beginRun(ctx, j, "r1", bad); err != nil {
		t.Fatal(err)
	}
	if _, err := flowTaintEnter(ctx, j, "r1", nil, nil, map[string][]string{"a": {"b", "z"}}); !errors.Is(err, ErrStorage) {
		t.Errorf("an unreadable member: %v, want ErrStorage", err)
	}
}

// A run that began (it has a start) outside taint, declaring no source, has nothing to send, whatever its journal holds.
func TestFlowTaintSink_ABegunRunNotUnderTaintAndDeclaringNothingSendsNothing(t *testing.T) {
	ctx := context.Background()
	parent, plain := memJournal(), memJournal()
	if _, _, _, err := beginRun(ctx, plain, "c2", flowStart()); err != nil {
		t.Fatal(err)
	}
	recordNode(t, plain, "c2", "node:a")
	if _, err := putTaintMark(ctx, plain, "c2", "node:a", "planted"); err != nil {
		t.Fatal(err)
	}
	if err := flowTaintSink(withMarkSink(ctx, parent, "p2", "node:call"), plain, "c2", "node:a"); err != nil || len(marksFor(t, parent, "p2", "node:call")) != 0 {
		t.Errorf("a begun run not under taint: %v, %v", err, marksFor(t, parent, "p2", "node:call"))
	}
}

// A child that began outside taint but declares sources is counted by the fold, as the sink sends it.
func TestFoldChildMarks_CountsAChildThatDeclaresSourcesOutsideTaint(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	startTainted(t, j, "p1", flowTaintDecls{Policy: flowTaintPolicy{Refuse: true}})
	got, _ := flowTaintPrepare(ctx, j, "c1", flowStart(), nil, nil, map[string][]string{"a": {"feed"}})
	if _, _, _, err := beginRun(ctx, j, "c1", got.(RunStart)); err != nil {
		t.Fatal(err)
	}
	recordNode(t, j, "c1", "node:a")
	if err := putTaintChild(ctx, j, "p1", "node:call", "c1", childKindFlow); err != nil {
		t.Fatal(err)
	}
	if err := foldChildMarks(ctx, j, "p1", "node:call"); err != nil {
		t.Fatal(err)
	}
	if got := marksFor(t, j, "p1", "node:call"); strings.Join(got, ",") != "feed" {
		t.Errorf("folded %v, want [feed]", got)
	}
}

func TestFlowEndedTainted_ARunThatInheritedMarksIsTainted(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	got, err := flowTaintPrepare(taintedCtx(taintMark{Source: "inbox", Ref: "node:x"}), j, "c1", flowStart(), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := beginRun(ctx, j, "c1", got.(RunStart)); err != nil {
		t.Fatal(err)
	}
	if tainted, err := flowEndedTainted(ctx, j, "c1"); err != nil || !tainted {
		t.Errorf("a run that inherited a mark and read nothing: %v, %v; want it tainted", tainted, err)
	}
}

// A declared-sources member that does not read in a start Conform is given is a divergence.
func TestFlowTaintConform_AnUnreadableSourcesMemberIsADivergence(t *testing.T) {
	start := flowStart()
	start.Ext = map[string]json.RawMessage{extTaintSources: json.RawMessage(`{"a":["z","b"]}`)}
	b, _ := json.Marshal(start)
	recs := []Record{{Name: runStartStep, Kind: StepValue, Result: b}}
	diffs := flowTaintConform(recs, nil, nil, map[string]any{})
	if !strings.Contains(strings.Join(diffs, "|"), "declared sources do not read") {
		t.Errorf("diffs %q, want the unreadable sources named", diffs)
	}
}
