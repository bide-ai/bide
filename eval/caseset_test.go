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

// Each CaseReport carries its case's own hash.
func TestCaseReport_Hash(t *testing.T) {
	a := eval.Case{Name: "a", Input: "PASS", Want: 1}
	b := eval.Case{Name: "b", Input: "FAIL"}
	rep := reportOver(t, 1, a, b)
	if rep.Cases[0].Hash != eval.HashCases([]eval.Case{a}) || rep.Cases[1].Hash != eval.HashCases([]eval.Case{b}) {
		t.Fatalf("case hashes = %q, %q", rep.Cases[0].Hash, rep.Cases[1].Hash)
	}
	if rep.Cases[0].Hash == rep.Cases[1].Hash {
		t.Fatal("two different cases share a hash")
	}
}

// Allowed explicitly, a mismatch is compared over the shared cases only: each report's stats are
// recomputed from the cases both ran, and the note says so.
func TestCompare_CaseSetMismatchAllowedUsesSharedCases(t *testing.T) {
	shared := eval.Case{Name: "a", Input: "PASS"}
	old := reportOver(t, 4, shared, eval.Case{Name: "b", Input: "FAIL"}, eval.Case{Name: "s", Input: "SKIP"})
	new := reportOver(t, 3, shared, eval.Case{Name: "c", Input: "FAIL"})
	cmp, err := eval.Compare(old, new, eval.WithCaseSetMismatchAllowed())
	if err != nil {
		t.Fatal(err)
	}
	m := cmp.Metrics[0]
	if m.Direction != eval.DirectionFlat || m.OldPasses != 4 || m.OldScored != 4 || m.OldUnscored != 0 ||
		m.NewPasses != 3 || m.NewScored != 3 || m.NewUnscored != 0 || m.OldRate != 1 || m.NewRate != 1 {
		t.Fatalf("comparison = %+v, want case a alone: 4/4 against 3/3, flat", m)
	}
	if !strings.Contains(m.Note, "case sets differ") || !strings.Contains(m.Note, "1 shared case") {
		t.Fatalf("note = %q, want the mismatch and the shared-case scope", m.Note)
	}
	if err := cmp.Gate(); err != nil {
		t.Fatalf("Gate() = %v, want nil: the shared case did not move", err)
	}
	// Over the full sets the rates differ (4/8 against 3/6); over the shared case they do not.
	if old.Overall["graded"].Rate == 1 {
		t.Fatal("test setup: the old report's overall rate should include its failing case")
	}
}

// A shared case's unscored runs carry into the recomputed stats, so the tolerance still applies.
func TestCompare_SharedCasesKeepUnscored(t *testing.T) {
	shared := eval.Case{Name: "s", Input: "SKIP"}
	old := reportOver(t, 2, shared, eval.Case{Name: "p", Input: "PASS"})
	new := reportOver(t, 2, shared, eval.Case{Name: "q", Input: "PASS"})
	cmp, err := eval.Compare(old, new, eval.WithCaseSetMismatchAllowed(), eval.WithUnscoredTolerance(1))
	if err != nil {
		t.Fatal(err)
	}
	if m := cmp.Metrics[0]; m.Direction != eval.DirectionInconclusive || m.OldUnscored != 2 || m.NewUnscored != 2 ||
		!strings.Contains(m.Note, "no scored runs") {
		t.Fatalf("comparison = %+v, want inconclusive: the shared case was never scored", m)
	}
}

// A case whose input changed under the same name is a different case: it is not shared.
func TestCompare_ChangedCaseIsNotShared(t *testing.T) {
	old := reportOver(t, 3, eval.Case{Name: "a", Input: "PASS"})
	new := reportOver(t, 3, eval.Case{Name: "a", Input: "FAIL"})
	cmp, err := eval.Compare(old, new, eval.WithCaseSetMismatchAllowed())
	if err != nil {
		t.Fatal(err)
	}
	if m := cmp.Metrics[0]; m.Direction != eval.DirectionInconclusive || !strings.Contains(m.Note, "share no case") {
		t.Fatalf("comparison = %+v, want inconclusive: no case is shared", m)
	}
	if err := cmp.Gate(); !errors.Is(err, eval.ErrInconclusive) {
		t.Fatalf("Gate() = %v, want ErrInconclusive", err)
	}
}

// The case-set hash is in order, so the same cases reordered are a mismatch by default; allowed,
// every case is shared.
func TestCompare_ReorderedCases(t *testing.T) {
	a, b := eval.Case{Name: "a", Input: "PASS"}, eval.Case{Name: "b", Input: "FAIL"}
	old, new := reportOver(t, 3, a, b), reportOver(t, 3, b, a)
	if m := mustCompare(t, old, new).Metrics[0]; m.Direction != eval.DirectionInconclusive {
		t.Fatalf("comparison = %+v, want inconclusive by default", m)
	}
	cmp, err := eval.Compare(old, new, eval.WithCaseSetMismatchAllowed())
	if err != nil {
		t.Fatal(err)
	}
	if m := cmp.Metrics[0]; m.Direction != eval.DirectionFlat || m.OldScored != 6 || m.NewScored != 6 || !strings.Contains(m.Note, "2 shared case") {
		t.Fatalf("comparison = %+v, want both cases shared", m)
	}
}

// A report without a case-set hash (not produced by Run) cannot show it ran the same cases.
func TestCompare_MissingCaseSetHashIsInconclusive(t *testing.T) {
	old, new := reportFor(9, 10), reportFor(9, 10)
	old.Provenance.CaseSetHash, new.Provenance.CaseSetHash = "", ""
	if m := mustCompare(t, old, new).Metrics[0]; m.Direction != eval.DirectionInconclusive || !strings.Contains(m.Note, "old none, new none") {
		t.Fatalf("comparison = %+v, want inconclusive naming the missing hashes", m)
	}
}

// A case repeated within a report is one shared case, its runs summed.
func TestCompare_RepeatedCaseSummed(t *testing.T) {
	a := eval.Case{Name: "a", Input: "PASS"}
	old := reportOver(t, 2, a, a)
	new := reportOver(t, 2, a, eval.Case{Name: "b", Input: "PASS"})
	cmp, err := eval.Compare(old, new, eval.WithCaseSetMismatchAllowed())
	if err != nil {
		t.Fatal(err)
	}
	if m := cmp.Metrics[0]; m.OldScored != 4 || m.NewScored != 2 || !strings.Contains(m.Note, "1 shared case") {
		t.Fatalf("comparison = %+v, want the repeated case's 4 runs against 2, one shared case", m)
	}
}

// A case without a hash (a report not produced by Run) is never shared, even when both reports have
// one: an empty hash identifies nothing.
func TestCompare_UnhashedCasesAreNotShared(t *testing.T) {
	old := reportOver(t, 2, eval.Case{Name: "a", Input: "PASS"})
	new := reportOver(t, 2, eval.Case{Name: "b", Input: "PASS"})
	old.Cases[0].Hash, new.Cases[0].Hash = "", ""
	cmp, err := eval.Compare(old, new, eval.WithCaseSetMismatchAllowed())
	if err != nil {
		t.Fatal(err)
	}
	if m := cmp.Metrics[0]; m.Direction != eval.DirectionInconclusive || !strings.Contains(m.Note, "share no case") {
		t.Fatalf("comparison = %+v, want inconclusive: unhashed cases are not shared", m)
	}
}
