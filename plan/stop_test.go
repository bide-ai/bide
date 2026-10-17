package plan_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/plan"
)

func stopRig(t *testing.T) (*agent.Journal, *agent.StopControl, *agent.StopSwitch) {
	t.Helper()
	j := agenttest.MustJournal(agent.NewMemStore())
	ctl, err := agent.NewStopControl(j, agent.WithStopMode(agent.StopBasic))
	if err != nil {
		t.Fatal(err)
	}
	sw, err := agent.NewStopSwitch(ctl, agent.WithStopPoll(10*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	return j, ctl, sw
}

func stopFlow(t *testing.T, name string, fired *atomic.Int64, sw *agent.StopSwitch) *plan.Flow[int, int] {
	t.Helper()
	b := plan.New[int, int](name)
	a := b.Step("charge", func(_ context.Context, n int) (int, error) { fired.Add(1); return n + 1, nil })
	c := b.Step("lookup", func(_ context.Context, n int) (int, error) { return n * 10, nil }, plan.ReadOnly())
	b.Edge(a, c)
	if sw != nil {
		b.WithStopSwitch(sw)
	}
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func wantStopped(t *testing.T, err error) *agent.Stopped {
	t.Helper()
	var st *agent.Stopped
	if !errors.As(err, &st) || !agent.IsPause(err) {
		t.Fatalf("err = %v, want a *agent.Stopped pause", err)
	}
	return st
}

// A flow-scope stop holds a run of that flow before any node runs, a stop on another flow does not, and
// after the lift the run carries on from where it was.
func TestStop_FlowScopeHoldsTheFlow(t *testing.T) {
	ctx := context.Background()
	j, ctl, sw := stopRig(t)
	var fired atomic.Int64
	flow := stopFlow(t, "payout", &fired, sw)
	other, err := ctl.SetStop(ctx, agent.StopRequest{Scope: agent.StopScope{Kind: agent.StopFlow, Name: "other"}, By: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := flow.Run(ctx, j, "r0", 1); err != nil || out != 20 {
		t.Fatalf("a stop on another flow: %d %v", out, err)
	}
	e, err := ctl.SetStop(ctx, agent.StopRequest{Scope: agent.StopScope{Kind: agent.StopFlow, Name: "payout"}, By: "a", Reason: "incident"})
	if err != nil {
		t.Fatal(err)
	}
	before := fired.Load()
	time.Sleep(15 * time.Millisecond)
	_, err = flow.Run(ctx, j, "r1", 1)
	st := wantStopped(t, err)
	if st.StopID != e.ID || st.Scope.Name != "payout" || fired.Load() != before {
		t.Fatalf("%+v fired %d (was %d)", st, fired.Load(), before)
	}
	if _, err := ctl.LiftStop(ctx, e.ID, agent.LiftRequest{By: "a"}); err != nil {
		t.Fatal(err)
	}
	_ = other
	time.Sleep(15 * time.Millisecond)
	if out, err := flow.Run(ctx, j, "r1", 1); err != nil || out != 20 || fired.Load() != before+1 {
		t.Fatalf("after the lift: %d %v fired %d", out, err, fired.Load())
	}
}

// A stop that lands after a drive's start is seen by the node itself: a side-effect node reads the
// control run after its claim and, on a covering stop, is not run: its claim is recorded as not
// started, so a resume after the lift runs it once.
func TestStop_FlowNodeIsHeldAfterItsClaim(t *testing.T) {
	ctx := context.Background()
	j, ctl, sw := stopRig(t)
	var fired atomic.Int64
	b := plan.New[int, int]("payout")
	a := b.Step("prep", func(c context.Context, n int) (int, error) {
		// A stop set while the first node runs, after the drive's start check.
		if _, err := ctl.SetStop(c, agent.StopRequest{Scope: agent.StopScope{Kind: agent.StopFlow, Name: "payout"}, By: "a"}); err != nil {
			t.Error(err)
		}
		return n, nil
	}, plan.ReadOnly())
	c := b.Step("charge", func(_ context.Context, n int) (int, error) { fired.Add(1); return n + 1, nil })
	b.Edge(a, c)
	b.WithStopSwitch(sw)
	flow, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	_, err = flow.Run(ctx, j, "r1", 1)
	st := wantStopped(t, err)
	if fired.Load() != 0 {
		t.Fatalf("the node ran under a stop: %+v", st)
	}
	st2, _ := ctl.State(ctx)
	for _, e := range st2.Active() {
		if _, err := ctl.LiftStop(ctx, e.ID, agent.LiftRequest{By: "a"}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(15 * time.Millisecond)
	// The first node's step is recorded; it does not set the stop again.
	if out, err := flow.Run(ctx, j, "r1", 1); err != nil || out != 2 || fired.Load() != 1 {
		t.Fatalf("after the lift: %d %v fired %d", out, err, fired.Load())
	}
}

// A flow with no switch on a store whose control run holds a record is refused, and a nil switch is
// refused at Build.
func TestStop_FlowWithoutASwitchIsRefused(t *testing.T) {
	ctx := context.Background()
	j, ctl, sw := stopRig(t)
	var fired atomic.Int64
	if out, err := stopFlow(t, "x", &fired, nil).Run(ctx, agenttest.MustJournal(agent.NewMemStore()), "ok", 1); err != nil || out != 20 {
		t.Fatalf("a store with no control run: %d %v", out, err)
	}
	if _, err := ctl.SetStop(ctx, agent.StopRequest{Scope: agent.StopScope{Kind: agent.StopTool, Name: "t"}, By: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := stopFlow(t, "y", &fired, nil).Run(ctx, j, "r2", 1); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("a flow without a switch on a store with a control run: %v", err)
	}
	if out, err := stopFlow(t, "z", &fired, sw).Run(ctx, j, "r3", 1); err != nil || out != 20 {
		t.Fatalf("with a switch: %d %v", out, err)
	}
	b := plan.New[int, int]("n")
	b.Step("a", func(_ context.Context, n int) (int, error) { return n, nil })
	b.WithStopSwitch(nil)
	if _, err := b.Build(); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("a nil switch: %v", err)
	}
}
