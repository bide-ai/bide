package plan

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/journaltest"
)

// A completed flow run records run:complete with its flow and output: recovery skips it, a drive
// with its input returns the output whatever the flow's topology is now, and a drive with another
// input, under another flow's name, or by an Agent is ErrConfig.
func TestCompletion_FinishedFlowRun(t *testing.T) {
	ctx := context.Background()
	mem := agenttest.MemJournal()
	var fired int
	flow := effectFlow(t, &fired, Idempotent())
	if _, err := flow.Run(ctx, mem, "r", 5); err == nil {
		t.Fatal("first drive: want the node's error")
	}
	if out, err := flow.Run(ctx, mem, "r", 5); err != nil || out != "charged 5" {
		t.Fatalf("second drive: %q, %v", out, err)
	}
	if done, err := agent.IsComplete(ctx, mem, "r"); err != nil || !done {
		t.Fatalf("IsComplete = %v, %v; want true", done, err)
	}
	rec, err := mem.History(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	last := rec[len(rec)-1]
	if last.Name != "run:complete" || string(last.Result) != `{"flow":"charge-flow","output":"charged 5"}` {
		t.Fatalf("last record = %s %s, want the completion with the flow and its output", last.Name, last.Result)
	}

	// The same flow with another topology: the finished run returns its recorded output.
	b := New[int, string]("charge-flow")
	b.Step("charge", func(_ context.Context, n int) (string, error) { fired++; return "new", nil })
	changed, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := changed.Run(ctx, mem, "r", 5); err != nil || out != "charged 5" || fired != 2 {
		t.Fatalf("a changed flow over the finished run: %q, %v, fired %d; want the recorded output and no body run", out, err, fired)
	}
	if _, err := flow.Run(ctx, mem, "r", 6); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("a finished run with another input: %v, want ErrConfig", err)
	}
	ob := New[int, string]("other")
	ob.Step("x", func(_ context.Context, n int) (string, error) { return "", nil })
	other, err := ob.Build()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Run(ctx, mem, "r", 5); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("another flow over the finished run: %v, want ErrConfig", err)
	}
	a := agenttest.MustNew(agenttest.NewScriptedModel(agenttest.TextTurn("hi")), mem)
	if _, err := a.Run(ctx, "r", agent.UserText("5")); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("an Agent over a finished flow run: %v, want ErrConfig", err)
	}

	// A flow over an agent's finished run is refused, not handed the agent's completion.
	if _, err := a.Run(ctx, "agent-run", agent.UserText("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := flow.Run(ctx, mem, "agent-run", 5); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("a flow over an agent's finished run: %v, want ErrConfig", err)
	}
	for _, raw := range []string{`{}`, `{"flow":"g","output":"x"}`, `not json`} {
		if _, err := decodeCompletion[string]("f", "r", json.RawMessage(raw)); !errors.Is(err, agent.ErrConfig) {
			t.Fatalf("decodeCompletion of %s for flow f: %v, want ErrConfig", raw, err)
		}
	}
	if out, err := decodeCompletion[string]("f", "r", json.RawMessage(`{"flow":"f","output":"x"}`)); err != nil || out != "x" {
		t.Fatalf("decodeCompletion of f's completion: %q, %v", out, err)
	}
}

