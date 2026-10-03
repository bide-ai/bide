package eval_test

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/eval"
)

func answer(text string) eval.RunFunc {
	return func(context.Context, string) eval.RunOutput { return eval.RunOutput{Final: agent.UserText(text)} }
}

// A misconfigured metric or runner is a configuration error from Run, before any execution, and
// never a panic: a nil regexp, predicate, model or Fn, an empty or repeated name, a nil RunFunc.
func TestRun_MisconfigurationIsErrConfig(t *testing.T) {
	ok := eval.NoError()
	for _, tc := range []struct {
		name    string
		nilRun  bool
		metrics []eval.Metric
		want    string
	}{
		{"nil regexp", false, []eval.Metric{eval.Matches(nil)}, "nil regexp"},
		{"nil custom predicate", false, []eval.Metric{eval.Custom("c", nil)}, "nil predicate"},
		{"nil governance predicate", false, []eval.Metric{eval.GovernanceHeld("g", nil)}, "nil predicate"},
		{"nil judge model", false, []eval.Metric{eval.Judge("j", nil, "rubric")}, "nil model"},
		{"nil Fn", false, []eval.Metric{{Name: "bare"}}, "nil Fn"},
		{"zero metric", false, []eval.Metric{{}}, "nil Fn"},
		{"empty name", false, []eval.Metric{{Fn: ok.Fn}}, "empty name"},
		{"repeated name", false, []eval.Metric{ok, ok}, "two metrics are named"},
		{"bad metric after a good one", false, []eval.Metric{ok, eval.Matches(nil)}, "nil regexp"},
		{"nil RunFunc", true, []eval.Metric{ok}, "nil RunFunc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			run := func(context.Context, string) eval.RunOutput {
				calls.Add(1)
				return eval.RunOutput{}
			}
			if tc.nilRun {
				run = nil
			}
			rep, err := eval.Run(context.Background(), run, []eval.Case{{Name: "c", Input: "x"}}, tc.metrics, eval.Options{Runs: 2})
			if !errors.Is(err, agent.ErrConfig) {
				t.Fatalf("err = %v, want agent.ErrConfig", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to mention %q", err, tc.want)
			}
			if calls.Load() != 0 || rep.TotalRuns != 0 || rep.Format != "" {
				t.Errorf("a misconfigured Run executed %d runs and returned %+v", calls.Load(), rep)
			}
		})
	}
}

// Matches takes a compiled regexp: the caller owns compilation, the metric is named after the
// pattern, and a run that errored does not pass it.
func TestMatches(t *testing.T) {
	m := eval.Matches(regexp.MustCompile(`^order #\d+ (approved|held)$`))
	if m.Name != `matches:^order #\d+ (approved|held)$` {
		t.Fatalf("name = %q", m.Name)
	}
	ctx := context.Background()
	for text, want := range map[string]bool{
		"order #12 approved":  true,
		"order #7 held":       true,
		"order #x approved":   false,
		"order #12 approved!": false,
	} {
		if got, err := m.Fn(ctx, eval.Case{}, eval.RunOutput{Final: agent.UserText(text)}); got != want || err != nil {
			t.Errorf("Matches(%q) = %v, want %v", text, got, want)
		}
	}
	if pass, err := m.Fn(ctx, eval.Case{}, eval.RunOutput{Final: agent.UserText("order #1 held"), Err: errors.New("boom")}); pass || err != nil {
		t.Error("Matches passed a run that errored")
	}
}

type ctxKey struct{}

// GovernanceHeld's predicate receives the evaluation's context, as every other metric's Fn does.
func TestGovernanceHeld_ReceivesContext(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxKey{}, "eval-ctx")
	var seen atomic.Int32
	held := eval.GovernanceHeld("held", func(ctx context.Context, _ eval.RunOutput) (bool, error) {
		if ctx.Value(ctxKey{}) == "eval-ctx" {
			seen.Add(1)
			return true, nil
		}
		return false, nil
	})
	rep := mustRun(t, ctx, answer("ok"), []eval.Case{{Name: "c", Input: "x"}}, []eval.Metric{held}, eval.Options{Runs: 3})
	if seen.Load() != 3 || rep.Overall["held"].Passes != 3 {
		t.Fatalf("predicate saw the eval context %d of 3 times; stat %+v", seen.Load(), rep.Overall["held"])
	}
}

