package benchmarks

import (
	"context"

	"github.com/blackwell-systems/bide/chaos"
	"github.com/cloudwego/eino/compose"
)

// Eino (ByteDance) has checkpoint/interrupt/resume — but it is HITL-interrupt-driven, NOT
// automatic crash-resume: a checkpoint is written only when a node interrupts (and you
// resume it with ResumeWithData). There is no per-step checkpoint, so an UNPLANNED process
// crash has nothing to resume from — the run is lost and re-invoking re-runs everything.
// So for crash-safety eino sits with the no-durable-resume group. We model the crash by
// cancelling the run's context right after the side effect fires; "resume" is a fresh
// invocation, which re-fires.

// EinoGraph returns a chaos.System for an eino compose graph (start → charge → end).
func EinoGraph() chaos.System { return einoSys{} }

type einoSys struct{}

func (einoSys) Writes() int { return 2 }

func (einoSys) NewRun() chaos.Run { return &einoRun{fired: new(int)} }

type einoRun struct{ fired *int } // no crash-resume store — eino has none

func (r *einoRun) Fired() int { return *r.fired }

func (r *einoRun) Step(crashAt int) bool {
	ctx := context.Background()

	g := compose.NewGraph[string, string]()
	_ = g.AddLambdaNode("charge", compose.InvokableLambda(func(_ context.Context, in string) (string, error) {
		*r.fired++ // the non-idempotent side effect
		return in, nil
	}))
	_ = g.AddEdge(compose.START, "charge")
	_ = g.AddEdge("charge", compose.END)

	run, err := g.Compile(ctx)
	if err != nil {
		return false
	}
	_, _ = run.Invoke(ctx, "charge the customer") // charge fires (once per invocation)

	// eino has NO automatic crash checkpoint: a crash loses the whole run (nothing was
	// durably recorded), so the caller must re-invoke — which re-runs and re-charges. Any
	// crash point loses everything, so we model the crash at the step level; "resume" (the
	// next Step) is a fresh invocation.
	return crashAt > 0
}