// Conform replays the run's routing and refuses records off it: an iteration the loop did not
// reach, a loop Switch's choice recorded as a linear one, a completion by another flow.
func TestConform_ReplaysTheRouting(t *testing.T) {
	ctx := context.Background()
	flow, err := buildCountdownLoop(10, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, value, diff string
	}{
		{"node:iter:3:refine", `{"N":0}`, "node:iter:3:refine (a loop iteration the run did not reach)"},
		{"node:refine", `{"N":0}`, "node:refine (a loop body node recorded outside an iteration)"},
		{"node:iter:0:seed", `{"N":0}`, "node:iter:0:seed (an iteration of a node outside any loop)"},
		{"switch:check", `"done"`, "switch:check (switch over undeclared node)"},
		{"switch:iter:5:check", `"done"`, "switch:iter:5:check (a loop iteration the run did not reach)"},
		{"node:refine:step:x", `1`, "node:refine:step:x (a loop body node recorded outside an iteration)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := agenttest.MemJournal()
			if _, err := flow.Run(ctx, mem, "r", 2); err != nil {
				t.Fatal(err)
			}
			if _, err := journaltest.Do(ctx, mem, "r", tc.name, func(context.Context) (agent.Record, error) {
				return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(tc.value)}, nil
			}); err != nil {
				t.Fatal(err)
			}
			ok, diffs, err := flow.Conform(ctx, mem, "r")
			if err != nil || ok || len(diffs) != 1 || diffs[0] != tc.diff {
				t.Fatalf("Conform = %v, %q, %v; want the one divergence %q", ok, diffs, err, tc.diff)
			}
		})
	}
	// A node on the arm its Switch did not take, alone.
	sb := New[int, string]("sw")
	a := sb.Step("a", func(_ context.Context, n int) (int, error) { return n, nil }, ReadOnly())
	yes := sb.Step("yes", func(_ context.Context, n int) (string, error) { return "yes", nil }, ReadOnly())
	no := sb.Step("no", func(_ context.Context, n int) (string, error) { return "no", nil }, ReadOnly())
	sb.Switch(a, When(func(v int) bool { return v > 0 }, yes), Else(no))
	swFlow, err := sb.Build()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, diff string }{
		{"node:no", "node:no (a node on a path the run did not take)"},
		{"node:a:x", "node:a:x (unexpected step)"},
		{"node:a:step:", "node:a:step: (unexpected step)"},
	} {
		smem := agenttest.MemJournal()
		if _, err := swFlow.Run(ctx, smem, "r", 1); err != nil {
			t.Fatal(err)
		}
		if _, err := journaltest.Do(ctx, smem, "r", tc.name, func(context.Context) (agent.Record, error) {
			return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`"no"`)}, nil
		}); err != nil {
			t.Fatal(err)
		}
		if ok, diffs, _ := swFlow.Conform(ctx, smem, "r"); ok || len(diffs) != 1 || diffs[0] != tc.diff {
			t.Fatalf("Conform = %v, %q; want the one divergence %q", ok, diffs, tc.diff)
		}
	}
	// A journal missing only its start, or only its digest.
	for _, missing := range []string{"run:start", flowDigestStep} {
		smem := agenttest.MemJournal()
		for _, w := range []struct{ name, value string }{
			{"run:start", `{"input":"1","kind":"flow","flow":{"name":"sw"}}`},
			{flowDigestStep, strconv.Quote(swFlow.Digest())},
			{"node:a", `1`},
		} {
			if w.name == missing {
				continue
			}
			if _, err := journaltest.Do(ctx, smem, "r", w.name, func(context.Context) (agent.Record, error) {
				return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(w.value)}, nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		if ok, diffs, _ := swFlow.Conform(ctx, smem, "r"); ok || len(diffs) != 1 || !strings.HasPrefix(diffs[0], missing+" (missing") {
			t.Fatalf("Conform without %s = %v, %q; want it reported missing", missing, ok, diffs)
		}
	}

	mem := agenttest.MemJournal()
	for _, w := range []struct{ name, value string }{
		{"run:start", `{"input":"2","kind":"flow","flow":{"name":"countdown"}}`},
		{flowDigestStep, strconv.Quote(flow.Digest())},
		{"run:complete", `{"flow":"other","output":"x"}`},
	} {
		if _, err := journaltest.Do(ctx, mem, "r", w.name, func(context.Context) (agent.Record, error) {
			return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(w.value)}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if ok, diffs, _ := flow.Conform(ctx, mem, "r"); ok || len(diffs) != 1 || diffs[0] != "run:complete (not completed by this flow)" {
		t.Fatalf("Conform = %v, %q; want the other flow's completion flagged", ok, diffs)
	}
}

// Flow.ResolveHalt checks a resolution against the flow before agent.ResolveHalt records it.
func TestFlowResolveHalt_ChecksTheResolution(t *testing.T) {
	ctx := context.Background()
	halted := func(t *testing.T) (*Flow[int, string], *agent.MemStore, *agent.OutcomeUnknown) {
		t.Helper()
		mem := agent.NewMemStore()
		j := agenttest.MustJournal(mem)
		var fired int
		flow := effectFlow(t, &fired)
		_, _ = flow.Run(ctx, j, "r", 5)
		_, err := flow.Run(ctx, j, "r", 5)
		halt, ok := errors.AsType[*agent.OutcomeUnknown](err)
		if !ok {
			t.Fatalf("want a halt: %v", err)
		}
		return flow, mem, halt
	}
	type fields struct {
		N int `json:"n"`
	}
	for _, tc := range []struct {
		name string
		ref  func(agent.HaltRef) agent.HaltRef
		out  agent.Outcome
	}{
		{"a string for an int", nil, agent.Outcome{Result: "charged"}},
		{"null for an int", nil, agent.Outcome{Result: nil}},
		{"an object for an int", nil, agent.Outcome{Result: fields{N: 1}}},
		{"a tool call", func(r agent.HaltRef) agent.HaltRef { r.Op.Kind = agent.OpTool; return r }, agent.Outcome{Result: 7}},
		{"an undeclared node", func(r agent.HaltRef) agent.HaltRef { r.Op.ID = "node:ghost"; return r }, agent.Outcome{Result: 7}},
		{"an iteration outside a loop", func(r agent.HaltRef) agent.HaltRef { r.Op.ID = "node:iter:0:charge"; return r }, agent.Outcome{Result: 7}},
		{"a key that is no node's", func(r agent.HaltRef) agent.HaltRef { r.Op.ID = "charge"; return r }, agent.Outcome{Result: 7}},
	} {
		if tc.name == "an iteration outside a loop" {
			flow, mem, halt := halted(t)
			j := agenttest.MustJournal(mem)
			if err := flow.ResolveHalt(ctx, j, tc.ref(halt.Ref()), tc.out); !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "loop body") {
				t.Fatalf("ResolveHalt of an iteration outside a loop: %v, want the flow's refusal", err)
			}
			continue
		}
		if tc.name == "a tool call" {
			flow, mem, halt := halted(t)
			j3 := agenttest.MustJournal(mem)
			ref := tc.ref(halt.Ref())
			if err := flow.ResolveHalt(ctx, j3, ref, tc.out); !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "halts are on Steps") {
				t.Fatalf("ResolveHalt of a tool call: %v, want the flow's refusal", err)
			}
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			flow, mem, halt := halted(t)
			j := agenttest.MustJournal(mem)
			ref := halt.Ref()
			if tc.ref != nil {
				ref = tc.ref(ref)
			}
			if err := flow.ResolveHalt(ctx, j, ref, tc.out); !errors.Is(err, agent.ErrConfig) {
				t.Fatalf("ResolveHalt: %v, want ErrConfig", err)
			}
			recs, _ := j.History(ctx, "r")
			if hasRecord(recs, "node:charge") {
				t.Fatal("a refused resolution recorded the node's output")
			}
		})
	}
	// A struct output: a field the type does not have is refused, as Run would drop it.
	type receipt struct {
		ID string `json:"id"`
	}
	sb := New[int, receipt]("struct")
	var sfired int
	sb.Step("issue", func(_ context.Context, n int) (receipt, error) {
		sfired++
		if sfired == 1 {
			return receipt{}, errors.New("lost")
		}
		return receipt{ID: "r"}, nil
	})
	sflow, err := sb.Build()
	if err != nil {
		t.Fatal(err)
	}
	smem := agenttest.MemJournal()
	_, _ = sflow.Run(ctx, smem, "s", 1)
	_, err = sflow.Run(ctx, smem, "s", 1)
	shalt, ok := errors.AsType[*agent.OutcomeUnknown](err)
	if !ok {
		t.Fatalf("want a halt: %v", err)
	}
	if err := sflow.ResolveHalt(ctx, smem, shalt.Ref(), agent.Outcome{Result: map[string]string{"id": "r", "Id2": "x"}}); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("ResolveHalt with an unknown field: %v, want ErrConfig", err)
	}
	if err := sflow.ResolveHalt(ctx, smem, shalt.Ref(), agent.Outcome{Result: receipt{ID: "r9"}}); err != nil {
		t.Fatalf("ResolveHalt with the receipt: %v", err)
	}
	if out, err := sflow.Run(ctx, smem, "s", 1); err != nil || out.ID != "r9" {
		t.Fatalf("after the resolution: %+v, %v", out, err)
	}

	flow, mem, halt := halted(t)
	j2 := agenttest.MustJournal(mem)
	if err := flow.ResolveHalt(ctx, j2, halt.Ref(), agent.Outcome{Result: 9}); err != nil {
		t.Fatalf("ResolveHalt: %v", err)
	}
	if out, err := flow.Run(ctx, j2, "r", 5); err != nil || out != "charged 9" {
		t.Fatalf("after the resolution: %q, %v", out, err)
	}

	// A failed outcome needs no value of the node's type: the node then fails on every drive.
	flow, mem, halt = halted(t)
	j2 = agenttest.MustJournal(mem)
	if err := flow.ResolveHalt(ctx, j2, halt.Ref(), agent.Outcome{Result: "declined", IsError: true}); err != nil {
		t.Fatalf("ResolveHalt of a failure: %v", err)
	}
	if _, err := flow.Run(ctx, j2, "r", 5); !errors.Is(err, agent.ErrTool) || !strings.Contains(err.Error(), "resolved as failed") {
		t.Fatalf("after a failed resolution: %v, want the node's failure", err)
	}
}

