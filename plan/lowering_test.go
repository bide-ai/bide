package plan

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/journaltest"
)

// Tests of plan lowering: every node runs as an agent.Step through the engine's step hook, under
// its node key, so the Step's claim protocol, halts, resolution, not-started records and pause
// guard cover flow nodes, and a flow's run records how it started.

// effectFlow is a two-node flow: "charge" fires an effect (it counts its calls) and fails its
// first call after the effect, as a connection lost after the charge was sent would, leaving its
// attempt marker with no result; "done" renders the charged amount.
func effectFlow(t *testing.T, fired *int, opts ...NodeOption) *Flow[int, string] {
	t.Helper()
	b := New[int, string]("charge-flow")
	charge := b.Step("charge", func(_ context.Context, n int) (int, error) {
		*fired++
		if *fired == 1 {
			return 0, errors.New("connection reset after the charge was sent")
		}
		return n, nil
	}, opts...)
	done := b.Step("done", func(_ context.Context, n int) (string, error) { return "charged " + strconv.Itoa(n), nil })
	b.Edge(charge, done)
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	return flow
}

// A node that halted with an unknown outcome is a Step halt: ResolveHaltRef clears it with the
// node's output, and the next Run continues past the node without running its body, feeding the
// resolved output downstream.
func TestLowering_ResolveHaltRefClearsNodeHalt(t *testing.T) {
	ctx := context.Background()
	mem := agenttest.MemJournal()
	var fired int
	flow := effectFlow(t, &fired)
	if _, err := flow.Run(ctx, mem, "r", 5); err == nil {
		t.Fatal("first drive: want the node's error")
	}
	_, err := flow.Run(ctx, mem, "r", 5)
	halt, ok := errors.AsType[*agent.OutcomeUnknown](err)
	if !ok {
		t.Fatalf("second drive: err = %v, want *agent.OutcomeUnknown", err)
	}
	want := agent.OpRef{Kind: agent.OpStep, ID: "node:charge"}
	if halt.Op != want || halt.RunID != "r" || halt.RootRunID != "r" || halt.Cause != agent.HaltCrashed || halt.AttemptedAt.IsZero() {
		t.Fatalf("halt = %+v, want a crashed halt on %+v in run r with its attempt time", halt, want)
	}
	if !agent.IsPause(err) {
		t.Fatalf("a node halt is not a pause: %v", err)
	}
	if fired != 1 {
		t.Fatalf("the halted node ran %d times, want 1", fired)
	}
	if err := agent.ResolveHaltRef(ctx, mem, halt.Ref(), agent.Outcome{Result: 7}); err != nil {
		t.Fatalf("ResolveHaltRef: %v", err)
	}
	out, err := flow.Run(ctx, mem, "r", 5)
	if err != nil || out != "charged 7" {
		t.Fatalf("drive after the resolution: out %q, err %v; want the resolved output downstream", out, err)
	}
	if fired != 1 {
		t.Fatalf("the resolved node ran again: %d calls, want 1", fired)
	}
	if ok, diffs, err := flow.Conform(ctx, mem, "r"); err != nil || !ok {
		t.Fatalf("Conform = %v, %v, %v; want the resolved run to conform", ok, diffs, err)
	}
}

