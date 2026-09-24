// Package eval is a lightweight, statistical evaluation harness for agents. It answers "does the
// model decide well?" the only way a stochastic model can be answered: run labeled cases multiple
// times and report a pass-rate distribution, not a single verdict.
//
// Read the boundary carefully, it is the point of this package. eval measures the MODEL's judgment,
// statistically and best-effort; it is NOT a proof and a pass rate is not a guarantee. The provable
// parts of the system are separate and live elsewhere: the governed policy's convergence and
// invariant enforcement (see the govern package and the gsm proof) bound what the model can do for
// all inputs, and the audit package proves what it did. In short: eval quantifies the model;
// governance contains it; audit records it. Do not present an eval pass rate as a guarantee, and do
// not confuse it with the machine-checked guarantees the rest of the SDK provides.
//
// Because the model is stochastic, evaluate with Runs > 1 and pin the model version (and set
// temperature 0 for the most reproducible baseline, acknowledging even that is not perfectly
// deterministic). The report is a distribution over runs, by design.
package eval

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	agent "github.com/dayna/go-agents"
)

// Case is one evaluation input plus optional expectations that metrics interpret.
type Case struct {
	Name  string
	Input string
	Want  any
}

// RunOutput is the result of executing one case: the agent's final message and any error.
type RunOutput struct {
	Final agent.Message
	Err   error
}

// RunFunc executes one case input and returns its output. Wrap an agent with AgentRunner, or supply
// any function (useful for tests and non-agent baselines).
type RunFunc func(ctx context.Context, input string) RunOutput

// Metric scores a single run of a case as pass (true) or fail (false).
type Metric struct {
	Name string
	Fn   func(ctx context.Context, c Case, out RunOutput) bool
}

// NoError passes iff the run did not error.
func NoError() Metric {
	return Metric{Name: "no_error", Fn: func(_ context.Context, _ Case, out RunOutput) bool { return out.Err == nil }}
}

// Contains passes iff the final message text contains substr.
func Contains(substr string) Metric {
	return Metric{Name: "contains:" + substr, Fn: func(_ context.Context, _ Case, out RunOutput) bool {
		return out.Err == nil && strings.Contains(out.Final.Text(), substr)
	}}
}

// Matches passes iff the final message text matches the pattern.
func Matches(pattern string) Metric {
	re := regexp.MustCompile(pattern)
	return Metric{Name: "matches:" + pattern, Fn: func(_ context.Context, _ Case, out RunOutput) bool {
		return out.Err == nil && re.MatchString(out.Final.Text())
	}}
}

// Custom builds a metric from a name and a predicate.
func Custom(name string, fn func(ctx context.Context, c Case, out RunOutput) bool) Metric {
	return Metric{Name: name, Fn: fn}
}

// Judge is an LLM-as-judge metric: it asks a model whether the output satisfies rubric, passing iff
// the judge answers PASS. It is itself stochastic (a model grading a model), so treat it as a
// signal, run it with Runs > 1, and never read it as a verdict.
func Judge(name string, m agent.Model, rubric string) Metric {
	return Metric{Name: name, Fn: func(ctx context.Context, c Case, out RunOutput) bool {
		prompt := fmt.Sprintf(
			"You are grading an assistant's output against a rubric. Reply with exactly PASS or FAIL.\n\nRubric: %s\n\nInput: %s\n\nOutput: %s",
			rubric, c.Input, out.Final.Text())
		msg, _, err := agent.Generate(ctx, m, agent.Request{Messages: []agent.Message{agent.UserText(prompt)}})
		if err != nil {
			return false
		}
		return strings.HasPrefix(strings.TrimSpace(strings.ToUpper(msg.Text())), "PASS")
	}}
}

// Options configures a run. Runs is the number of executions per case (default 1); use more than
// one to sample a stochastic model. Concurrency caps in-flight executions (default 8, which is
// friendly to provider rate limits).
type Options struct {
	Runs        int
	Concurrency int
}