// A Step a node's body runs is recorded under the node's key, so a halt of it names that key and
// resolves like a node's: the node's body runs again, and the Step returns the resolved value
// without running its effect.
func TestNestedStep_HaltResolvesUnderTheNodeKey(t *testing.T) {
	ctx := context.Background()
	mem := agenttest.MemJournal()
	var charges, bodies int
	b := New[int, int]("nested")
	b.Step("a", func(ctx context.Context, n int) (int, error) {
		bodies++
		return mem.Step(ctx, "r", "charge", func(context.Context) (int, error) {
			charges++
			if charges == 1 {
				return 0, errors.New("connection reset after the charge")
			}
			return n + 1, nil
		})
	}, ReadOnly())
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := flow.Run(ctx, mem, "r", 1); err == nil {
		t.Fatal("first drive: want the Step's error")
	}
	_, err = flow.Run(ctx, mem, "r", 1)
	halt, ok := errors.AsType[*agent.OutcomeUnknown](err)
	if !ok || halt.Op != (agent.OpRef{Kind: agent.OpStep, ID: "node:a:step:charge"}) {
		t.Fatalf("second drive: %v, want a halt on node:a:step:charge", err)
	}
	if err := flow.ResolveHalt(ctx, mem, halt.Ref(), agent.Outcome{Result: 42}); err != nil {
		t.Fatalf("ResolveHalt: %v", err)
	}
	if out, err := flow.Run(ctx, mem, "r", 1); err != nil || out != 42 || charges != 1 {
		t.Fatalf("after the resolution: %d, %v, charges %d; want 42 and 1", out, err, charges)
	}
	if ok, diffs, err := flow.Conform(ctx, mem, "r"); err != nil || !ok {
		t.Fatalf("Conform = %v, %q, %v", ok, diffs, err)
	}
	if bodies != 3 {
		t.Fatalf("the ReadOnly node's body ran %d times, want 3 (once per drive)", bodies)
	}
}