// A node that halts inside a loop body is the Step of its iteration's key, and resolves the same
// way; the loop continues from the resolved iteration.
func TestLowering_ResolveHaltRefClearsLoopIterationHalt(t *testing.T) {
	ctx := context.Background()
	mem := agenttest.MemJournal()
	var calls int
	build := func() *Flow[int, string] {
		b := New[int, string]("countdown")
		seed := b.Step("seed", func(_ context.Context, n int) (loopState, error) { return loopState{N: n, Trace: "seed"}, nil }, ReadOnly())
		refine := b.Step("refine", func(_ context.Context, s loopState) (loopState, error) {
			calls++
			if calls == 2 { // the second iteration's effect is sent, and its result lost
				return loopState{}, errors.New("connection reset")
			}
			return loopState{N: s.N - 1, Trace: s.Trace + "|refine"}, nil
		})
		check := b.Step("check", func(_ context.Context, s loopState) (loopState, error) { return s, nil }, ReadOnly())
		done := b.Step("done", func(_ context.Context, s loopState) (string, error) {
			return fmt.Sprintf("done N=%d trace=%s", s.N, s.Trace), nil
		}, ReadOnly())
		b.Edge(seed, refine)
		b.Edge(refine, check)
		b.Switch(check, LoopBack(10, func(s loopState) bool { return s.N > 0 }, refine).Named("again"), Else(done))
		flow, err := b.Build()
		if err != nil {
			t.Fatal(err)
		}
		return flow
	}
	if _, err := build().Run(ctx, mem, "loop", 3); err == nil {
		t.Fatal("first drive: want refine's error")
	}
	_, err := build().Run(ctx, mem, "loop", 3)
	halt, ok := errors.AsType[*agent.OutcomeUnknown](err)
	if !ok || halt.Op != (agent.OpRef{Kind: agent.OpStep, ID: "node:iter:1:refine"}) {
		t.Fatalf("second drive: err = %v, want a halt on node:iter:1:refine", err)
	}
	if err := agent.ResolveHaltRef(ctx, mem, halt.Ref(), agent.Outcome{Result: loopState{N: 0, Trace: "seed|refine|resolved"}}); err != nil {
		t.Fatalf("ResolveHaltRef: %v", err)
	}
	out, err := build().Run(ctx, mem, "loop", 3)
	if err != nil || out != "done N=0 trace=seed|refine|resolved" {
		t.Fatalf("drive after the resolution: out %q, err %v", out, err)
	}
	if calls != 2 {
		t.Fatalf("refine ran %d times, want 2 (the resolved iteration is not run again)", calls)
	}
}

// ResolveHaltRef accepts a node key as a Step's name, but no other reserved name.
func TestLowering_ResolveHaltRefRefusesOtherReservedNames(t *testing.T) {
	for _, id := range []string{"switch:x", "flow:digest", "node:", "node:a:b", "node:iter:x:a", "attempt:step:x"} {
		ref := agent.HaltRef{RunID: "r", Op: agent.OpRef{Kind: agent.OpStep, ID: id}, Cause: agent.HaltCrashed}
		err := agent.ResolveHaltRef(context.Background(), agenttest.MemJournal(), ref, agent.Outcome{Result: 1})
		if !errors.Is(err, agent.ErrConfig) {
			t.Errorf("ResolveHaltRef on step %q: err = %v, want ErrConfig", id, err)
		}
	}
}

// cancelOnInsert is a Store that cancels a context right after it stores the entry named name: a
// driver cancelled after it claimed a node's attempt and before the node's body ran.
type cancelOnInsert struct {
	agent.Store
	name   string
	once   sync.Once
	cancel context.CancelFunc
}

func (s *cancelOnInsert) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	e, ok, err := s.Store.Insert(ctx, runID, name, data)
	if name == s.name {
		s.once.Do(s.cancel)
	}
	return e, ok, err
}

func (s *cancelOnInsert) Unwrap() agent.Store { return s.Store }

