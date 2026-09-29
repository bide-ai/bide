// Package eval is a statistical evaluation harness for agents. It answers "does the model decide
// well?" the only way a stochastic model can be answered: run labeled cases multiple times and
// report a pass-rate distribution with confidence intervals, not a single verdict.
//
// Read the boundary carefully, it is the point of this package. eval measures the MODEL's judgment,
// statistically and best-effort; it is NOT a proof and a pass rate is not a guarantee. The provable
// parts of the system are separate: the governed policy's convergence and invariant enforcement
// (the govern package and the gsm proof) bound what the model can do for all inputs, and the audit
// package proves what it did. eval quantifies the model; governance contains it; audit records it.
//
// Rigor: because a raw pass rate over few runs is misleading (4/5 is not "80%", it is 80% with a
// wide interval), every rate carries a 95% Wilson confidence interval, and the harness evaluates the
// agent's TRAJECTORY (which tools it called, how many steps it took), not only the final message,
// using the durable journal. Evaluate with Runs well above 1, pin the model version, and set
// temperature 0 for the most reproducible baseline (still not perfectly deterministic).
package eval

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bide-ai/bide/agent"
)

// Case is one evaluation input plus optional expectations that metrics interpret. Tags are
// arbitrary labels (for example "hard", "kyc", "refund") used to stratify the Report: every tag a
// case carries gets its own aggregate pass rate, so a hard subset can be reported apart from an easy
// one.
type Case struct {
	Name  string
	Input string
	Want  any
	Tags  []string
}

// Trajectory is the agent's behavior on one run, reconstructed from the durable journal: how many
// model turns it took and which tools it called, in order, plus the raw records for custom metrics.
type Trajectory struct {
	Steps     int
	ToolCalls []string
	Records   []agent.Record
}

// TrajectoryFrom reconstructs a Trajectory from a run's journal records.
func TrajectoryFrom(records []agent.Record) Trajectory {
	tr := Trajectory{Records: records}
	for _, r := range records {
		if r.Kind == agent.StepModel && r.Message != nil {
			tr.Steps++
			for _, p := range r.Message.Parts {
				if tu, ok := p.(agent.ToolUse); ok {
					tr.ToolCalls = append(tr.ToolCalls, tu.Name)
				}
			}
		}
	}
	return tr
}

// RunOutput is the result of executing one case: the final message, any error, the runID, and the
// trajectory (populated by AgentRunner; empty for a bare RunFunc).
type RunOutput struct {
	Final agent.Message
	Err   error
	RunID string
	Trace Trajectory
}

// RunFunc executes one case input and returns its output. Wrap an agent with AgentRunner, or supply
// any function (useful for tests and non-agent baselines).
type RunFunc func(ctx context.Context, input string) RunOutput

// Metric scores a single run of a case as pass (true) or fail (false).
type Metric struct {
	Name string
	Fn   func(ctx context.Context, c Case, out RunOutput) bool
}

// --- outcome metrics (score the final message) ---

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

// Custom builds a metric from a name and a predicate; the predicate may inspect out.Trace.
func Custom(name string, fn func(ctx context.Context, c Case, out RunOutput) bool) Metric {
	return Metric{Name: name, Fn: fn}
}

// Judge is an LLM-as-judge metric: it asks a model whether the output satisfies rubric, passing iff
// the judge answers PASS. It is itself stochastic (a model grading a model), so treat it as a
// signal, run it with Runs well above 1, and never read it as a verdict.
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

// --- trajectory metrics (score the agent's behavior, from the journal) ---

// CalledTool passes iff the run called a tool with this name at least once.
func CalledTool(name string) Metric {
	return Metric{Name: "called:" + name, Fn: func(_ context.Context, _ Case, out RunOutput) bool {
		for _, t := range out.Trace.ToolCalls {
			if t == name {
				return true
			}
		}
		return false
	}}
}

// MaxSteps passes iff the run took at most n model turns (a guard against looping/thrashing).
func MaxSteps(n int) Metric {
	return Metric{Name: fmt.Sprintf("max_steps:%d", n), Fn: func(_ context.Context, _ Case, out RunOutput) bool {
		return out.Trace.Steps <= n
	}}
}

// ToolOrder passes iff the named tools were each called, in the given relative order (as a
// subsequence of the actual call order).
func ToolOrder(names ...string) Metric {
	return Metric{Name: "tool_order:" + strings.Join(names, ">"), Fn: func(_ context.Context, _ Case, out RunOutput) bool {
		i := 0
		for _, t := range out.Trace.ToolCalls {
			if i < len(names) && t == names[i] {
				i++
			}
		}
		return i == len(names)
	}}
}

