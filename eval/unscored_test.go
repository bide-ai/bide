package eval_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
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

var errCannotGrade = errors.New("grader unavailable")

// graded scores "PASS" as a pass and "FAIL" as a fail, and cannot score "SKIP". It returns true
// with its error on "SKIP", so a harness that read the boolean of an error would count a pass.
var graded = eval.Custom("graded", func(_ context.Context, _ eval.Case, out eval.RunOutput) (bool, error) {
	switch out.Final.Text() {
	case "PASS":
		return true, nil
	case "SKIP":
		return true, errCannotGrade
	}
	return false, nil
})

// sequence is a RunFunc answering texts in order, one per execution (run with Concurrency 1).
func sequence(texts ...string) eval.RunFunc {
	var n atomic.Int32
	return func(context.Context, string) eval.RunOutput {
		return eval.RunOutput{Final: agent.UserText(texts[int(n.Add(1)-1)%len(texts)])}
	}
}

// reportWith builds a report whose "graded" metric has the given passes, fails and unscored runs,
// over one case.
func reportWith(t *testing.T, passes, fails, unscored int) eval.Report {
	t.Helper()
	var texts []string
	for range passes {
		texts = append(texts, "PASS")
	}
	for range fails {
		texts = append(texts, "FAIL")
	}
	for range unscored {
		texts = append(texts, "SKIP")
	}
	return mustRun(t, context.Background(), sequence(texts...), []eval.Case{{Name: "c", Input: "x"}},
		[]eval.Metric{graded}, eval.Options{Runs: len(texts), Concurrency: 1})
}

// An unscored run is counted apart and moves neither the pass rate nor its Wilson interval: the
// stat over 3 passes, 1 fail and 2 unscored equals the stat over 3 passes and 1 fail, plus the
// unscored count. The boolean a metric returns with its error is ignored.
func TestRun_UnscoredLeftOutOfRateAndInterval(t *testing.T) {
	got := reportWith(t, 3, 1, 2).Overall["graded"]
	want := reportWith(t, 3, 1, 0).Overall["graded"]
	want.Unscored = 2
	if got != want {
		t.Fatalf("stat = %+v, want %+v", got, want)
	}
	if got.Passes != 3 || got.Scored != 4 || got.Rate != 0.75 {
		t.Fatalf("stat = %+v, want 3 passes of 4 scored", got)
	}
}

// With no scored run a metric has no evidence: rate 0 and the interval [0, 1].
func TestRun_AllUnscored(t *testing.T) {
	got := reportWith(t, 0, 0, 3).Overall["graded"]
	if got != (eval.MetricStat{Unscored: 3, CILow: 0, CIHigh: 1}) {
		t.Fatalf("stat = %+v, want 3 unscored, no scored, interval [0, 1]", got)
	}
}

// Unscored runs are counted per case, per tag, and overall.
func TestRun_UnscoredPerCaseAndTag(t *testing.T) {
	run := func(_ context.Context, input string) eval.RunOutput {
		return eval.RunOutput{Final: agent.UserText(input)}
	}
	cases := []eval.Case{
		{Name: "p", Input: "PASS", Tags: []string{"easy"}},
		{Name: "s", Input: "SKIP", Tags: []string{"hard", "easy"}},
		{Name: "f", Input: "FAIL", Tags: []string{"hard"}},
	}
	rep := mustRun(t, context.Background(), run, cases, []eval.Metric{graded}, eval.Options{Runs: 2})
	check := func(where string, s eval.MetricStat, passes, scored, unscored int) {
		t.Helper()
		if s.Passes != passes || s.Scored != scored || s.Unscored != unscored {
			t.Errorf("%s = %+v, want %d passes, %d scored, %d unscored", where, s, passes, scored, unscored)
		}
	}
	check("case p", rep.Cases[0].Metrics["graded"], 2, 2, 0)
	check("case s", rep.Cases[1].Metrics["graded"], 0, 0, 2)
	check("case f", rep.Cases[2].Metrics["graded"], 0, 2, 0)
	check("tag easy", rep.ByTag["easy"]["graded"], 2, 2, 2)
	check("tag hard", rep.ByTag["hard"]["graded"], 0, 2, 2)
	check("overall", rep.Overall["graded"], 2, 4, 2)
	if !strings.Contains(rep.String(), "unscored=2") {
		t.Errorf("report text does not show the unscored runs:\n%s", rep)
	}
}