// A driver cancelled after it claimed a node's attempt and before the node's body ran records the
// attempt as not started, so the next drive re-attempts the node under a numbered marker instead
// of halting, and the node's effect fires once.
func TestLowering_NodeCancelledBeforeBodyIsReattempted(t *testing.T) {
	mem := agent.NewMemStore()
	j2 := agenttest.MustJournal(mem)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	j, err := agent.NewJournal(&cancelOnInsert{Store: mem, name: "attempt:step:node:charge", cancel: cancel})
	if err != nil {
		t.Fatal(err)
	}
	var fired int
	b := New[int, string]("charge-flow")
	charge := b.Step("charge", func(_ context.Context, n int) (int, error) { fired++; return n, nil })
	done := b.Step("done", func(_ context.Context, n int) (string, error) { return "charged " + strconv.Itoa(n), nil })
	b.Edge(charge, done)
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := flow.Run(ctx, j, "r", 5); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled drive: err = %v, want context.Canceled", err)
	}
	if fired != 0 {
		t.Fatalf("the cancelled drive ran the node's body %d times, want 0", fired)
	}
	recs, err := j2.History(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	notStarted := 0
	for _, r := range recs {
		if r.Kind == agent.StepNotStarted {
			notStarted++
		}
	}
	if !hasRecord(recs, "attempt:step:node:charge") || notStarted != 1 {
		t.Fatalf("after the cancelled drive the journal holds %v; want the node's marker and one not-started record", names(recs))
	}

	out, err := flow.Run(context.Background(), j2, "r", 5)
	if err != nil || out != "charged 5" {
		t.Fatalf("re-drive: out %q, err %v; want the node re-attempted, not a halt", out, err)
	}
	if fired != 1 {
		t.Fatalf("the node's body ran %d times, want 1", fired)
	}
	if recs, err = j2.History(context.Background(), "r"); err != nil || !hasRecord(recs, "attempt:retry:1:step:node:charge") {
		t.Fatalf("the re-attempt claimed no numbered marker: %v (err %v)", names(recs), err)
	}
	if ok, diffs, err := flow.Conform(context.Background(), j2, "r"); err != nil || !ok {
		t.Fatalf("Conform = %v, %v, %v; want the re-attempted run to conform", ok, diffs, err)
	}
}

// errNoComplete is what noComplete returns for a run's completion.
var errNoComplete = errors.New("the completion was lost")

// noComplete is a store that loses every run's completion: a flow driven over a Journal on it
// runs to the end and records everything but run:complete, as a process that died at the end
// would.
type noComplete struct{ *agent.MemStore }

func (s noComplete) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	if name == "run:complete" {
		return agent.Entry{}, false, errNoComplete
	}
	return s.MemStore.Insert(ctx, runID, name, data)
}

// names lists the names of recs, for failure messages.
func names(recs []agent.Record) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.Name
	}
	return out
}

// A flow run records how it started: its kind, its flow's name and its input. A drive with
// another input, under another flow's name, or of a run an Agent started is ErrConfig, and
// records nothing; the recorded start reads back through agent.RecordedStart.
func TestLowering_RunStartHoldsTheFlowAndItsInput(t *testing.T) {
	ctx := context.Background()
	mem := agenttest.MemJournal()
	var fired int
	flow := effectFlow(t, &fired, Idempotent())
	if _, err := flow.Run(ctx, mem, "r", 5); err == nil {
		t.Fatal("first drive: want the node's error")
	}
	start, ok, err := agent.RecordedStart(ctx, mem, "r")
	if err != nil || !ok {
		t.Fatalf("RecordedStart: %v, %v", ok, err)
	}
	if start.Kind != agent.RunKindFlow || start.Flow == nil || start.Flow.Name != "charge-flow" || start.Input.Text() != "5" {
		t.Fatalf("recorded start = %+v, want a flow run of charge-flow with input 5", start)
	}
	before, err := mem.History(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := flow.Run(ctx, mem, "r", 6); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("resume with another input: err = %v, want ErrConfig", err)
	}
	other := New[int, string]("other-flow")
	other.Step("charge", func(_ context.Context, n int) (string, error) { return "", nil })
	otherFlow, err := other.Build()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := otherFlow.Run(ctx, mem, "r", 5); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("resume under another flow's name: err = %v, want ErrConfig", err)
	}
	a := agenttest.MustNew(agent.NewScriptedModel(agent.TextTurn("hi")), mem)
	if _, err := a.Run(ctx, "r", agent.UserText("5")); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("an Agent driving a flow's run: err = %v, want ErrConfig", err)
	}
	after, err := mem.History(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("refused drives recorded %v, want nothing beyond %v", names(after), names(before))
	}
	if fired != 1 {
		t.Fatalf("a refused drive ran the node: %d calls, want 1", fired)
	}
	if out, err := flow.Run(ctx, mem, "r", 5); err != nil || out != "charged 5" {
		t.Fatalf("resume with the recorded input: out %q, err %v", out, err)
	}

	// The other way round: a flow does not drive a run an Agent started.
	if _, err := a.Run(ctx, "agent-run", agent.UserText("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := flow.Run(ctx, mem, "agent-run", 5); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("a flow driving an Agent's run: err = %v, want ErrConfig", err)
	}
	if recs, err := mem.History(ctx, "agent-run"); err != nil || hasRecord(recs, flowDigestStep) {
		t.Fatalf("the refused flow drive recorded into the agent run: %v (err %v)", names(recs), err)
	}
}