// Options configures a run. Runs is the executions per case (default 1); use many more to sample a
// stochastic model and get a meaningful confidence interval. Concurrency caps in-flight executions
// (default 8, friendly to provider rate limits).
type Options struct {
	Runs        int
	Concurrency int
	// Provenance records the run conditions (model, temperature, seed, timestamp) to stamp onto the
	// Report. It is optional; Run always overwrites CaseSetHash with a hash of the cases scored,
	// regardless of what the caller supplies here.
	Provenance Provenance
}

// MetricStat is a metric's pass count over some runs, with a 95% Wilson score confidence interval
// on the true pass rate. The interval is wide for small Runs, by design: it stops a lucky 4/5 from
// reading as a solid 80%.
type MetricStat struct {
	Passes int     `json:"passes"`
	Runs   int     `json:"runs"`
	Rate   float64 `json:"rate"`
	CILow  float64 `json:"ci_low"`
	CIHigh float64 `json:"ci_high"`
}

func stat(passes, runs int) MetricStat {
	s := MetricStat{Passes: passes, Runs: runs}
	if runs > 0 {
		s.Rate = float64(passes) / float64(runs)
		s.CILow, s.CIHigh = wilson(passes, runs)
	}
	return s
}

// wilson returns the 95% Wilson score interval for a binomial proportion. It is well-behaved for
// small n and extreme rates, unlike the normal approximation.
func wilson(passes, n int) (lo, hi float64) {
	if n == 0 {
		return 0, 0
	}
	const z = 1.96
	nf := float64(n)
	phat := float64(passes) / nf
	denom := 1 + z*z/nf
	center := (phat + z*z/(2*nf)) / denom
	margin := z * math.Sqrt(phat*(1-phat)/nf+z*z/(4*nf*nf)) / denom
	lo, hi = center-margin, center+margin
	if lo < 0 {
		lo = 0
	}
	if hi > 1 {
		hi = 1
	}
	return lo, hi
}

// CaseReport holds one case's per-metric stats over Runs executions.
type CaseReport struct {
	Name    string                `json:"name"`
	Metrics map[string]MetricStat `json:"metrics"`
}

// Report is the statistical result of an evaluation: per-case and aggregate pass rates with
// confidence intervals, plus latency percentiles. JSON-marshalable. A distribution, not a verdict.
// ByTag stratifies the aggregate by case tag: tag -> metric -> stat, over the runs of every case
// carrying that tag. Provenance records the conditions under which the run happened (with a hash of
// the case set) for reproducibility and audit.
type Report struct {
	RunsPerCase int                              `json:"runs_per_case"`
	TotalRuns   int                              `json:"total_runs"`
	Cases       []CaseReport                     `json:"cases"`
	Overall     map[string]MetricStat            `json:"overall"`
	ByTag       map[string]map[string]MetricStat `json:"by_tag,omitempty"`
	LatencyP50  time.Duration                    `json:"latency_p50"`
	LatencyP95  time.Duration                    `json:"latency_p95"`
	Provenance  Provenance                       `json:"provenance"`
}

// Run executes each case Runs times through run, scores every execution with each metric, and
// returns a Report of pass rates (with Wilson confidence intervals) and latency percentiles.
// Executions run concurrently up to Options.Concurrency. Because the model is stochastic, the report
// is a distribution over runs, not a single pass/fail.
//
// If ctx is cancelled before every execution has finished, Run starts no further executions and
// returns ctx's error with an empty Report: executions cut short by the cancellation would score as
// failures, and the ones that did finish are not a representative sample.
func Run(ctx context.Context, run RunFunc, cases []Case, metrics []Metric, opts Options) (Report, error) {
	runs := opts.Runs
	if runs < 1 {
		runs = 1
	}
	conc := opts.Concurrency
	if conc < 1 {
		conc = 8
	}
	total := len(cases) * runs

	passes := make([][]int64, len(cases))
	for i := range passes {
		passes[i] = make([]int64, len(metrics))
	}
	latencies := make([]time.Duration, total)

	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
launch:
	for i := range cases {
		for r := 0; r < runs; r++ {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				break launch
			}
			wg.Add(1)
			go func(i, r int) {
				defer wg.Done()
				defer func() { <-sem }()
				start := time.Now()
				out := run(ctx, cases[i].Input)
				latencies[i*runs+r] = time.Since(start)
				for m := range metrics {
					if metrics[m].Fn(ctx, cases[i], out) {
						atomic.AddInt64(&passes[i][m], 1)
					}
				}
			}(i, r)
		}
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return Report{}, fmt.Errorf("eval: cancelled before all %d runs finished: %w", total, err)
	}

	rep := Report{RunsPerCase: runs, TotalRuns: total, Overall: map[string]MetricStat{}, Provenance: opts.Provenance}
	rep.Provenance.CaseSetHash = HashCases(cases)
	totals := make([]int64, len(metrics))
	// Per-tag accumulators: tag -> metric index -> (passes, runs).
	tagPasses := map[string][]int64{}
	tagRuns := map[string]int{}
	for i, c := range cases {
		cr := CaseReport{Name: c.Name, Metrics: map[string]MetricStat{}}
		for m := range metrics {
			cr.Metrics[metrics[m].Name] = stat(int(passes[i][m]), runs)
			totals[m] += passes[i][m]
		}
		rep.Cases = append(rep.Cases, cr)
		for _, tag := range c.Tags {
			if _, ok := tagPasses[tag]; !ok {
				tagPasses[tag] = make([]int64, len(metrics))
			}
			for m := range metrics {
				tagPasses[tag][m] += passes[i][m]
			}
			tagRuns[tag] += runs
		}
	}
	for m := range metrics {
		rep.Overall[metrics[m].Name] = stat(int(totals[m]), total)
	}
	if len(tagPasses) > 0 {
		rep.ByTag = map[string]map[string]MetricStat{}
		for tag, tp := range tagPasses {
			byMetric := map[string]MetricStat{}
			for m := range metrics {
				byMetric[metrics[m].Name] = stat(int(tp[m]), tagRuns[tag])
			}
			rep.ByTag[tag] = byMetric
		}
	}
	if total > 0 {
		sort.Slice(latencies, func(a, b int) bool { return latencies[a] < latencies[b] })
		rep.LatencyP50 = latencies[pctIndex(total, 50)]
		rep.LatencyP95 = latencies[pctIndex(total, 95)]
	}
	return rep, nil
}

