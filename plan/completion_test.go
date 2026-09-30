package plan

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// A completed flow run records run:complete with its flow and output: recovery skips it, a drive
// with its input returns the output whatever the flow's topology is now, and a drive with another
// input, under another flow's name, or by an Agent is ErrConfig.
func TestCompletion_FinishedFlowRun(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
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
	a := agent.New(agent.NewScriptedModel(agent.TextTurn("hi")), mem)
	if _, err := a.Run(ctx, "r", "5"); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("an Agent over a finished flow run: %v, want ErrConfig", err)
	}

	// A flow over an agent's finished run is refused, not handed the agent's completion.
	if _, err := a.Run(ctx, "agent-run", "hello"); err != nil {
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
			mem := agent.NewMemStore()
			if _, err := flow.Run(ctx, mem, "r", 2); err != nil {
				t.Fatal(err)
			}
			if _, err := mem.Do(ctx, "r", tc.name, func(context.Context) (agent.Record, error) {
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
		smem := agent.NewMemStore()
		if _, err := swFlow.Run(ctx, smem, "r", 1); err != nil {
			t.Fatal(err)
		}
		if _, err := smem.Do(ctx, "r", tc.name, func(context.Context) (agent.Record, error) {
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
		smem := agent.NewMemStore()
		for _, w := range []struct{ name, value string }{
			{"run:start", `{"input":"1","kind":"flow","flow":{"name":"sw"}}`},
			{flowDigestStep, strconv.Quote(swFlow.Digest())},
			{"node:a", `1`},
		} {
			if w.name == missing {
				continue
			}
			if _, err := smem.Do(ctx, "r", w.name, func(context.Context) (agent.Record, error) {
				return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(w.value)}, nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		if ok, diffs, _ := swFlow.Conform(ctx, smem, "r"); ok || len(diffs) != 1 || !strings.HasPrefix(diffs[0], missing+" (missing") {
			t.Fatalf("Conform without %s = %v, %q; want it reported missing", missing, ok, diffs)
		}
	}

	mem := agent.NewMemStore()
	for _, w := range []struct{ name, value string }{
		{"run:start", `{"input":"2","kind":"flow","flow":{"name":"countdown"}}`},
		{flowDigestStep, strconv.Quote(flow.Digest())},
		{"run:complete", `{"flow":"other","output":"x"}`},
	} {
		if _, err := mem.Do(ctx, "r", w.name, func(context.Context) (agent.Record, error) {
			return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(w.value)}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if ok, diffs, _ := flow.Conform(ctx, mem, "r"); ok || len(diffs) != 1 || diffs[0] != "run:complete (not completed by this flow)" {
		t.Fatalf("Conform = %v, %q; want the other flow's completion flagged", ok, diffs)
	}
}

// Flow.ResolveHalt checks a resolution against the flow before agent.ResolveHaltRef records it.
func TestFlowResolveHalt_ChecksTheResolution(t *testing.T) {
	ctx := context.Background()
	halted := func(t *testing.T) (*Flow[int, string], *agent.MemStore, *agent.OutcomeUnknown) {
		t.Helper()
		mem := agent.NewMemStore()
		var fired int
		flow := effectFlow(t, &fired)
		_, _ = flow.Run(ctx, mem, "r", 5)
		_, err := flow.Run(ctx, mem, "r", 5)
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
			if err := flow.ResolveHalt(ctx, mem, tc.ref(halt.Ref()), tc.out); !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "loop body") {
				t.Fatalf("ResolveHalt of an iteration outside a loop: %v, want the flow's refusal", err)
			}
			continue
		}
		if tc.name == "a tool call" {
			flow, mem, halt := halted(t)
			ref := tc.ref(halt.Ref())
			if err := flow.ResolveHalt(ctx, mem, ref, tc.out); !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "halts are on Steps") {
				t.Fatalf("ResolveHalt of a tool call: %v, want the flow's refusal", err)
			}
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			flow, mem, halt := halted(t)
			ref := halt.Ref()
			if tc.ref != nil {
				ref = tc.ref(ref)
			}
			if err := flow.ResolveHalt(ctx, mem, ref, tc.out); !errors.Is(err, agent.ErrConfig) {
				t.Fatalf("ResolveHalt: %v, want ErrConfig", err)
			}
			recs, _ := mem.History(ctx, "r")
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
	smem := agent.NewMemStore()
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
	if err := flow.ResolveHalt(ctx, mem, halt.Ref(), agent.Outcome{Result: 9}); err != nil {
		t.Fatalf("ResolveHalt: %v", err)
	}
	if out, err := flow.Run(ctx, mem, "r", 5); err != nil || out != "charged 9" {
		t.Fatalf("after the resolution: %q, %v", out, err)
	}

	// A failed outcome needs no value of the node's type: the node then fails on every drive.
	flow, mem, halt = halted(t)
	if err := flow.ResolveHalt(ctx, mem, halt.Ref(), agent.Outcome{Result: "declined", IsError: true}); err != nil {
		t.Fatalf("ResolveHalt of a failure: %v", err)
	}
	if _, err := flow.Run(ctx, mem, "r", 5); !errors.Is(err, agent.ErrTool) || !strings.Contains(err.Error(), "resolved as failed") {
		t.Fatalf("after a failed resolution: %v, want the node's failure", err)
	}
}

// A Step a node's body runs is recorded under the node's key, so a halt of it names that key and
// resolves like a node's: the node's body runs again, and the Step returns the resolved value
// without running its effect.
func TestNestedStep_HaltResolvesUnderTheNodeKey(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	var charges, bodies int
	b := New[int, int]("nested")
	b.Step("a", func(ctx context.Context, n int) (int, error) {
		bodies++
		return agent.Step(ctx, mem, "r", "charge", func(context.Context) (int, error) {
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