// The input is held by its JSON: a struct input that encodes the same resumes, one that does not
// is refused.
func TestLowering_RunStartComparesInputJSON(t *testing.T) {
	type order struct {
		ID    string   `json:"id"`
		Items []string `json:"items"`
	}
	ctx := context.Background()
	mem := agenttest.MemJournal()
	b := New[order, int]("orders")
	b.Step("count", func(_ context.Context, o order) (int, error) { return len(o.Items), nil })
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if n, err := flow.Run(ctx, mem, "o", order{ID: "a", Items: []string{"x", "y"}}); err != nil || n != 2 {
		t.Fatalf("first drive: %d, %v", n, err)
	}
	if n, err := flow.Run(ctx, mem, "o", order{ID: "a", Items: []string{"x", "y"}}); err != nil || n != 2 {
		t.Fatalf("resume with an equal input: %d, %v", n, err)
	}
	if _, err := flow.Run(ctx, mem, "o", order{ID: "a", Items: []string{"x"}}); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("resume with a different input: err = %v, want ErrConfig", err)
	}
}

// A node reads the journal by point reads: no node loads the run's history, however many nodes a
// drive runs, whether it runs them or replays them.
func TestLowering_NoHistoryReadsPerNode(t *testing.T) {
	for _, n := range []int{1, 6} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			b := New[int, int]("chain")
			prev := b.Step("n0", func(_ context.Context, v int) (int, error) { return v + 1, nil })
			for i := 1; i < n; i++ {
				next := b.Step("n"+strconv.Itoa(i), func(_ context.Context, v int) (int, error) { return v + 1, nil })
				b.Edge(prev, next)
				prev = next
			}
			flow, err := b.Build()
			if err != nil {
				t.Fatal(err)
			}
			cs := agenttest.NewCountingStore(agent.NewMemStore())
			j, err := agent.NewJournal(cs)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if out, err := flow.Run(ctx, j, "r", 0); err != nil || out != n {
				t.Fatalf("Run: %d, %v", out, err)
			}
			// The one Load is the journal's check that its header is the run's first entry, which
			// every first drive of a new run makes (the budget agenttest holds the engine to). The
			// point reads are flow:digest's and each node's memo, run:cancelled once each node will
			// run (D1: the node's claim is won), and run:cancelled read back after run:complete.
			if c := cs.Reset(); c.Load != 1 || c.Get != 2*n+2 {
				t.Fatalf("a fresh drive of %d nodes loaded the run %d times and made %d point reads, want 1 and %d (%v)", n, c.Load, c.Get, 2*n+2, c.Names)
			}
			if out, err := flow.Run(ctx, j, "r", 0); err != nil || out != n {
				t.Fatalf("replay: %d, %v", out, err)
			}
			c := cs.Reset()
			if c.Load != 0 {
				t.Fatalf("a replay of %d nodes loaded the run %d times, want 0 (%v)", n, c.Load, c.Names)
			}
			// A replay of a finished run reads its end markers (run:complete and run:cancelled: the
			// first is its end) and nothing else, and writes nothing but the start's
			// insert-if-absent (which finds the recorded one).
			if c.Get != 2 || c.Insert != 1 || c.Inserted != 0 {
				t.Fatalf("a replay of %d nodes made %d point reads and %d inserts storing %d entries, want 2, 1 and 0 (%v)", n, c.Get, c.Insert, c.Inserted, c.Names)
			}
		})
	}
}