func pctIndex(n, p int) int {
	i := (n * p) / 100
	if i >= n {
		i = n - 1
	}
	return i
}

// AgentRunner wraps an Agent as a RunFunc, giving each run a unique runID so evaluation runs are
// independent, durable, and auditable. The ID is the prefix, a random nonce drawn once per
// AgentRunner, and a counter: an eval never reuses an earlier eval's runs, which would replay their
// recorded answers instead of sampling the model, even over a store kept between evals. It reads
// the run's journal from store (the same Durable the Agent was built with) to populate the
// Trajectory for trajectory metrics.
func AgentRunner(a *agent.Agent, store agent.Durable, runIDPrefix string) RunFunc {
	var n int64
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("eval: AgentRunner: read random nonce: %v", err))
	}
	nonce := hex.EncodeToString(b[:])
	return func(ctx context.Context, input string) RunOutput {
		id := fmt.Sprintf("%s-%s-%d", runIDPrefix, nonce, atomic.AddInt64(&n, 1))
		msg, err := a.Run(ctx, id, input)
		recs, _ := store.History(ctx, id)
		return RunOutput{Final: msg, Err: err, RunID: id, Trace: TrajectoryFrom(recs)}
	}
}

// String renders the report as a readable table with confidence intervals and latency. Metric names
// are sorted for stable output.
func (r Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "eval: %d run(s) per case, %d total\n", r.RunsPerCase, r.TotalRuns)
	names := make([]string, 0, len(r.Overall))
	for n := range r.Overall {
		names = append(names, n)
	}
	sort.Strings(names)
	line := func(who string, s MetricStat, name string) {
		fmt.Fprintf(&b, "  %-20s %-22s %d/%d  rate=%.0f%%  95%%CI=[%.0f%%,%.0f%%]\n",
			who, name, s.Passes, s.Runs, s.Rate*100, s.CILow*100, s.CIHigh*100)
	}
	for _, c := range r.Cases {
		for _, n := range names {
			line(c.Name, c.Metrics[n], n)
		}
	}
	for _, n := range names {
		line("OVERALL", r.Overall[n], n)
	}
	if len(r.ByTag) > 0 {
		tags := make([]string, 0, len(r.ByTag))
		for t := range r.ByTag {
			tags = append(tags, t)
		}
		sort.Strings(tags)
		for _, t := range tags {
			for _, n := range names {
				if s, ok := r.ByTag[t][n]; ok {
					line("tag:"+t, s, n)
				}
			}
		}
	}
	fmt.Fprintf(&b, "  latency p50=%s p95=%s\n", r.LatencyP50.Round(time.Millisecond), r.LatencyP95.Round(time.Millisecond))
	if r.Provenance.ModelID != "" || r.Provenance.CaseSetHash != "" {
		hash := r.Provenance.CaseSetHash
		if len(hash) > 12 {
			hash = hash[:12]
		}
		fmt.Fprintf(&b, "  provenance model=%s temp=%g seed=%d cases=%s\n",
			r.Provenance.ModelID, r.Provenance.Temperature, r.Provenance.Seed, hash)
	}
	return b.String()
}
