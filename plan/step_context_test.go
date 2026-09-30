package plan

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

type stepCtxKey struct{}

// blockUntilDone is a step or merge body: it checks that its ctx is the one the flow was run
// with (it carries the caller's value), signals that it started, and blocks until ctx is done.
func blockUntilDone(t *testing.T, ctx context.Context, started chan<- struct{}) error {
	t.Helper()
	if ctx.Value(stepCtxKey{}) != "caller" {
		t.Error("the body's ctx does not carry the value on the ctx Run was called with")
	}
	close(started)
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-time.After(10 * time.Second):
		return errors.New("the body's ctx was never cancelled")
	}
}

// expectCancelled runs run with a cancellable ctx, cancels it once the body starts, and checks
// that run returns promptly with an error wrapping context.Canceled.
func expectCancelled(t *testing.T, started <-chan struct{}, run func(ctx context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), stepCtxKey{}, "caller"))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("Run returned %v before the body started", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the body never started")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want an error wrapping context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling Run's ctx did not reach the running body")
	}
}

// Cancelling the ctx a flow runs with reaches a running Step, Join2 and Join3 body, and the body
// sees the caller's ctx values.
func TestStepBodiesReceiveRunContext(t *testing.T) {
	id := func(_ context.Context, n int) (int, error) { return n, nil }
	t.Run("Step", func(t *testing.T) {
		started := make(chan struct{})
		b := New[int, int]("step-ctx")
		b.Step("s", func(ctx context.Context, n int) (int, error) { return n, blockUntilDone(t, ctx, started) })
		flow, err := b.Build()
		if err != nil {
			t.Fatal(err)
		}
		expectCancelled(t, started, func(ctx context.Context) error {
			_, err := flow.Run(ctx, agent.NewMemStore(), "r", 1)
			return err
		})
	})
	t.Run("Join2", func(t *testing.T) {
		started := make(chan struct{})
		b := New[int, int]("join2-ctx")
		s := b.Step("s", id)
		x, y := b.Step("x", id), b.Step("y", id)
		b.Edge(s, x)
		b.Edge(s, y)
		b.Join2("j", x, y, func(ctx context.Context, a, c int) (int, error) { return a + c, blockUntilDone(t, ctx, started) })
		flow, err := b.Build()
		if err != nil {
			t.Fatal(err)
		}
		expectCancelled(t, started, func(ctx context.Context) error {
			_, err := flow.Run(ctx, agent.NewMemStore(), "r", 1)
			return err
		})
	})
	t.Run("Join3", func(t *testing.T) {
		started := make(chan struct{})
		b := New[int, int]("join3-ctx")
		s := b.Step("s", id)
		x, y, z := b.Step("x", id), b.Step("y", id), b.Step("z", id)
		b.Edge(s, x)
		b.Edge(s, y)
		b.Edge(s, z)
		b.Join3("j", x, y, z, func(ctx context.Context, a, c, d int) (int, error) { return a + c + d, blockUntilDone(t, ctx, started) })
		flow, err := b.Build()
		if err != nil {
			t.Fatal(err)
		}
		expectCancelled(t, started, func(ctx context.Context) error {
			_, err := flow.Run(ctx, agent.NewMemStore(), "r", 1)
			return err
		})
	})
}

// The same holds for blocks registered for a loaded flow: RegisterStep, RegisterJoin2 and
// RegisterJoin3 bodies receive the ctx the loaded flow runs with.
func TestRegisteredBodiesReceiveRunContext(t *testing.T) {
	id := func(_ context.Context, n int) (int, error) { return n, nil }
	load := func(t *testing.T, reg *Registry, cfg string) *Flow[int, int] {
		t.Helper()
		flow, err := Load[int, int]([]byte(cfg), reg)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return flow
	}
	run := func(flow *Flow[int, int]) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := flow.Run(ctx, agent.NewMemStore(), "r", 1)
			return err
		}
	}
	t.Run("RegisterStep", func(t *testing.T) {
		started := make(chan struct{})
		reg := NewRegistry()
		if err := RegisterStep(reg, "s", func(ctx context.Context, n int) (int, error) { return n, blockUntilDone(t, ctx, started) }); err != nil {
			t.Fatal(err)
		}
		flow := load(t, reg, `{"flow":"reg-step-ctx","in":"int","out":"int","entry":"s","nodes":[{"name":"s","block":"s"}],"wiring":[]}`)
		expectCancelled(t, started, run(flow))
	})
	t.Run("RegisterJoin2", func(t *testing.T) {
		started := make(chan struct{})
		reg := NewRegistry()
		for _, n := range []string{"s", "x", "y"} {
			if err := RegisterStep(reg, n, id); err != nil {
				t.Fatal(err)
			}
		}
		if err := RegisterJoin2(reg, "m", func(ctx context.Context, a, c int) (int, error) { return a + c, blockUntilDone(t, ctx, started) }); err != nil {
			t.Fatal(err)
		}
		flow := load(t, reg, `{"flow":"reg-join2-ctx","in":"int","out":"int","entry":"s",
			"nodes":[{"name":"s","block":"s"},{"name":"x","block":"x"},{"name":"y","block":"y"}],
			"wiring":[{"edge":["s","x"]},{"edge":["s","y"]},{"join":"j","inputs":["x","y"],"merge":"m"}]}`)
		expectCancelled(t, started, run(flow))
	})
	t.Run("RegisterJoin3", func(t *testing.T) {
		started := make(chan struct{})
		reg := NewRegistry()
		for _, n := range []string{"s", "x", "y", "z"} {
			if err := RegisterStep(reg, n, id); err != nil {
				t.Fatal(err)
			}
		}
		if err := RegisterJoin3(reg, "m", func(ctx context.Context, a, c, d int) (int, error) { return a + c + d, blockUntilDone(t, ctx, started) }); err != nil {
			t.Fatal(err)
		}
		flow := load(t, reg, `{"flow":"reg-join3-ctx","in":"int","out":"int","entry":"s",
			"nodes":[{"name":"s","block":"s"},{"name":"x","block":"x"},{"name":"y","block":"y"},{"name":"z","block":"z"}],
			"wiring":[{"edge":["s","x"]},{"edge":["s","y"]},{"edge":["s","z"]},{"join":"j","inputs":["x","y","z"],"merge":"m"}]}`)
		expectCancelled(t, started, run(flow))
	})
}