// A node that is not retry-safe must not pause, as for agent.Step: its body's pause is ErrConfig,
// and its marker stays, so the next drive halts rather than run the body again. A retry-safe node
// pauses.
func TestLowering_StepPauseGuard(t *testing.T) {
	pausing := func(ran *int) func(context.Context, int) (int, error) {
		return func(context.Context, int) (int, error) {
			*ran++
			return 0, &agent.SignalPending{RunRef: agent.RunRef{RunID: "r", RootRunID: "r"}, Name: "go"}
		}
	}
	ctx := context.Background()

	var ran int
	b := New[int, int]("pausing")
	b.Step("wait", pausing(&ran))
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	mem := agenttest.MemJournal()
	_, err = flow.Run(ctx, mem, "r", 1)
	if !errors.Is(err, agent.ErrConfig) || agent.IsPause(err) {
		t.Fatalf("a side-effect node that paused: err = %v, want ErrConfig and not a pause", err)
	}
	_, err = flow.Run(ctx, mem, "r", 1)
	if halt, ok := errors.AsType[*agent.OutcomeUnknown](err); !ok || halt.Op.ID != "node:wait" || ran != 1 {
		t.Fatalf("the next drive: err = %v, body ran %d times; want a halt on node:wait and 1", err, ran)
	}

	ran = 0
	rb := New[int, int]("pausing")
	rb.Step("wait", pausing(&ran), ReadOnly())
	rflow, err := rb.Build()
	if err != nil {
		t.Fatal(err)
	}
	_, err = rflow.Run(ctx, agenttest.MemJournal(), "r", 1)
	if p, ok := errors.AsType[*agent.SignalPending](err); !ok || p.Name != "go" {
		t.Fatalf("a retry-safe node that paused: err = %v, want its *agent.SignalPending", err)
	}
}