// GovernanceHeld's predicate error leaves the run unscored.
func TestGovernanceHeld_ErrorIsUnscored(t *testing.T) {
	held := eval.GovernanceHeld("held", func(_ context.Context, out eval.RunOutput) (bool, error) {
		if out.Final.Text() == "SKIP" {
			return true, errCannotGrade
		}
		return out.Final.Text() == "PASS", nil
	})
	rep := mustRun(t, context.Background(), sequence("PASS", "SKIP", "FAIL"), []eval.Case{{Name: "c", Input: "x"}},
		[]eval.Metric{held}, eval.Options{Runs: 3, Concurrency: 1})
	if s := rep.Overall["held"]; s.Passes != 1 || s.Scored != 2 || s.Unscored != 1 {
		t.Fatalf("stat = %+v, want 1 pass of 2 scored, 1 unscored", s)
	}
}

// The judge's own failure is unscored; a failed run and any reply but PASS are scored fails, so an
// output cannot talk the judge out of grading it.
func TestJudge_OnlyAJudgeCallFailureIsUnscored(t *testing.T) {
	ctx := context.Background()
	c := eval.Case{Input: "in"}
	answer := eval.RunOutput{Final: agent.UserText("some answer")}

	down := eval.Judge("rubric", &outageJudge{failFrom: 1}, "is it fine")
	if pass, err := down.Fn(ctx, c, answer); pass || !errors.Is(err, errJudgeDown) || !errors.Is(err, agent.ErrModel) {
		t.Errorf("judge down: (%v, %v), want unscored with the judge's error", pass, err)
	}
	failedRun := eval.RunOutput{Final: agent.UserText("x"), Err: errors.New("boom")}
	if pass, err := down.Fn(ctx, c, failedRun); pass || err != nil {
		t.Errorf("failed run: (%v, %v), want a scored fail (the judge is not asked)", pass, err)
	}
	for _, reply := range []string{"FAIL", "I cannot grade this", "", "PASS!"} {
		if pass, err := eval.Judge("rubric", judgeModel{reply}, "r").Fn(ctx, c, answer); pass || err != nil {
			t.Errorf("reply %q: (%v, %v), want a scored fail", reply, pass, err)
		}
	}
}

// The report JSON carries the scored and unscored counts.
func TestReport_UnscoredJSON(t *testing.T) {
	data, err := json.Marshal(reportWith(t, 1, 0, 1).Overall["graded"])
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); !strings.Contains(got, `"passes":1,"scored":1,"unscored":1,`) {
		t.Fatalf("stat JSON = %s", got)
	}
	// Zero unscored is stated, not omitted: it is what makes a rate comparable.
	data, err = json.Marshal(reportWith(t, 1, 0, 0).Overall["graded"])
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); !strings.Contains(got, `"unscored":0,`) {
		t.Fatalf("stat JSON = %s", got)
	}
}

// By default any unscored run makes a metric inconclusive, never a pass: no test is run, it is left
// out of the Benjamini-Hochberg family, and Gate reports it.
func TestCompare_UnscoredIsInconclusiveByDefault(t *testing.T) {
	old := reportWith(t, 9, 1, 0)
	new := reportWith(t, 9, 1, 1)
	cmp := mustCompare(t, old, new)
	m := cmp.Metrics[0]
	if m.Direction != eval.DirectionInconclusive || m.Significant || m.PValue != 1 || m.AdjustedP != 1 || m.RateDelta != 0 {
		t.Fatalf("comparison = %+v, want inconclusive with no test", m)
	}
	if m.NewUnscored != 1 || m.NewScored != 10 || m.OldScored != 10 || m.OldUnscored != 0 || !strings.Contains(m.Note, "tolerance") {
		t.Fatalf("comparison = %+v, want the counts and a note on the tolerance", m)
	}
	if err := cmp.Gate(); !errors.Is(err, eval.ErrInconclusive) || errors.Is(err, eval.ErrRegression) {
		t.Fatalf("Gate() = %v, want ErrInconclusive", err)
	}
	if !strings.Contains(cmp.String(), "inconclusive: "+m.Note) {
		t.Errorf("comparison text does not say inconclusive:\n%s", cmp)
	}
}

