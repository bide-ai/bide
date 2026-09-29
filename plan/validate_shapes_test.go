package plan

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// Build rejects wiring the runtime cannot execute as declared; before, each of these built and
// then ran wrong (an arm that was not chosen ran, an edge was never taken, an input depended on
// map order, a nested loop crashed). The error names the offending step.
func TestBuild_RejectsShapesRunCannotExecute(t *testing.T) {
	id := func(n int) (int, error) { return n, nil }
	never := func(int) bool { return false }
	cases := map[string]struct {
		build func() error
		names string
	}{
		"fan-in without a Join": {func() error {
			b := New[int, int]("f")
			x, a, c, z := b.Step("x", id), b.Step("a", id), b.Step("b", id), b.Step("z", id)
			b.Edge(x, a)
			b.Edge(x, c)
			b.Edge(a, z)
			b.Edge(c, z)
			_, err := b.Build()
			return err
		}, `"z"`},
		"two Switches over one step": {func() error {
			b := New[int, int]("f")
			x, p, q := b.Step("x", id), b.Step("p", id), b.Step("q", id)
			b.Switch(x, Else(p))
			b.Switch(x, Else(q))
			_, err := b.Build()
			return err
		}, `"x"`},
		"a Switch and an Edge from one step": {func() error {
			b := New[int, int]("f")
			x, side, p := b.Step("x", id), b.Step("side", id), b.Step("p", id)
			b.Edge(x, side)
			b.Switch(x, Else(p))
			_, err := b.Build()
			return err
		}, `"x"`},
		"a Switch inside a loop body": {func() error {
			b := New[int, int]("f")
			h, s, over, exitB, done := b.Step("h", id), b.Step("s", id), b.Step("over", id), b.Step("exitB", id), b.Step("done", id)
			b.Edge(h, s)
			b.Switch(s, When(func(n int) bool { return n != 0 }, over), Else(exitB))
			b.Switch(over, LoopBack(3, never, h), Else(done))
			_, err := b.Build()
			return err
		}, `"s"`},
		"a nested loop": {func() error {
			b := New[int, int]("f")
			oh, ih, ov, done := b.Step("oh", id), b.Step("ih", id), b.Step("ov", id), b.Step("done", id)
			b.Edge(oh, ih)
			b.Switch(ih, LoopBack(10, func(n int) bool { return n < 3 }, ih), Else(ov))
			b.Switch(ov, LoopBack(10, never, oh), Else(done))
			_, err := b.Build()
			return err
		}, `"ih"`},
		"an Edge leaving a loop body": {func() error {
			b := New[int, int]("f")
			h, m, over, done, side := b.Step("h", id), b.Step("m", id), b.Step("over", id), b.Step("done", id), b.Step("side", id)
			b.Edge(h, m)
			b.Edge(m, over)
			b.Edge(m, side)
			b.Switch(over, LoopBack(3, never, h), Else(done))
			_, err := b.Build()
			return err
		}, `"side"`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := c.build()
			if err == nil || !strings.Contains(err.Error(), c.names) {
				t.Fatalf("Build = %v; want an error naming %s", err, c.names)
			}
		})
	}
}

// A run started under one flow is not resumed under a changed one: its journal only means what
// it meant under the flow it started with.
func TestRun_RefusesToResumeUnderAChangedFlow(t *testing.T) {
	mem := agent.NewMemStore()
	b1 := New[int, int]("d")
	x1 := b1.Step("x", func(n int) (int, error) { return n, nil }, ReadOnly())
	p1 := b1.Step("p", func(int) (int, error) { return 0, errors.New("stopped") })
	b1.Edge(x1, p1)
	f1, _ := b1.Build()
	_, _ = f1.Run(context.Background(), mem, "r", 1)

	b2 := New[int, int]("d")
	x2 := b2.Step("x", func(n int) (int, error) { return n, nil }, ReadOnly())
	extra := b2.Step("extra", func(n int) (int, error) { return n * 100, nil })
	p2 := b2.Step("p", func(n int) (int, error) { return n, nil }, ReadOnly())
	b2.Edge(x2, extra)
	b2.Edge(extra, p2)
	f2, _ := b2.Build()
	if out, err := f2.Run(context.Background(), mem, "r", 1); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("resume under a changed flow = %d, %v; want ErrConfig", out, err)
	}
}