// Flow.ResolveHalt resolves only a halt of a run of this flow: the run's recorded start must name
// this flow, and its recorded digest must be this flow's.
func TestFlowResolveHalt_RefusesARunOfAnotherFlow(t *testing.T) {
	ctx := context.Background()
	halted := func(t *testing.T) (*agent.MemStore, *agent.OutcomeUnknown) {
		t.Helper()
		mem := agent.NewMemStore()
		j := agenttest.MustJournal(mem)
		var fired int
		flow := effectFlow(t, &fired)
		_, _ = flow.Run(ctx, j, "r", 5)
		_, err := flow.Run(ctx, j, "r", 5)
		halt, ok := errors.AsType[*agent.OutcomeUnknown](err)
		if !ok {
			t.Fatalf("want a halt: %v", err)
		}
		return mem, halt
	}
	// The same name and node types, another topology: a different digest.
	var fired int
	b := New[int, string]("charge-flow")
	charge := b.Step("charge", func(_ context.Context, n int) (int, error) { fired++; return n, nil })
	done := b.Step("done", func(_ context.Context, n int) (string, error) { return "", nil })
	extra := b.Step("extra", func(_ context.Context, s string) (string, error) { return s, nil })
	b.Edge(charge, done)
	b.Edge(done, extra)
	changed, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	mem, halt := halted(t)
	j := agenttest.MustJournal(mem)
	if err := changed.ResolveHalt(ctx, j, halt.Ref(), agent.Outcome{Result: 7}); !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("a changed flow of the same name: %v, want ErrConfig naming the digest", err)
	}
	// A journal whose start names another flow, under this flow's digest.
	forged := agenttest.MemJournal()
	for _, w := range []struct {
		name string
		rec  agent.Record
	}{
		{"run:start", agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`{"input":"5","kind":"flow","flow":{"name":"other"}}`)}},
		{flowDigestStep, agent.Record{Kind: agent.StepValue, Result: json.RawMessage(strconv.Quote(effectFlow(t, &fired).Digest()))}},
		{"attempt:step:node:charge", agent.Record{Kind: agent.StepAttempt, ToolUseID: "node:charge", AttemptedAt: 1}},
	} {
		if _, err := journaltest.Do(ctx, forged, "r", w.name, func(context.Context) (agent.Record, error) { return w.rec, nil }); err != nil {
			t.Fatal(err)
		}
	}
	if err := effectFlow(t, &fired).ResolveHalt(ctx, forged, halt.Ref(), agent.Outcome{Result: 7}); !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "not a run of this flow") {
		t.Fatalf("a run whose start names another flow: %v, want ErrConfig", err)
	}
	// A run with no recorded start.
	var f2 int
	flow := effectFlow(t, &f2)
	bare := agenttest.MemJournal()
	ref := halt.Ref()
	if err := flow.ResolveHalt(ctx, bare, ref, agent.Outcome{Result: 7}); !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "not a run of this flow") {
		t.Fatalf("a run with no start: %v, want ErrConfig", err)
	}
	// The run's own flow resolves it.
	if err := flow.ResolveHalt(ctx, j, halt.Ref(), agent.Outcome{Result: 7}); err != nil {
		t.Fatalf("the run's own flow: %v", err)
	}
}