// The tolerance admits a metric whose unscored share, in each report, is at most the tolerance, and
// the test then runs over the scored runs only.
func TestCompare_UnscoredTolerance(t *testing.T) {
	old := reportWith(t, 5, 1, 2) // 2 of 8 unscored: 0.25
	new := reportWith(t, 1, 5, 0)
	for tol, compared := range map[float64]bool{0: false, 0.24: false, 0.25: true, 1: true} {
		cmp, err := eval.Compare(old, new, eval.WithUnscoredTolerance(tol))
		if err != nil {
			t.Fatal(err)
		}
		m := cmp.Metrics[0]
		if got := m.Direction != eval.DirectionInconclusive; got != compared {
			t.Errorf("tolerance %g: compared = %v (%+v), want %v", tol, got, m, compared)
			continue
		}
		if compared && m.PValue != eval.FishersExactForTest(5, 1, 1, 5) {
			t.Errorf("tolerance %g: p = %v, want Fisher over the scored runs 5/6 vs 1/6 = %v", tol, m.PValue, eval.FishersExactForTest(5, 1, 1, 5))
		}
	}
}

// A metric with no scored run on either side is inconclusive whatever the tolerance.
func TestCompare_NoScoredRunsIsInconclusive(t *testing.T) {
	for _, pair := range [][2]eval.Report{
		{reportWith(t, 0, 0, 2), reportWith(t, 3, 0, 0)},
		{reportWith(t, 3, 0, 0), reportWith(t, 0, 0, 2)},
	} {
		cmp, err := eval.Compare(pair[0], pair[1], eval.WithUnscoredTolerance(1))
		if err != nil {
			t.Fatal(err)
		}
		if m := cmp.Metrics[0]; m.Direction != eval.DirectionInconclusive || !strings.Contains(m.Note, "no scored runs") {
			t.Errorf("comparison = %+v, want inconclusive for no scored runs", m)
		}
	}
}

// Inconclusive metrics stay out of the Benjamini-Hochberg family: a lone tested metric's adjusted
// p-value is its raw p-value, whatever else the reports hold.
func TestCompare_InconclusiveOutsideBHFamily(t *testing.T) {
	both := func(pPasses, pFails, gPasses, gUnscored int) eval.Report {
		var texts []string
		for i := range max(pPasses+pFails, gPasses+gUnscored) {
			a, b := "FAIL", "FAIL"
			if i < pPasses {
				a = "PASS"
			}
			if i < gPasses {
				b = "PASS"
			} else if i < gPasses+gUnscored {
				b = "SKIP"
			}
			texts = append(texts, a+" "+b)
		}
		first := eval.Custom("b_tested", func(_ context.Context, _ eval.Case, out eval.RunOutput) (bool, error) {
			return strings.HasPrefix(out.Final.Text(), "PASS "), nil
		})
		second := eval.Custom("a_unscored", func(ctx context.Context, c eval.Case, out eval.RunOutput) (bool, error) {
			_, b, _ := strings.Cut(out.Final.Text(), " ")
			return graded.Fn(ctx, c, eval.RunOutput{Final: agent.UserText(b)})
		})
		return mustRun(t, context.Background(), sequence(texts...), []eval.Case{{Name: "c", Input: "x"}},
			[]eval.Metric{first, second}, eval.Options{Runs: len(texts), Concurrency: 1})
	}
	cmp := mustCompare(t, both(18, 2, 10, 10), both(12, 8, 10, 0))
	if len(cmp.Metrics) != 2 || cmp.Metrics[0].Metric != "a_unscored" || cmp.Metrics[0].Direction != eval.DirectionInconclusive ||
		cmp.Metrics[0].AdjustedP != 1 || cmp.Metrics[1].Direction == eval.DirectionInconclusive {
		t.Fatalf("comparison = %+v, want a_unscored inconclusive (adjusted p 1) and b_tested tested", cmp.Metrics)
	}
	if m := cmp.Metrics[1]; m.AdjustedP != m.PValue {
		t.Fatalf("b_tested: adjusted p %v != raw p %v; the inconclusive metric joined the BH family", m.AdjustedP, m.PValue)
	}
}