// AgentRunner refuses a nil agent or store with ErrConfig instead of returning a RunFunc that
// panics on its first call.
func TestAgentRunner_NilArgumentsAreErrConfig(t *testing.T) {
	store := agenttest.MemJournal()
	a := agenttest.MustNew(&echoModel{}, store)
	if run, err := eval.AgentRunner(nil, store, "p"); !errors.Is(err, agent.ErrConfig) || run != nil {
		t.Errorf("nil agent: run=%v err=%v, want nil and ErrConfig", run != nil, err)
	}
	if run, err := eval.AgentRunner(a, nil, "p"); !errors.Is(err, agent.ErrConfig) || run != nil {
		t.Errorf("nil store: run=%v err=%v, want nil and ErrConfig", run != nil, err)
	}
}

// Every Report that Run returns carries the format tag, and it is the first field of its JSON.
func TestReport_Format(t *testing.T) {
	if eval.ReportFormat != "bide.eval.report.v2" {
		t.Fatalf("ReportFormat = %q", eval.ReportFormat)
	}
	rep := mustRun(t, context.Background(), answer("ok"), []eval.Case{{Name: "c", Input: "x"}}, []eval.Metric{eval.NoError()}, eval.Options{})
	if rep.Format != eval.ReportFormat {
		t.Fatalf("Format = %q, want %q", rep.Format, eval.ReportFormat)
	}
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), `{"format":"bide.eval.report.v2",`) {
		t.Fatalf("report JSON does not lead with its format: %s", data)
	}
	var back eval.Report
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if _, err := eval.Compare(back, rep); err != nil {
		t.Fatalf("a report read back from its JSON does not compare: %v", err)
	}
}

// Compare refuses a report of another format, or one with none (a zero Report, or JSON of another
// layout decoded into this type), instead of comparing it as if it had no metrics.
func TestCompare_RefusesUnknownFormat(t *testing.T) {
	good := reportFor(9, 10)
	// A v1 report: its stats count runs, not scored runs, so read as v2 its rates and counts
	// would not mean what Compare takes them to.
	var v1 eval.Report
	if err := json.Unmarshal([]byte(`{"format":"bide.eval.report.v1","overall":{"contains:PASS":{"passes":9,"runs":10,"rate":0.9}}}`), &v1); err != nil {
		t.Fatal(err)
	}
	var v3 eval.Report
	if err := json.Unmarshal([]byte(`{"format":"bide.eval.report.v3","overall_stats":{}}`), &v3); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]eval.Report{
		"zero":  {},
		"v1":    v1,
		"v3":    v3,
		"blank": {Format: "", Overall: good.Overall},
	} {
		for _, pair := range [][2]eval.Report{{bad, good}, {good, bad}} {
			cmp, err := eval.Compare(pair[0], pair[1])
			if !errors.Is(err, eval.ErrFormat) || !errors.Is(err, agent.ErrProtocol) {
				t.Errorf("%s: err = %v, want ErrFormat wrapping agent.ErrProtocol", name, err)
			}
			if len(cmp.Metrics) != 0 {
				t.Errorf("%s: compared %d metrics of a report it cannot read", name, len(cmp.Metrics))
			}
		}
	}
}

// MetricDirection is a typed closed set whose JSON values are the documented strings.
func TestMetricDirection_Values(t *testing.T) {
	for d, want := range map[eval.MetricDirection]string{
		eval.DirectionRegression:  "regression",
		eval.DirectionImprovement: "improvement",
		eval.DirectionFlat:        "flat",
	} {
		data, err := json.Marshal(eval.MetricComparison{Direction: d})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), `"direction":"`+want+`"`) {
			t.Errorf("%v marshals as %s, want direction %q", d, data, want)
		}
	}
	cmp := mustCompare(t, reportFor(10, 100), reportFor(60, 100))
	if cmp.Metrics[0].Direction != eval.DirectionImprovement {
		t.Fatalf("10/100 -> 60/100 direction = %q, want %q", cmp.Metrics[0].Direction, eval.DirectionImprovement)
	}
}