// A completion must be the output of the terminal node the run reached: one whose output differs
// is a divergence, and so is one with no reached terminal recorded.
func TestConform_CompletionIsTheTerminalOutput(t *testing.T) {
	ctx := context.Background()
	// A completion recorded while the run halted on its first node: the reached nodes, the terminal
	// among them, are not recorded.
	{
		mem := agenttest.MemJournal()
		var fired int
		flow := effectFlow(t, &fired)
		_, _ = flow.Run(ctx, mem, "r", 5)
		if _, err := flow.Run(ctx, mem, "r", 5); err == nil {
			t.Fatal("want a halt")
		}
		if _, err := journaltest.Do(ctx, mem, "r", "run:complete", func(context.Context) (agent.Record, error) {
			return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`{"flow":"charge-flow","output":"charged 5"}`)}, nil
		}); err != nil {
			t.Fatal(err)
		}
		want := []string{"node:charge (missing: the run completed without it)", "node:done (missing: the run completed without it)"}
		if ok, diffs, _ := flow.Conform(ctx, mem, "r"); ok || !slices.Equal(diffs, want) {
			t.Fatalf("Conform = %v, %q; want the reached nodes reported missing: %q", ok, diffs, want)
		}
	}
	for _, tc := range []struct {
		output, diff string
	}{
		{`"charged 6"`, "run:complete (its output is not the terminal node's)"},
		{`"charged 5"`, ""},
	} {
		mem := agent.NewMemStore()
		j := agenttest.MustJournal(mem)
		var fired int
		flow := effectFlow(t, &fired, Idempotent())
		// Run to the end but lose the completion, then record one by hand.
		_, _ = flow.Run(ctx, agenttest.MustJournal(noComplete{mem}), "r", 5)
		if _, err := flow.Run(ctx, agenttest.MustJournal(noComplete{mem}), "r", 5); !errors.Is(err, errNoComplete) {
			t.Fatalf("drive: %v", err)
		}
		if _, err := journaltest.Do(ctx, j, "r", "run:complete", func(context.Context) (agent.Record, error) {
			return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`{"flow":"charge-flow","output":` + tc.output + `}`)}, nil
		}); err != nil {
			t.Fatal(err)
		}
		ok, diffs, err := flow.Conform(ctx, j, "r")
		if tc.diff == "" {
			if err != nil || !ok {
				t.Fatalf("Conform of the right completion = %v, %q, %v", ok, diffs, err)
			}
			continue
		}
		if err != nil || ok || len(diffs) != 1 || diffs[0] != tc.diff {
			t.Fatalf("Conform = %v, %q, %v; want %q", ok, diffs, err, tc.diff)
		}
	}
}

