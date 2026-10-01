package plan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// A completed run whose nodes ran Parallel tasks and nested Steps conforms.
func TestRev103d_ConformParallelAndNestedSteps(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	b := New[string, string]("par")
	a := b.Step("a", func(ctx context.Context, in string) (string, error) {
		res, err := agent.Parallel(ctx, mem, "r", []agent.Task[string]{
			{Name: "x", Fn: func(context.Context) (string, error) { return "<x>", nil }},
			{Name: "y:z", Fn: func(context.Context) (string, error) { return "&y", nil }},
		}, agent.WithMaxConcurrency(2))
		if err != nil {
			return "", err
		}
		outer, err := agent.Step(ctx, mem, "r", "outer", func(ctx context.Context) (string, error) {
			return agent.Step(ctx, mem, "r", "inner", func(context.Context) (string, error) { return "in", nil })
		})
		return in + res[0] + res[1] + outer, err
	})
	c := b.Step("c", func(ctx context.Context, in string) (string, error) { return in + "!", nil })
	b.Edge(a, c)
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	out, err := flow.Run(ctx, mem, "r", "go<")
	if err != nil {
		t.Fatal(err)
	}
	recs, _ := mem.History(ctx, "r")
	for _, r := range recs {
		t.Logf("%s %s %s", r.Kind, r.Name, r.Result)
	}
	if ok, diffs, err := flow.Conform(ctx, mem, "r"); err != nil || !ok {
		t.Fatalf("Conform of a completed run with Parallel and nested Steps = %v, %q, %v (out %q)", ok, diffs, err, out)
	}
}

// A run that halted mid-node (the terminal, and a Step inside a node), resolved and resumed, conforms.
func TestRev103d_ConformAfterHaltResolved(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	var fails = map[string]bool{"inside": true, "last": true}
	b := New[int, string]("halts")
	a := b.Step("a", func(ctx context.Context, n int) (int, error) {
		return agent.Step(ctx, mem, "r", "inside", func(context.Context) (int, error) {
			if fails["inside"] {
				return 0, errors.New("crash inside")
			}
			return n + 1, nil
		})
	})
	last := b.Step("last", func(_ context.Context, n int) (string, error) {
		if fails["last"] {
			return "", errors.New("crash at the terminal")
		}
		return fmt.Sprint("<", n, ">"), nil
	})
	b.Edge(a, last)
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		_, err = flow.Run(ctx, mem, "r", 1)
		if err == nil {
			break
		}
		halt, ok := errors.AsType[*agent.OutcomeUnknown](err)
		if !ok {
			continue // the first failed attempt; the next drive halts
		}
		var res any = 41
		if halt.Op.ID == "node:last" {
			res = "<resolved & done>"
		}
		if rerr := flow.ResolveHalt(ctx, mem, halt.Ref(), agent.Outcome{Result: res}); rerr != nil {
			t.Fatalf("ResolveHalt %s: %v", halt.Op.ID, rerr)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	out, _ := flow.Run(ctx, mem, "r", 1)
	if out != "<resolved & done>" {
		t.Fatalf("out %q", out)
	}
	if ok, diffs, err := flow.Conform(ctx, mem, "r"); err != nil || !ok {
		recs, _ := mem.History(ctx, "r")
		for _, r := range recs {
			t.Logf("%s %s %s", r.Kind, r.Name, r.Result)
		}
		t.Fatalf("Conform after resolved halts = %v, %q, %v", ok, diffs, err)
	}
}

// A run begun before the escaping change (input, digest and a node result written by json.Marshal,
// with HTML escapes) resumes after it, completes, and conforms.
func TestRev103d_ResumeAcrossTheEscapingChange(t *testing.T) {
	ctx := context.Background()
	b := New[map[string]string, string]("mix")
	a := b.Step("a", func(_ context.Context, in map[string]string) (string, error) { return in["k"] + "<&>", nil })
	c := b.Step("c", func(_ context.Context, s string) (string, error) { return s + "</end>", nil })
	b.Edge(a, c)
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	in := map[string]string{"k": "<in>", "z<": "&"}
	src := agent.NewMemStore()
	want, err := flow.Run(ctx, src, "r", in)
	if err != nil {
		t.Fatal(err)
	}
	recs, _ := src.History(ctx, "r")
	old := agent.NewMemStore()
	for _, r := range recs {
		if r.Kind == agent.StepHeader || r.Name == "node:c" || r.Name == "attempt:step:node:c" || r.Name == "run:complete" {
			continue
		}
		switch r.Name {
		case "run:start":
			var st agent.RunStart
			_ = json.Unmarshal(r.Result, &st)
			inb, _ := json.Marshal(in)
			st.Input = agent.UserText(string(inb))
			r.Result, _ = json.Marshal(st)
		case "node:a":
			var s string
			_ = json.Unmarshal(r.Result, &s)
			r.Result, _ = json.Marshal(s)
		}
		if _, err := old.Do(ctx, "r", r.Name, func(context.Context) (agent.Record, error) { return r, nil }); err != nil {
			t.Fatal(err)
		}
	}
	got, err := flow.Run(ctx, old, "r", in)
	if err != nil || got != want {
		t.Fatalf("resume across the change = %q, %v; want %q", got, err, want)
	}
	if ok, diffs, err := flow.Conform(ctx, old, "r"); err != nil || !ok {
		t.Fatalf("Conform = %v, %q, %v", ok, diffs, err)
	}
	if again, err := flow.Run(ctx, old, "r", in); err != nil || again != want {
		t.Fatalf("drive of the completed run = %q, %v", again, err)
	}
}