// RecoverLoop drives a flow run that halts as a pause, not a failure: the halt is not reported,
// while a flow run that fails is.
func TestLowering_RecoverLoopTreatsAFlowHaltAsAPause(t *testing.T) {
	mem := agenttest.MemJournal()
	var fired int
	halting := effectFlow(t, &fired)
	if _, err := halting.Run(context.Background(), mem, "halting", 5); err == nil {
		t.Fatal("first drive: want the node's error")
	}
	b := New[int, int]("failing")
	b.Step("fail", func(context.Context, int) (int, error) { return 0, errors.New("a genuine failure") }, ReadOnly())
	failing, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failing.Run(context.Background(), mem, "failing", 1); err == nil {
		t.Fatal("want the failing flow's error")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	drives := map[string]int{}
	var results []error
	var reported []error
	resume := func(ctx context.Context, runID string, _ agent.RunStart) error {
		var err error
		switch runID {
		case "halting":
			_, err = halting.Run(ctx, mem, runID, 5)
		case "failing":
			_, err = failing.Run(ctx, mem, runID, 1)
		}
		mu.Lock()
		defer mu.Unlock()
		drives[runID]++
		results = append(results, err)
		// A run is driven again only once its earlier drive has finished, report included, so
		// once each run has been driven twice every report of its first drive is in.
		if drives["halting"] >= 2 && drives["failing"] >= 2 {
			cancel()
		}
		return err
	}
	loopErr := agent.RecoverLoop(ctx, mem, resume,
		agent.WithRecoverInterval(time.Millisecond),
		agent.WithRecoverErrors(func(err error) { mu.Lock(); reported = append(reported, err); mu.Unlock() }))
	if !errors.Is(loopErr, context.Canceled) {
		t.Fatalf("RecoverLoop: %v", loopErr)
	}
	mu.Lock()
	defer mu.Unlock()
	var halts int
	for _, err := range results {
		if _, ok := errors.AsType[*agent.OutcomeUnknown](err); ok {
			halts++
		}
	}
	if halts < 2 {
		t.Fatalf("the halting run's drives returned %v, want halts", results)
	}
	var failures int
	for _, err := range reported {
		if _, ok := errors.AsType[*agent.OutcomeUnknown](err); ok {
			t.Fatalf("RecoverLoop reported the flow's halt as a failure: %v", err)
		}
		failures++
	}
	if failures == 0 {
		t.Fatal("RecoverLoop reported no failure for the failing flow: the check is vacuous")
	}
	if fired != 1 {
		t.Fatalf("recovery ran the halted node's body again: %d calls, want 1", fired)
	}
}

// Conform reads the lowered keys: a run's start must name this flow, a node's record is
// "node:<name>" (a bare node name, the former key, is a divergence), and a Step marker of a
// declared node is internal while one of an undeclared node, or a tool call's marker, is not.
func TestLowering_ConformReadsTheLoweredKeys(t *testing.T) {
	ctx := context.Background()
	var fired int
	flow := effectFlow(t, &fired, Idempotent())
	for _, tc := range []struct {
		name string
		rec  agent.Record
		diff string
	}{
		{"run:start", agent.Record{Kind: agent.StepValue, Result: []byte(`{"input":"5","kind":"flow","flow":{"name":"other"}}`)}, "run:start (not a run of this flow)"},
		{"run:start", agent.Record{Kind: agent.StepValue, Result: []byte(`{"input":"5"}`)}, "run:start (not a run of this flow)"},
		{"charge", agent.Record{Kind: agent.StepValue, Result: []byte(`5`)}, "charge (unexpected step)"},
		{"node:ghost", agent.Record{Kind: agent.StepValue, Result: []byte(`5`)}, "node:ghost (unexpected step)"},
		{"attempt:step:node:ghost", agent.Record{Kind: agent.StepAttempt, ToolUseID: "node:ghost"}, "attempt:step:node:ghost (attempt for undeclared step)"},
		{"attempt:tool:charge", agent.Record{Kind: agent.StepAttempt, ToolUseID: "charge"}, "attempt:tool:charge (unexpected step)"},
		{"attempt:step:node:charge", agent.Record{Kind: agent.StepValue}, "attempt:step:node:charge (unexpected step)"},
	} {
		t.Run(tc.name+"/"+tc.diff, func(t *testing.T) {
			mem := agenttest.MemJournal()
			if _, err := journaltest.Do(ctx, mem, "r", tc.name, func(context.Context) (agent.Record, error) { return tc.rec, nil }); err != nil {
				t.Fatal(err)
			}
			ok, diffs, err := flow.Conform(ctx, mem, "r")
			if err != nil || ok || len(diffs) != 1 || diffs[0] != tc.diff {
				t.Fatalf("Conform = %v, %q, %v; want the one divergence %q", ok, diffs, err, tc.diff)
			}
		})
	}
	mem := agenttest.MemJournal()
	if _, err := flow.Run(ctx, mem, "clean", 5); err == nil {
		t.Fatal("first drive: want the node's error")
	}
	if out, err := flow.Run(ctx, mem, "clean", 5); err != nil || out != "charged 5" {
		t.Fatalf("second drive: %q, %v", out, err)
	}
	if ok, diffs, err := flow.Conform(ctx, mem, "clean"); err != nil || !ok {
		t.Fatalf("Conform = %v, %v, %v; want a clean run to conform", ok, diffs, err)
	}
}