// Inputs are compared exactly: a uint64 near its maximum, or an int64 beyond 2^53, differs from
// its neighbour, while the same value resumes the run.
func TestRunStart_ExactIntegerInputs(t *testing.T) {
	ctx := context.Background()
	b := New[uint64, uint64]("u64")
	b.Step("echo", func(_ context.Context, n uint64) (uint64, error) { return n, nil }, ReadOnly())
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	const top = ^uint64(0)
	mem := agenttest.MemJournal()
	if out, err := flow.Run(ctx, mem, "r", top); err != nil || out != top {
		t.Fatalf("first drive: %d, %v", out, err)
	}
	if out, err := flow.Run(ctx, mem, "r", top); err != nil || out != top {
		t.Fatalf("the same input: %d, %v", out, err)
	}
	if _, err := flow.Run(ctx, mem, "r", top-1); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("the input %d of a run started with %d: %v, want ErrConfig", top-1, top, err)
	}
}

// A completed loop run records every iteration it ran: a missing iteration's node or choice is a
// divergence.
func TestConform_CompletionRequiresEveryIteration(t *testing.T) {
	ctx := context.Background()
	flow, err := buildCountdownLoop(10, nil)
	if err != nil {
		t.Fatal(err)
	}
	src := agenttest.MemJournal()
	if _, err := flow.Run(ctx, src, "r", 3); err != nil {
		t.Fatal(err)
	}
	recs, err := src.History(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	for _, drop := range []string{"node:iter:1:refine", "switch:iter:0:check", "node:seed", "node:done"} {
		forged := agenttest.MemJournal()
		for _, r := range recs {
			if r.Name == drop || r.Kind == agent.StepHeader {
				continue
			}
			if _, err := journaltest.Do(ctx, forged, "r", r.Name, func(context.Context) (agent.Record, error) { return r, nil }); err != nil {
				t.Fatal(err)
			}
		}
		ok, diffs, err := flow.Conform(ctx, forged, "r")
		if err != nil || ok || !slices.Contains(diffs, drop+" (missing: the run completed without it)") {
			t.Fatalf("a completed run without %s: Conform = %v, %q, %v", drop, ok, diffs, err)
		}
	}
}

// A flow input with no canonical JSON is refused before anything is recorded.
func TestRun_InputWithARepeatedKeyIsRefused(t *testing.T) {
	ctx := context.Background()
	b := New[json.RawMessage, int]("raw")
	b.Step("n", func(_ context.Context, in json.RawMessage) (int, error) { return len(in), nil }, ReadOnly())
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	mem := agenttest.MemJournal()
	if _, err := flow.Run(ctx, mem, "r", json.RawMessage(`{"a":1,"a":2}`)); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("Run with a repeated key: %v, want ErrConfig", err)
	}
	if recs, _ := mem.History(ctx, "r"); len(recs) != 0 {
		t.Fatalf("the refused drive recorded %d records", len(recs))
	}
}