// A metric present in only one report is inconclusive, not silently dropped: a renamed or removed
// metric must not let a gate pass.
func TestCompare_MissingMetricIsInconclusive(t *testing.T) {
	withName := func(name string) eval.Report {
		return mustRun(t, context.Background(), answer("ok"), []eval.Case{{Name: "c", Input: "x"}},
			[]eval.Metric{eval.Custom(name, func(context.Context, eval.Case, eval.RunOutput) (bool, error) { return true, nil })},
			eval.Options{Runs: 5})
	}
	cmp := mustCompare(t, withName("accuracy"), withName("accuracy_v2"))
	if len(cmp.Metrics) != 2 {
		t.Fatalf("comparison = %+v, want both metrics listed", cmp.Metrics)
	}
	for _, m := range cmp.Metrics {
		want := map[string]string{"accuracy": "missing from the new report", "accuracy_v2": "missing from the old report"}[m.Metric]
		if m.Direction != eval.DirectionInconclusive || m.Note != want {
			t.Errorf("%s = %+v, want inconclusive, %q", m.Metric, m, want)
		}
	}
	if err := cmp.Gate(); !errors.Is(err, eval.ErrInconclusive) {
		t.Fatalf("Gate() = %v, want ErrInconclusive", err)
	}
}

// A bad tolerance or a nil option is a configuration error.
func TestCompare_BadOptionIsErrConfig(t *testing.T) {
	r := reportWith(t, 1, 0, 0)
	for _, opt := range []eval.CompareOption{eval.WithUnscoredTolerance(-0.01), eval.WithUnscoredTolerance(1.01), eval.WithUnscoredTolerance(math.NaN()), nil} {
		if _, err := eval.Compare(r, r, opt); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("option %v: err = %v, want agent.ErrConfig", opt, err)
		}
	}
}

// Gate passes only a comparison with at least one metric, all flat or improved.
func TestComparison_Gate(t *testing.T) {
	flat := eval.MetricComparison{Metric: "a", Direction: eval.DirectionFlat}
	up := eval.MetricComparison{Metric: "b", Direction: eval.DirectionImprovement}
	down := eval.MetricComparison{Metric: "c", Direction: eval.DirectionRegression}
	odd := eval.MetricComparison{Metric: "d", Direction: "sideways"}
	for _, tc := range []struct {
		metrics []eval.MetricComparison
		want    []error
	}{
		{[]eval.MetricComparison{flat, up}, nil},
		{nil, []error{eval.ErrInconclusive}},
		{[]eval.MetricComparison{flat, down}, []error{eval.ErrRegression}},
		{[]eval.MetricComparison{odd}, []error{eval.ErrInconclusive}},
		{[]eval.MetricComparison{down, odd}, []error{eval.ErrRegression, eval.ErrInconclusive}},
	} {
		err := eval.Comparison{Metrics: tc.metrics}.Gate()
		if (err == nil) != (tc.want == nil) {
			t.Errorf("%v: Gate() = %v, want %v", tc.metrics, err, tc.want)
		}
		for _, w := range tc.want {
			if !errors.Is(err, w) {
				t.Errorf("%v: Gate() = %v, want it to wrap %v", tc.metrics, err, w)
			}
		}
	}
	// End to end: a clear regression fails the gate.
	if err := mustCompare(t, reportFor(95, 100), reportFor(70, 100)).Gate(); !errors.Is(err, eval.ErrRegression) {
		t.Fatalf("95/100 -> 70/100: Gate() = %v, want ErrRegression", err)
	}
}
