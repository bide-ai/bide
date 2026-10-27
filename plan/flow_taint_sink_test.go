package plan

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// Taint coming up (docs/design/flow-taint.md, decision 10): the marks a flow run learned reach the node that
// started it, as taint-mark records in the parent's journal, and the nodes after it are read under them.

// sinkStore fails the Insert of the first journal name that matches, as a crash at that point would leave the
// journal; arm it with fail.
type sinkStore struct {
	agent.Store
	mu     sync.Mutex
	match  func(name string) bool
	failed bool
}

func (s *sinkStore) fail(match func(string) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.match, s.failed = match, false
}

func (s *sinkStore) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	s.mu.Lock()
	hit := s.match != nil && !s.failed && s.match(name)
	if hit {
		s.failed = true
	}
	s.mu.Unlock()
	if hit {
		return agent.Entry{}, false, errors.New("crash")
	}
	return s.Store.Insert(ctx, runID, name, data)
}

// marksOf is the sources of the taint-mark records of run for the node with key nodeKey, sorted.
func marksOf(t *testing.T, j *agent.Journal, run, nodeKey string) []string {
	t.Helper()
	recs, err := j.History(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range recs {
		if !strings.HasPrefix(r.Name, "taint-mark:") {
			continue
		}
		var m struct {
			NodeKey string `json:"node_key"`
			Source  string `json:"source"`
		}
		if err := json.Unmarshal(r.Result, &m); err != nil {
			t.Fatalf("%s: %v", r.Name, err)
		}
		if m.NodeKey == nodeKey {
			out = append(out, m.Source)
		}
	}
	slices.Sort(out)
	return out
}

func childrenOf(t *testing.T, j *agent.Journal, run string) []string {
	t.Helper()
	recs, _ := j.History(context.Background(), run)
	var out []string
	for _, r := range recs {
		if strings.HasPrefix(r.Name, "taint-child:") {
			var c struct {
				NodeKey string `json:"node_key"`
				RunID   string `json:"run_id"`
			}
			if err := json.Unmarshal(r.Result, &c); err != nil {
				t.Fatal(err)
			}
			out = append(out, c.NodeKey+" -> "+c.RunID)
		}
	}
	slices.Sort(out)
	return out
}

// sinkParent is the flow call -> pay: call is a node whose body runs the flows run starts (it is read-only unless
// callSide is set), pay has a side effect. The parent declares no source of its own, so only what its children
// bring up can taint pay.
type sinkParent struct {
	t     *testing.T
	pays  int
	calls int
}

func (p *sinkParent) build(pol *agent.TaintPolicy, callSide bool, run func(ctx context.Context) error) *Flow[int, int] {
	p.t.Helper()
	b := New[int, int]("parent").WithTaintPolicy(*pol)
	var callOpts []NodeOption
	if !callSide {
		callOpts = append(callOpts, ReadOnly())
	}
	call := b.Step("call", func(ctx context.Context, v int) (int, error) {
		p.calls++
		if err := run(ctx); err != nil {
			return 0, err
		}
		return v, nil
	}, callOpts...)
	pay := b.Step("pay", func(_ context.Context, v int) (int, error) { p.pays++; return v, nil })
	b.Edge(call, pay)
	f, err := b.Build()
	if err != nil {
		p.t.Fatal(err)
	}
	return f
}

// source is a child flow with one read-only node, read, that declares the source src (or none, for ""), under the
// policy pol.
func sourceFlow(t *testing.T, name, src string, pol *agent.TaintPolicy, extra ...NodeOption) *Flow[int, int] {
	t.Helper()
	b := New[int, int](name)
	if pol != nil {
		b.WithTaintPolicy(*pol)
	}
	opts := append([]NodeOption{ReadOnly()}, extra...)
	if src != "" {
		opts = append(opts, Untrusted(src))
	}
	b.Step("read", func(_ context.Context, v int) (int, error) { return v, nil }, opts...)
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// A mark the child learned is a mark of the parent's node: the node after it needs its escalation gate, and
// reads the child as the source of the mark. A child that learned nothing brings nothing.
func TestFlowTaintSink_AChildsMarkGatesTheParentsNextNode(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	pp := &sinkParent{t: t}
	c := sourceFlow(t, "child", "feed", single())
	p := pp.build(single(), false, func(ctx context.Context) error {
		_, err := c.Run(ctx, j, "c1", 1)
		return err
	})
	_, err := p.Run(ctx, j, "p1", 1)
	if pend := pending(t, err); pend.ToolUseID != "node:pay:taint" {
		t.Fatalf("paused at %q, want node:pay:taint", pend.ToolUseID)
	}
	if pp.pays != 0 {
		t.Fatalf("pay ran %d times before its gate passed", pp.pays)
	}
	if got := marksOf(t, j, "p1", "node:call"); !slices.Equal(got, []string{"feed"}) {
		t.Errorf("the parent holds the marks %v for node:call, want [feed]", got)
	}
	if got := childrenOf(t, j, "p1"); !slices.Equal(got, []string{"node:call -> c1"}) {
		t.Errorf("the parent's children %v, want node:call -> c1", got)
	}
	// The reason names the child as where the mark was read.
	recs, _ := j.History(ctx, "p1")
	found := false
	for _, r := range recs {
		if r.Name == "gate-reason:node%3Apay" && strings.Contains(string(r.Result), `"source":"feed"`) && strings.Contains(string(r.Result), "child:node:call") {
			found = true
		}
	}
	if !found {
		t.Error("the gate-reason record of pay does not name feed read from the child of node:call")
	}
	if err := agent.Approve(ctx, j, "p1", "node:pay:taint", true); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Run(ctx, j, "p1", 1); err != nil || pp.pays != 1 {
		t.Fatalf("after approval: %v, pay ran %d times", err, pp.pays)
	}
	if ok, diffs, err := p.Conform(ctx, j, "p1"); err != nil || !ok {
		t.Errorf("the parent's journal does not conform: %v, %v", diffs, err)
	}
	if ok, diffs, err := c.Conform(ctx, j, "c1"); err != nil || !ok {
		t.Errorf("the child's journal does not conform: %v, %v", diffs, err)
	}

	// A child that read nothing untrusted sends nothing: pay runs ungated.
	j2 := agenttest.MemJournal()
	pp2 := &sinkParent{t: t}
	quiet := sourceFlow(t, "child", "", single())
	p2 := pp2.build(single(), false, func(ctx context.Context) error {
		_, err := quiet.Run(ctx, j2, "c1", 1)
		return err
	})
	if _, err := p2.Run(ctx, j2, "p2", 1); err != nil || pp2.pays != 1 {
		t.Fatalf("a child with no mark: %v, pay ran %d times; want it to run ungated", err, pp2.pays)
	}
	if got := marksOf(t, j2, "p2", "node:call"); len(got) != 0 {
		t.Errorf("a child with no mark sent %v", got)
	}
}

// A child with no policy sends up too: a flow run under taint with no policy of its own runs only nodes that
// cannot have a side effect, and what a node of it learns (here, from its own child) is its own mark.
func TestFlowTaintSink_APolicylessChildSendsUpWhatItsOwnChildBroughtUp(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	pp := &sinkParent{t: t}
	grand := sourceFlow(t, "grand", "deep", single())
	mb := New[int, int]("middle") // no policy
	mb.Step("mid", func(c context.Context, v int) (int, error) {
		if _, err := grand.Run(c, j, "g1", v); err != nil {
			t.Errorf("grandchild: %v", err)
		}
		return v, nil
	}, ReadOnly())
	middle, err := mb.Build()
	if err != nil {
		t.Fatal(err)
	}
	p := pp.build(single(), false, func(c context.Context) error {
		_, err := middle.Run(c, j, "m1", 1)
		return err
	})
	_, err = p.Run(ctx, j, "p1", 1)
	if pend := pending(t, err); pend.ToolUseID != "node:pay:taint" {
		t.Fatalf("paused at %q, want node:pay:taint", pend.ToolUseID)
	}
	// The grandchild's mark is the child's own, in the child's journal, and from there the root's.
	if got := marksOf(t, j, "m1", "node:mid"); !slices.Equal(got, []string{"deep"}) {
		t.Errorf("the child holds %v for node:mid, want [deep]", got)
	}
	if got := marksOf(t, j, "p1", "node:call"); !slices.Equal(got, []string{"deep"}) {
		t.Errorf("the root holds %v for node:call, want [deep]", got)
	}
	if ok, diffs, err := middle.Conform(ctx, j, "m1"); err != nil || !ok {
		t.Errorf("the policyless child's journal does not conform: %v, %v", diffs, err)
	}
}

// The child sends its own marks and no others: what it inherited is already the parent's, and echoing it up a
// chain would make a node after the child read the parent's own mark as one the child found.
func TestFlowTaintSink_TheChildSendsItsOwnMarksNotTheOnesItInherited(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	b := New[int, int]("parent").WithTaintPolicy(*single())
	fetch := b.Step("fetch", func(_ context.Context, v int) (int, error) { return v, nil }, Untrusted("inbox"))
	c := sourceFlow(t, "child", "feed", single())
	call := b.Step("call", func(ctx context.Context, v int) (int, error) {
		if _, err := c.Run(ctx, j, "c1", v); err != nil {
			t.Errorf("child: %v", err)
		}
		return v, nil
	}, ReadOnly())
	b.Edge(fetch, call)
	p, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Run(ctx, j, "p1", 1); err != nil {
		t.Fatal(err)
	}
	if inh := inherited(t, startOf(t, j, "c1")); len(inh) != 1 || inh[0]["source"] != "inbox" {
		t.Fatalf("the child inherited %v, want inbox", inh)
	}
	if got := marksOf(t, j, "p1", "node:call"); !slices.Equal(got, []string{"feed"}) {
		t.Errorf("the parent holds %v for node:call, want [feed] and not the inherited inbox", got)
	}
}

// A crash between the child's record of a node and the sink leaves the mark unsent; the next drive of the parent
// runs the child again, which replays the node and sends it. The node after the child is gated all the same, and
// the child's body does not run twice.
func TestFlowTaintSink_ACrashBetweenTheChildsRecordAndTheSinkLosesNothing(t *testing.T) {
	ctx := context.Background()
	ss := &sinkStore{Store: agent.NewMemStore()}
	j := agenttest.MustJournal(ss)
	reads := 0
	cb := New[int, int]("child").WithTaintPolicy(*single())
	cb.Step("read", func(_ context.Context, v int) (int, error) { reads++; return v, nil }, ReadOnly(), Untrusted("feed"))
	c, err := cb.Build()
	if err != nil {
		t.Fatal(err)
	}
	pp := &sinkParent{t: t}
	p := pp.build(single(), false, func(ctx context.Context) error {
		_, err := c.Run(ctx, j, "c1", 1)
		return err
	})
	run := func() error {
		_, err := p.Run(ctx, j, "p1", 1)
		return err
	}
	ss.fail(func(name string) bool { return strings.HasPrefix(name, "taint-mark:") })
	if err := run(); err == nil || !strings.Contains(err.Error(), "crash") {
		t.Fatalf("the first drive: %v, want the crash of the sink", err)
	}
	if got := marksOf(t, j, "p1", "node:call"); len(got) != 0 {
		t.Fatalf("the mark was written despite the crash: %v", got)
	}
	if recs, _ := j.History(ctx, "c1"); !hasName(recs, "node:read") {
		t.Fatal("the child's node was not recorded before the crash")
	}
	// The next drive: the child replays, the sink sends, pay waits at its gate.
	if pend := pending(t, run()); pend.ToolUseID != "node:pay:taint" {
		t.Fatalf("paused at %q, want node:pay:taint", pend.ToolUseID)
	}
	if reads != 1 || pp.pays != 0 {
		t.Errorf("the child's read ran %d times and pay %d; want 1 and 0", reads, pp.pays)
	}
	if got := marksOf(t, j, "p1", "node:call"); !slices.Equal(got, []string{"feed"}) {
		t.Errorf("after the replay the parent holds %v, want [feed]", got)
	}
}

func hasName(recs []agent.Record, name string) bool {
	return slices.ContainsFunc(recs, func(r agent.Record) bool { return r.Name == name })
}

// A child that finished without its parent (a recovery pass drove it to the end after its node was resolved)
// still sends everything it learned when the parent drives it: a run that has finished is not replayed node by
// node.
func TestFlowTaintSink_AChildFinishedWithoutItsParentStillSends(t *testing.T) {
	ctx := context.Background()
	ss := &sinkStore{Store: agent.NewMemStore()}
	j := agenttest.MustJournal(ss)
	// The child's source is a node with a side effect: a crash after its body leaves a halt, which is resolved.
	cb := New[int, int]("child").WithTaintPolicy(*single())
	cb.Step("read", func(_ context.Context, v int) (int, error) { return v, nil }, Untrusted("feed"))
	c, err := cb.Build()
	if err != nil {
		t.Fatal(err)
	}
	pp := &sinkParent{t: t}
	p := pp.build(single(), false, func(ctx context.Context) error {
		_, err := c.Run(ctx, j, "c1", 1)
		return err
	})
	run := func() error {
		_, err := p.Run(ctx, j, "p1", 1)
		return err
	}
	ss.fail(func(name string) bool { return name == "node:read" })
	if err := run(); err == nil {
		t.Fatal("the first drive did not fail")
	}
	// The child halts on the unknown outcome of read; its operator resolves it, and a recovery pass (no parent)
	// finishes the run.
	_, err = c.Run(ctx, j, "c1", 1)
	halt, ok := errors.AsType[*agent.OutcomeUnknown](err)
	if !ok {
		t.Fatalf("the child driven again: %v, want a halt", err)
	}
	if err := c.ResolveHalt(ctx, j, halt.Ref(), agent.Outcome{Result: 7}, agent.WithoutLiveDriverCheck()); err != nil {
		t.Fatal(err)
	}
	if out, err := c.Run(ctx, j, "c1", 1); err != nil || out != 7 {
		t.Fatalf("the child finished by itself: %d, %v", out, err)
	}
	if got := marksOf(t, j, "p1", "node:call"); len(got) != 0 {
		t.Fatalf("the parent holds %v before it drives the child again", got)
	}
	// The parent drives the finished child.
	if pend := pending(t, run()); pend.ToolUseID != "node:pay:taint" {
		t.Fatalf("paused at %q, want node:pay:taint", pend.ToolUseID)
	}
	if got := marksOf(t, j, "p1", "node:call"); !slices.Equal(got, []string{"feed"}) {
		t.Errorf("the parent holds %v, want [feed]", got)
	}
}

// A ResolveHalt of the parent's node folds the marks of the runs it started, which the node never sent: its
// result is recorded by the operator, and the node after it is gated all the same.
func TestFlowTaintSink_AResolveHaltOfTheParentNodeFoldsTheChildsMarks(t *testing.T) {
	ctx := context.Background()
	ss := &sinkStore{Store: agent.NewMemStore()}
	j := agenttest.MustJournal(ss)
	c := sourceFlow(t, "child", "feed", single())
	pp := &sinkParent{t: t}
	p := pp.build(single(), true, func(ctx context.Context) error {
		_, err := c.Run(ctx, j, "c1", 1)
		return err
	})
	run := func() error {
		_, err := p.Run(ctx, j, "p1", 1)
		return err
	}
	// The child records its node and the sink crashes: the parent's node has a live attempt and no result.
	ss.fail(func(name string) bool { return strings.HasPrefix(name, "taint-mark:") })
	if err := run(); err == nil {
		t.Fatal("the first drive did not fail")
	}
	halt, ok := errors.AsType[*agent.OutcomeUnknown](run())
	if !ok {
		t.Fatal("the second drive did not halt on the node whose outcome is unknown")
	}
	if got := marksOf(t, j, "p1", "node:call"); len(got) != 0 {
		t.Fatalf("the parent holds %v before the resolution", got)
	}
	if err := p.ResolveHalt(ctx, j, halt.Ref(), agent.Outcome{Result: 5}, agent.WithoutLiveDriverCheck()); err != nil {
		t.Fatal(err)
	}
	if got := marksOf(t, j, "p1", "node:call"); !slices.Equal(got, []string{"feed"}) {
		t.Fatalf("after the resolution the parent holds %v for node:call, want [feed]", got)
	}
	if pend := pending(t, run()); pend.ToolUseID != "node:pay:taint" {
		t.Fatalf("paused at %q, want node:pay:taint", pend.ToolUseID)
	}
	if pp.pays != 0 {
		t.Errorf("pay ran %d times ungated", pp.pays)
	}
}

// The fold follows the children of children: the grandchild's mark, which neither level sent, is the root's.
func TestFlowTaintSink_AResolveHaltFoldsTheMarksOfGrandchildren(t *testing.T) {
	ctx := context.Background()
	ss := &sinkStore{Store: agent.NewMemStore()}
	j := agenttest.MustJournal(ss)
	grand := sourceFlow(t, "grand", "deep", single())
	mb := New[int, int]("middle").WithTaintPolicy(*single())
	mb.Step("mid", func(c context.Context, v int) (int, error) {
		if _, err := grand.Run(c, j, "g1", v); err != nil {
			return 0, err
		}
		return v, nil
	}, ReadOnly())
	middle, err := mb.Build()
	if err != nil {
		t.Fatal(err)
	}
	pp := &sinkParent{t: t}
	p := pp.build(single(), true, func(c context.Context) error {
		_, err := middle.Run(c, j, "m1", 1)
		return err
	})
	run := func() error {
		_, err := p.Run(ctx, j, "p1", 1)
		return err
	}
	// Every sink write fails from now on: the grandchild's read is recorded, and nothing went up.
	ss.fail(func(name string) bool { return strings.HasPrefix(name, "taint-mark:") })
	if err := run(); err == nil {
		t.Fatal("the first drive did not fail")
	}
	halt, ok := errors.AsType[*agent.OutcomeUnknown](run())
	if !ok {
		t.Fatalf("the second drive did not halt")
	}
	if err := p.ResolveHalt(ctx, j, halt.Ref(), agent.Outcome{Result: 5}, agent.WithoutLiveDriverCheck()); err != nil {
		t.Fatal(err)
	}
	if got := marksOf(t, j, "p1", "node:call"); !slices.Equal(got, []string{"deep"}) {
		t.Errorf("the root holds %v for node:call after the resolution, want [deep]", got)
	}
}

// A run that is not under taint writes no mark, and a ResolveHalt in it folds nothing.
func TestFlowTaintSink_ARunNotUnderTaintHasNoSink(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	c := sourceFlow(t, "child", "feed", single())
	b := New[int, int]("parent") // no policy: the run is not under taint, so its node binds no sink
	b.Step("call", func(ctx context.Context, v int) (int, error) {
		if _, err := c.Run(ctx, j, "c1", v); err != nil {
			t.Errorf("child: %v", err)
		}
		return v, nil
	}, ReadOnly())
	p, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Run(ctx, j, "p1", 1); err != nil {
		t.Fatal(err)
	}
	recs, _ := j.History(ctx, "p1")
	for _, r := range recs {
		if strings.HasPrefix(r.Name, "taint-") {
			t.Errorf("a run that is not under taint holds %q", r.Name)
		}
	}
}

// A flow started under taint has to use its parent's journal: the marks go up as records of it, and a
// ResolveHalt finds the child in it. Another journal is refused before the child writes anything.
func TestFlowTaintSink_AChildInAnotherJournalIsRefused(t *testing.T) {
	ctx := context.Background()
	j, other := agenttest.MemJournal(), agenttest.MemJournal()
	c := sourceFlow(t, "child", "feed", single())
	var childErr error
	pp := &sinkParent{t: t}
	p := pp.build(single(), false, func(ctx context.Context) error {
		_, childErr = c.Run(ctx, other, "c1", 1)
		return nil
	})
	if _, err := p.Run(ctx, j, "p1", 1); err != nil {
		t.Fatalf("parent: %v", err)
	}
	if !errors.Is(childErr, agent.ErrConfig) || !strings.Contains(childErr.Error(), "journal") {
		t.Errorf("the child in another journal: %v, want ErrConfig naming the journal", childErr)
	}
	if recs, _ := other.History(ctx, "c1"); len(recs) != 0 {
		t.Errorf("the refused child wrote %d records", len(recs))
	}
}

// The fold of a node holds the marks of the runs that node started, and no other node's: a node's marks are
// read by the nodes after it, and the node itself does not read them.
func TestFlowTaintSink_AResolveHaltFoldsOnlyTheChildrenOfItsNode(t *testing.T) {
	ctx := context.Background()
	ss := &sinkStore{Store: agent.NewMemStore()}
	j := agenttest.MustJournal(ss)
	ca := sourceFlow(t, "childa", "a", single())
	cb := sourceFlow(t, "childb", "b", single())
	b := New[int, int]("parent").WithTaintPolicy(*single())
	call1 := b.Step("call1", func(ctx context.Context, v int) (int, error) {
		_, err := ca.Run(ctx, j, "c1", v)
		return v, err
	}, ReadOnly())
	call2 := b.Step("call2", func(ctx context.Context, v int) (int, error) {
		_, err := cb.Run(ctx, j, "c2", v)
		return v, err
	})
	b.Edge(call1, call2)
	p, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	// call2 comes after call1's child, so it waits at its gate; once approved, its child's mark is lost to a crash.
	_, err = p.Run(ctx, j, "p1", 1)
	pending(t, err)
	if err := agent.Approve(ctx, j, "p1", "node:call2:taint", true); err != nil {
		t.Fatal(err)
	}
	ss.fail(func(name string) bool { return name == "taint-mark:node%3Acall2:b" })
	if _, err := p.Run(ctx, j, "p1", 1); err == nil {
		t.Fatal("the drive after the approval did not fail")
	}
	_, err = p.Run(ctx, j, "p1", 1)
	halt, ok := errors.AsType[*agent.OutcomeUnknown](err)
	if !ok {
		t.Fatalf("the second drive: %v, want a halt", err)
	}
	if err := p.ResolveHalt(ctx, j, halt.Ref(), agent.Outcome{Result: 5}, agent.WithoutLiveDriverCheck()); err != nil {
		t.Fatal(err)
	}
	if got := marksOf(t, j, "p1", "node:call2"); !slices.Equal(got, []string{"b"}) {
		t.Errorf("node:call2 holds %v, want [b] and not the marks of call1's child", got)
	}
	if got := marksOf(t, j, "p1", "node:call1"); !slices.Equal(got, []string{"a"}) {
		t.Errorf("node:call1 holds %v, want [a]", got)
	}
}

// plant returns a copy of run's journal with one more record, as a forgery would leave it.
func plant(t *testing.T, j *agent.Journal, run, name, body string) *agent.Journal {
	t.Helper()
	nj := copyWithout(t, j, run, func(string) bool { return false })
	data, err := agent.JournalEntry(name, agent.Record{Kind: agent.StepValue, Result: []byte(body)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := nj.Store().Insert(context.Background(), run, name, data); err != nil {
		t.Fatal(err)
	}
	return nj
}

// Conform reads the records the sink writes: a taint-child or taint-mark record that does not read, or that names
// a node the flow does not declare, is a divergence; and a run started inside a tainted run with no policy has
// the marks its children brought up (above), and no gate-reason records.
func TestFlowTaintSink_ConformChecksTheLinkRecords(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	pp := &sinkParent{t: t}
	c := sourceFlow(t, "child", "feed", single())
	p := pp.build(single(), false, func(ctx context.Context) error {
		_, err := c.Run(ctx, j, "c1", 1)
		return err
	})
	if _, err := p.Run(ctx, j, "p1", 1); err == nil {
		t.Fatal("the run did not wait at pay's gate")
	}
	for what, f := range map[string]struct{ name, body, want string }{
		"a link that does not read":  {"taint-child:node%3Acall:c9", `1`, "taint-child record that does not read"},
		"a link of an undeclared node": {"taint-child:node%3Aghost:c9", `{"format":"bide.taint-child.v1","node_key":"node:ghost","run_id":"c9"}`, "taint-child record of an undeclared node"},
		"a mark of an undeclared node": {"taint-mark:node%3Aghost:s", `{"format":"bide.taint-mark.v1","node_key":"node:ghost","source":"s"}`, "taint-mark record of an undeclared node"},
	} {
		ok, diffs, err := p.Conform(ctx, plant(t, j, "p1", f.name, f.body), "p1")
		if err != nil || ok || !strings.Contains(strings.Join(diffs, "|"), f.want) {
			t.Errorf("%s: Conform = %v, %q, %v; want %q", what, ok, diffs, err, f.want)
		}
	}

	// A policyless child of a tainted node: the marks its own child brought up are fine, a gate-reason record is not.
	j2 := agenttest.MemJournal()
	grand := sourceFlow(t, "grand", "deep", single())
	mb := New[int, int]("middle")
	mb.Step("mid", func(c context.Context, v int) (int, error) {
		_, err := grand.Run(c, j2, "g1", v)
		return v, err
	}, ReadOnly())
	middle, err := mb.Build()
	if err != nil {
		t.Fatal(err)
	}
	p2 := (&sinkParent{t: t}).build(single(), false, func(c context.Context) error {
		_, err := middle.Run(c, j2, "m1", 1)
		return err
	})
	if _, err := p2.Run(ctx, j2, "p2", 1); err == nil {
		t.Fatal("the run did not wait at pay's gate")
	}
	if ok, diffs, err := middle.Conform(ctx, j2, "m1"); err != nil || !ok {
		t.Fatalf("a policyless child that holds its child's marks: Conform = %v, %q, %v", ok, diffs, err)
	}
	ok, diffs, err := middle.Conform(ctx, plant(t, j2, "m1", "gate-reason:node%3Amid", `1`), "m1")
	if err != nil || ok || !strings.Contains(strings.Join(diffs, "|"), "gate-reason:node%3Amid (a taint record in a run that did not start under taint with declarations)") {
		t.Errorf("a gate-reason record in a policyless nested run: Conform = %v, %q, %v", ok, diffs, err)
	}
}

// A nested flow with a loop sends what its body nodes learned (in the iteration whose node learned it) to the node
// that started it, as a flow without one does.
func TestFlowTaintSink_ALoopInTheChildSendsWhatItsBodyLearned(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	b := New[int, int]("loopchild").WithTaintPolicy(*single())
	seed := b.Step("seed", func(_ context.Context, v int) (int, error) { return v, nil }, ReadOnly())
	refine := b.Step("refine", func(_ context.Context, v int) (int, error) { return v + 1, nil }, ReadOnly(), Untrusted("feed"))
	check := b.Step("check", func(_ context.Context, v int) (int, error) { return v, nil }, ReadOnly())
	done := b.Step("done", func(_ context.Context, v int) (int, error) { return v, nil }, ReadOnly())
	b.Edge(seed, refine)
	b.Edge(refine, check)
	b.Switch(check, LoopBack(3, func(v int) bool { return v < 2 }, refine), Else(done))
	lc, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	pp := &sinkParent{t: t}
	p := pp.build(single(), false, func(ctx context.Context) error {
		_, err := lc.Run(ctx, j, "lc1", 0)
		return err
	})
	_, err = p.Run(ctx, j, "p1", 1)
	if pend := pending(t, err); pend.ToolUseID != "node:pay:taint" {
		t.Fatalf("paused at %q, want node:pay:taint", pend.ToolUseID)
	}
	if got := marksOf(t, j, "p1", "node:call"); !slices.Equal(got, []string{"feed"}) {
		t.Errorf("the parent holds %v for node:call, want [feed]", got)
	}
}
