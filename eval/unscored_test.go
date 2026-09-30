package eval_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/eval"
)

var errJudgeDown = fmt.Errorf("judge provider unavailable: %w", agent.ErrModel)

// outageJudge replies PASS, except that every call from the failFrom-th on fails, as a judge
// provider does during an outage.
type outageJudge struct {
	calls    atomic.Int32
	failFrom int32
}

func (m *outageJudge) Stream(ctx context.Context, req agent.Request) (*agent.Stream, error) {
	if m.calls.Add(1) >= m.failFrom {
		return nil, errJudgeDown
	}
	return judgeModel{"PASS"}.Stream(ctx, req)
}

// A judge outage is not the agent failing. Runs the judge could not grade are left out of the
// judge metric's pass rate: the agent's output passed every time it was graded.
func TestJudge_OutageIsNotAFail(t *testing.T) {
	judge := &outageJudge{failFrom: 3}
	rep := mustRun(t, context.Background(), answer("some answer"), []eval.Case{{Name: "c", Input: "in"}},
		[]eval.Metric{eval.Judge("rubric", judge, "is it fine")}, eval.Options{Runs: 4, Concurrency: 1})
	if got := rep.Overall["rubric"].Rate; got != 1 {
		t.Fatalf("rate = %v, want 1: the two runs the judge graded passed, and the outage graded none", got)
	}
}