// A completed run records every choice its routing reached, and its completion is the last reached
// terminal's output, written as the journal writes values (no HTML escapes).
func TestConform_CompletionChoicesAndTerminals(t *testing.T) {
	ctx := context.Background()
	var count int
	sw, err := buildCounterFlow(&count)
	if err != nil {
		t.Fatal(err)
	}
	src := agenttest.MemJournal()
	if _, err := sw.Run(ctx, src, "r", 0); err != nil {
		t.Fatal(err)
	}
	recs, _ := src.History(ctx, "r")
	forged := agenttest.MemJournal()
	for _, r := range recs {
		if r.Name == "switch:entry" || r.Kind == agent.StepHeader {
			continue
		}
		if _, err := journaltest.Do(ctx, forged, "r", r.Name, func(context.Context) (agent.Record, error) { return r, nil }); err != nil {
			t.Fatal(err)
		}
	}
	if ok, diffs, _ := sw.Conform(ctx, forged, "r"); ok || !slices.Contains(diffs, "switch:entry (missing: the run completed without it)") {
		t.Fatalf("a completed run without its choice: Conform = %v, %q", ok, diffs)
	}

	// A fan-out to two terminals: the run completes with the later one's output, and conforms.
	b := New[int, string]("fan")
	a := b.Step("a", func(_ context.Context, n int) (int, error) { return n, nil }, ReadOnly())
	x := b.Step("x", func(_ context.Context, n int) (string, error) { return "x<", nil }, ReadOnly())
	y := b.Step("y", func(_ context.Context, n int) (string, error) { return "y&", nil }, ReadOnly())
	b.Edge(a, x)
	b.Edge(a, y)
	fan, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	mem := agenttest.MemJournal()
	out, err := fan.Run(ctx, mem, "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if ok, diffs, err := fan.Conform(ctx, mem, "r"); err != nil || !ok {
		t.Fatalf("Conform of a fan-out run = %v, %q, %v", ok, diffs, err)
	}
	recs, _ = mem.History(ctx, "r")
	for _, r := range recs {
		if r.Name == "run:complete" && string(r.Result) != `{"flow":"fan","output":"`+out+`"}` {
			t.Fatalf("the completion is recorded as %s, want the output %q unescaped", r.Result, out)
		}
	}
}

// Conform compares a completion with the terminal's output as JSON values: a terminal output some
// writer recorded with HTML escapes is the completion's unescaped output.
func TestConform_CompletionComparedAsJSONValues(t *testing.T) {
	ctx := context.Background()
	b := New[int, string]("html")
	b.Step("t", func(_ context.Context, n int) (string, error) { return "a<b", nil }, ReadOnly())
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	src := agenttest.MemJournal()
	if _, err := flow.Run(ctx, src, "r", 1); err != nil {
		t.Fatal(err)
	}
	esc := string([]byte{'\\', 'u', '0', '0', '3', 'c'})
	recs, _ := src.History(ctx, "r")
	forged := agenttest.MemJournal()
	for _, r := range recs {
		if r.Kind == agent.StepHeader {
			continue
		}
		if r.Name == "node:t" {
			r.Result = json.RawMessage(`"a` + esc + `b"`)
		}
		if _, err := journaltest.Do(ctx, forged, "r", r.Name, func(context.Context) (agent.Record, error) { return r, nil }); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := forged.History(ctx, "r"); !strings.Contains(string(got[len(got)-2].Result)+string(got[len(got)-1].Result), esc) {
		t.Fatal("the fixture did not keep the escape")
	}
	if ok, diffs, err := flow.Conform(ctx, forged, "r"); err != nil || !ok {
		t.Fatalf("Conform = %v, %q, %v; want the escaped and unescaped outputs to be one value", ok, diffs, err)
	}
}
