package eval_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/eval"
)

// An eval cancelled partway has not measured anything: executions cut short would score as
// failures, and the ones that finished are not a representative sample. Run returns the
// cancellation instead of a report, and starts no executions after it.
func TestRun_CancelledEvalIsAnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var started atomic.Int32
	run := func(ctx context.Context, _ string) eval.RunOutput {
		if started.Add(1) == 2 {
			cancel() // the caller gives up partway
		}
		if err := ctx.Err(); err != nil {
			return eval.RunOutput{Err: err}
		}
		return eval.RunOutput{Final: agent.Message{Role: agent.RoleAssistant, Parts: []agent.Part{agent.Text{Text: "ok"}}}}
	}
	rep, err := eval.Run(ctx, run, []eval.Case{{Name: "c", Input: "x"}}, []eval.Metric{eval.NoError()}, eval.Options{Runs: 10, Concurrency: 1})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled; report = %+v", err, rep.Overall)
	}
	if rep.TotalRuns != 0 || len(rep.Overall) != 0 {
		t.Fatalf("a cancelled eval returned a report: %+v", rep)
	}
	if n := started.Load(); n > 3 {
		t.Fatalf("%d executions started; want none started after the cancellation", n)
	}
}

// After a cancellation, Run starts no further executions, even when a concurrency slot is free at
// the same moment: a select between a free slot and a done context picks either at random, so the
// launcher must check the context again after taking a slot. Repeated because the race is random.
func TestRun_NoExecutionStartsAfterCancel(t *testing.T) {
	for iter := 0; iter < 20000; iter++ {
		ctx, cancel := context.WithCancel(context.Background())
		var started atomic.Int32
		run := func(ctx context.Context, _ string) eval.RunOutput {
			if started.Add(1) == 2 {
				cancel()
			}
			return eval.RunOutput{Err: ctx.Err()}
		}
		_, err := eval.Run(ctx, run, []eval.Case{{Name: "c", Input: "x"}}, []eval.Metric{eval.NoError()}, eval.Options{Runs: 50, Concurrency: 1})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("iteration %d: err = %v, want context.Canceled", iter, err)
		}
		if n := started.Load(); n != 2 {
			t.Fatalf("iteration %d: %d executions started; want 2 (none after the cancellation)", iter, n)
		}
		cancel()
	}
}