// MetricStat is a metric's pass count over some number of runs.
type MetricStat struct {
	Passes int     `json:"passes"`
	Runs   int     `json:"runs"`
	Rate   float64 `json:"rate"`
}

// CaseReport holds one case's per-metric stats over Runs executions.
type CaseReport struct {
	Name    string                `json:"name"`
	Metrics map[string]MetricStat `json:"metrics"`
}

// Report is the statistical result of an evaluation: per-case pass rates and an aggregate. It is
// JSON-marshalable for storage or inspection. It is a distribution, not a verdict.
type Report struct {
	RunsPerCase int                   `json:"runs_per_case"`
	Cases       []CaseReport          `json:"cases"`
	Overall     map[string]MetricStat `json:"overall"`
}

// Run executes each case Runs times through run, scores every execution with each metric, and
// returns a Report of pass rates. Executions run concurrently up to Options.Concurrency. Because
// the model is stochastic, the report is a pass-rate distribution over runs, not a single pass/fail.
func Run(ctx context.Context, run RunFunc, cases []Case, metrics []Metric, opts Options) Report {
	runs := opts.Runs
	if runs < 1 {
		runs = 1
	}
	conc := opts.Concurrency
	if conc < 1 {
		conc = 8
	}

	passes := make([][]int64, len(cases))
	for i := range passes {
		passes[i] = make([]int64, len(metrics))
	}

	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for i := range cases {
		for r := 0; r < runs; r++ {
			wg.Add(1)
			sem <- struct{}{}
			go func(i int) {
				defer wg.Done()
				defer func() { <-sem }()
				out := run(ctx, cases[i].Input)
				for m := range metrics {
					if metrics[m].Fn(ctx, cases[i], out) {
						atomic.AddInt64(&passes[i][m], 1)
					}
				}
			}(i)
		}
	}
	wg.Wait()

	rep := Report{RunsPerCase: runs, Overall: map[string]MetricStat{}}
	totals := make([]int64, len(metrics))
	for i, c := range cases {
		cr := CaseReport{Name: c.Name, Metrics: map[string]MetricStat{}}
		for m := range metrics {
			p := int(passes[i][m])
			cr.Metrics[metrics[m].Name] = MetricStat{Passes: p, Runs: runs, Rate: float64(p) / float64(runs)}
			totals[m] += passes[i][m]
		}
		rep.Cases = append(rep.Cases, cr)
	}
	denom := len(cases) * runs
	for m := range metrics {
		rate := 0.0
		if denom > 0 {
			rate = float64(totals[m]) / float64(denom)
		}
		rep.Overall[metrics[m].Name] = MetricStat{Passes: int(totals[m]), Runs: denom, Rate: rate}
	}
	return rep
}

// AgentRunner wraps an Agent as a RunFunc, giving each run a unique runID (prefix plus a counter)
// so evaluation runs are independent in the durable store.
func AgentRunner(a *agent.Agent, runIDPrefix string) RunFunc {
	var n int64
	return func(ctx context.Context, input string) RunOutput {
		id := fmt.Sprintf("%s-%d", runIDPrefix, atomic.AddInt64(&n, 1))
		msg, err := a.Run(ctx, id, input)
		return RunOutput{Final: msg, Err: err}
	}
}

// String renders the report as a readable table. Metric names are sorted for stable output.
func (r Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "eval: %d run(s) per case\n", r.RunsPerCase)
	names := make([]string, 0, len(r.Overall))
	for n := range r.Overall {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, c := range r.Cases {
		for _, n := range names {
			s := c.Metrics[n]
			fmt.Fprintf(&b, "  %-22s %-24s %d/%d (%.0f%%)\n", c.Name, n, s.Passes, s.Runs, s.Rate*100)
		}
	}
	fmt.Fprintf(&b, "  %-22s\n", "OVERALL")
	for _, n := range names {
		s := r.Overall[n]
		fmt.Fprintf(&b, "  %-22s %-24s %d/%d (%.0f%%)\n", "", n, s.Passes, s.Runs, s.Rate*100)
	}
	return b.String()
}
