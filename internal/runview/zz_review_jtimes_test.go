package runview

import (
	"context"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// REVIEW (low/medium): an unsigned run:complete stamp from a drive whose clock is behind the run's
// start (a resume on another host) makes span <= 0, so timeline() draws no bar for ANY event.
func TestReview_SkewedEndStampKeepsWaterfall(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	refund := agent.MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithApproval(agent.SingleApproval()))
	drive := func(at time.Time) error {
		m := agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{}`), agenttest.TextTurn("done"))
		_, err := agenttest.MustNew(m, j, agent.WithTools(refund), agent.WithClock(clockAt(at))).Run(ctx, "r", agent.UserText("go"))
		return err
	}
	t0 := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	if err := drive(t0); err == nil {
		t.Fatal("want pause")
	}
	if err := agent.Approve(ctx, j, "r", "c1", true, agent.WithClock(clockAt(t0.Add(time.Minute)))); err != nil {
		t.Fatal(err)
	}
	if err := drive(t0.Add(-time.Hour)); err != nil { // resumed on a host whose clock is an hour behind
		t.Fatal(err)
	}
	v := viewOfJournal(t, j, "r")
	n := 0
	for _, e := range v.Events {
		if e.Bar != nil {
			n++
			if e.Bar.X < 0 || e.Bar.W <= 0 || e.Bar.X+e.Bar.W > 100.0001 {
				t.Errorf("bar out of range: %+v", *e.Bar)
			}
		}
	}
	if n == 0 {
		t.Errorf("no timeline bar at all: one skewed end stamp erased the waterfall of %d events", len(v.Events))
	}
}
