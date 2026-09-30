package eval_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/eval"
)

// reportOver runs cases whose input is the answer, scored by graded, so a case's input fixes its
// result ("PASS", "FAIL" or "SKIP").
func reportOver(t *testing.T, runs int, cases ...eval.Case) eval.Report {
	t.Helper()
	return mustRun(t, context.Background(), echoInput, cases, []eval.Metric{graded}, eval.Options{Runs: runs})
}

func echoInput(_ context.Context, input string) eval.RunOutput {
	return eval.RunOutput{Final: agent.UserText(input)}
}

// Two reports over different case sets do not measure the same thing: a rate that moved may only
// mean the cases changed. By default every metric is inconclusive, naming the mismatch, and the
// gate fails, even when the rates look identical.
func TestCompare_CaseSetMismatchIsInconclusive(t *testing.T) {
	old := reportOver(t, 5, eval.Case{Name: "a", Input: "PASS"}, eval.Case{Name: "b", Input: "PASS"})
	new := reportOver(t, 5, eval.Case{Name: "a", Input: "PASS"}, eval.Case{Name: "c", Input: "PASS"})
	cmp := mustCompare(t, old, new)
	if len(cmp.Metrics) != 1 {
		t.Fatalf("comparison = %+v, want one metric", cmp.Metrics)
	}
	m := cmp.Metrics[0]
	if m.Direction != eval.DirectionInconclusive || m.Significant || !strings.Contains(m.Note, "case sets differ") {
		t.Fatalf("comparison = %+v, want inconclusive with a note that the case sets differ", m)
	}
	if err := cmp.Gate(); !errors.Is(err, eval.ErrInconclusive) {
		t.Fatalf("Gate() = %v, want ErrInconclusive", err)
	}
}
