package eval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// This file adds statistical-rigor features on top of the core harness: regression comparison with
// a real significance test, a power/sample-size helper, run provenance, stratified reporting by
// tag, and a governance-compliance metric. All statistics use the standard library only.

// --- 1. regression comparison ---

// MetricComparison is one metric's before/after comparison between two Reports. RateDelta is the
// signed change in pass rate (new minus old). PValue is the raw two-sided significance of the
// change; AdjustedP is that p-value after a Benjamini-Hochberg correction across all metrics
// compared in the same Comparison. Significant is true when AdjustedP < 0.05. Direction reads the
// sign of a significant change (DirectionRegression for a drop, DirectionImprovement for a rise) or
// DirectionFlat when the change is not significant.
type MetricComparison struct {
	Metric      string          `json:"metric"`
	OldRate     float64         `json:"old_rate"`
	NewRate     float64         `json:"new_rate"`
	RateDelta   float64         `json:"rate_delta"`
	OldPasses   int             `json:"old_passes"`
	OldRuns     int             `json:"old_runs"`
	NewPasses   int             `json:"new_passes"`
	NewRuns     int             `json:"new_runs"`
	PValue      float64         `json:"p_value"`
	AdjustedP   float64         `json:"adjusted_p"`
	Significant bool            `json:"significant"`
	Direction   MetricDirection `json:"direction"`
}

// MetricDirection is the reading of one metric's change between two Reports. It is a closed set:
// DirectionRegression, DirectionImprovement and DirectionFlat.
type MetricDirection string

const (
	// DirectionRegression is a significant drop in pass rate.
	DirectionRegression MetricDirection = "regression"
	// DirectionImprovement is a significant rise in pass rate.
	DirectionImprovement MetricDirection = "improvement"
	// DirectionFlat is a change that is not significant (or no change).
	DirectionFlat MetricDirection = "flat"
)

// Comparison is the full set of per-metric comparisons between two Reports, over the metrics present
// in both. It is a distribution-aware regression check, not a single pass/fail: it separates a real
// shift from sampling noise using a significance test and a multiple-comparison correction.
type Comparison struct {
	Metrics []MetricComparison `json:"metrics"`
}

// Compare pairs each metric present in BOTH reports' Overall stats and tests whether its pass rate
// changed. For each metric it computes a two-sided p-value (a two-proportion z-test in general, or
// Fisher's exact test when any expected cell count is below 5), then applies a Benjamini-Hochberg
// correction across all compared metrics to control the false discovery rate. A metric is flagged
// Significant when its adjusted p-value is below 0.05, and Direction names the sign of the change.
//
// Both reports must carry Format ReportFormat, as every Report that Run returns does; otherwise
// Compare returns an error wrapping ErrFormat and no comparison. A report of another layout (or a
// zero Report) decoded into this type would otherwise compare as if its metrics were absent, and a
// regression gate would pass on it.
func Compare(old, new Report) (Comparison, error) {
	for _, r := range []struct {
		which  string
		format string
	}{{"old", old.Format}, {"new", new.Format}} {
		if r.format != ReportFormat {
			return Comparison{}, fmt.Errorf("%w: the %s report's format is %q, want %q", ErrFormat, r.which, r.format, ReportFormat)
		}
	}
	names := make([]string, 0, len(old.Overall))
	for n := range old.Overall {
		if _, ok := new.Overall[n]; ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)

	comps := make([]MetricComparison, 0, len(names))
	raw := make([]float64, 0, len(names))
	for _, n := range names {
		o := old.Overall[n]
		w := new.Overall[n]
		p := comparePValue(o.Passes, o.Runs, w.Passes, w.Runs)
		comps = append(comps, MetricComparison{
			Metric:    n,
			OldRate:   o.Rate,
			NewRate:   w.Rate,
			RateDelta: w.Rate - o.Rate,
			OldPasses: o.Passes,
			OldRuns:   o.Runs,
			NewPasses: w.Passes,
			NewRuns:   w.Runs,
			PValue:    p,
		})
		raw = append(raw, p)
	}

	adj := bhAdjust(raw)
	for i := range comps {
		comps[i].AdjustedP = adj[i]
		comps[i].Significant = adj[i] < 0.05
		switch {
		case !comps[i].Significant:
			comps[i].Direction = DirectionFlat
		case comps[i].RateDelta < 0:
			comps[i].Direction = DirectionRegression
		case comps[i].RateDelta > 0:
			comps[i].Direction = DirectionImprovement
		default:
			comps[i].Direction = DirectionFlat
		}
	}
	return Comparison{Metrics: comps}, nil
}

// comparePValue picks the significance test by the expected-cell-count rule: if any of the four
// expected counts in the 2x2 table falls below 5, the normal approximation behind the z-test is
// unreliable, so it uses Fisher's exact test instead.
func comparePValue(x1, n1, x2, n2 int) float64 {
	if n1 == 0 || n2 == 0 {
		return 1
	}
	a, b := x1, n1-x1
	c, d := x2, n2-x2
	if minExpected(a, b, c, d) < 5 {
		return fishersExact(a, b, c, d)
	}
	return twoProportionP(x1, n1, x2, n2)
}

// minExpected returns the smallest expected cell count of the 2x2 table [[a,b],[c,d]] under the
// hypothesis of no association (row and column totals fixed).
func minExpected(a, b, c, d int) float64 {
	total := float64(a + b + c + d)
	if total == 0 {
		return 0
	}
	r1, r2 := float64(a+b), float64(c+d)
	c1, c2 := float64(a+c), float64(b+d)
	m := r1 * c1 / total
	for _, e := range []float64{r1 * c2 / total, r2 * c1 / total, r2 * c2 / total} {
		if e < m {
			m = e
		}
	}
	return m
}

// twoProportionP is a two-sided two-proportion z-test. It pools the two samples under the null of
// equal rates, forms z = (p1-p2)/sqrt(ppool*(1-ppool)*(1/n1+1/n2)), and returns 2*(1-Phi(|z|)).
func twoProportionP(x1, n1, x2, n2 int) float64 {
	if n1 == 0 || n2 == 0 {
		return 1
	}
	p1 := float64(x1) / float64(n1)
	p2 := float64(x2) / float64(n2)
	ppool := float64(x1+x2) / float64(n1+n2)
	denom := ppool * (1 - ppool) * (1/float64(n1) + 1/float64(n2))
	if denom <= 0 {
		if p1 == p2 {
			return 1
		}
		return 0
	}
	z := (p1 - p2) / math.Sqrt(denom)
	return 2 * (1 - phi(math.Abs(z)))
}

// phi is the standard normal cumulative distribution function, via the error function:
// Phi(x) = 0.5*(1 + erf(x/sqrt(2))).
func phi(x float64) float64 {
	return 0.5 * (1 + math.Erf(x/math.Sqrt2))
}

// fishersExact returns the two-sided p-value of Fisher's exact test for the 2x2 table
// [[a,b],[c,d]], summing the hypergeometric probability of every table with the same margins whose
// probability is no greater than the observed table's (the standard two-sided convention). Cell
// probabilities are computed through log-gamma to stay stable for large counts.
func fishersExact(a, b, c, d int) float64 {
	if a < 0 || b < 0 || c < 0 || d < 0 {
		return 1
	}
	row1 := a + b
	row2 := c + d
	col1 := a + c
	col2 := b + d
	total := a + b + c + d
	if total == 0 {
		return 1
	}
	// Constant part of the hypergeometric log-probability (depends only on the fixed margins).
	logConst := lgamma(row1) + lgamma(row2) + lgamma(col1) + lgamma(col2) - lgamma(total)
	logProb := func(a int) float64 {
		b := row1 - a
		c := col1 - a
		d := total - row1 - c
		if b < 0 || c < 0 || d < 0 {
			return math.Inf(-1)
		}
		return logConst - (lgamma(a) + lgamma(b) + lgamma(c) + lgamma(d))
	}
	observed := logProb(a)
	// a ranges over all values keeping every cell non-negative.
	lo := 0
	if row1-col2 > lo {
		lo = row1 - col2
	}
	hi := row1
	if col1 < hi {
		hi = col1
	}
	const eps = 1e-9
	sum := 0.0
	for k := lo; k <= hi; k++ {
		lp := logProb(k)
		if lp <= observed+eps {
			sum += math.Exp(lp)
		}
	}
	if sum > 1 {
		sum = 1
	}
	return sum
}

// lgamma returns log((n)!) = lgamma(n+1) for a non-negative integer n.
func lgamma(n int) float64 {
	v, _ := math.Lgamma(float64(n) + 1)
	return v
}

// bhAdjust applies the Benjamini-Hochberg step-up procedure to a slice of raw p-values, returning
// adjusted p-values (q-values) in the input order. For the i-th smallest p-value p_(i) of m tests,
// the adjustment is min over k >= i of p_(k)*m/k, enforced to be monotone non-decreasing in rank and
// capped at 1.
func bhAdjust(p []float64) []float64 {
	m := len(p)
	if m == 0 {
		return nil
	}
	idx := make([]int, m)
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(i, j int) bool { return p[idx[i]] < p[idx[j]] })

	adj := make([]float64, m)
	// Walk from the largest p-value down, keeping the running minimum of p_(k)*m/k.
	running := math.Inf(1)
	for rank := m; rank >= 1; rank-- {
		orig := idx[rank-1]
		q := p[orig] * float64(m) / float64(rank)
		if q < running {
			running = q
		}
		v := running
		if v > 1 {
			v = 1
		}
		adj[orig] = v
	}
	return adj
}

// --- 2. power / sample-size ---

// RequiredRuns returns the per-arm sample size needed to detect a drop of minDetectableDrop from
// baselineRate in a two-proportion comparison at the given alpha (two-sided) and power, using the
// standard normal-approximation formula. It rounds up. Use it to size an evaluation before running
// it: a drop smaller than what RequiredRuns can resolve at your Runs will read as noise.
//
// baselineRate must be in [0, 1] and alpha and power in (0, 1); anything else (NaN included)
// returns an error and no count. A drop of zero or less returns 0 (nothing to detect), and a drop
// past the baseline is sized as a drop to 0. A drop so small that the runs it needs exceed int
// returns math.MaxInt. When power is so low that any sample reaches it, the answer is 1.
func RequiredRuns(baselineRate, minDetectableDrop, alpha, power float64) (int, error) {
	if !(baselineRate >= 0 && baselineRate <= 1) || math.IsNaN(minDetectableDrop) ||
		!(alpha > 0 && alpha < 1) || !(power > 0 && power < 1) {
		return 0, fmt.Errorf("eval: RequiredRuns(%g, %g, %g, %g): want baselineRate in [0,1], a non-NaN drop, and alpha and power in (0,1)",
			baselineRate, minDetectableDrop, alpha, power)
	}
	if minDetectableDrop <= 0 {
		return 0, nil
	}
	p1 := baselineRate
	p2 := baselineRate - minDetectableDrop
	if p2 < 0 {
		p2 = 0
	}
	delta := p1 - p2
	if delta <= 0 {
		return math.MaxInt, nil // a positive drop that rounds away against the baseline
	}
	pbar := (p1 + p2) / 2
	zAlpha := probit(1 - alpha/2)
	zBeta := probit(power)
	num := zAlpha*math.Sqrt(2*pbar*(1-pbar)) + zBeta*math.Sqrt(p1*(1-p1)+p2*(1-p2))
	if num <= 0 {
		return 1, nil // the formula needs delta*sqrt(n) >= num, which every n meets
	}
	n := math.Ceil((num * num) / (delta * delta))
	if n >= math.MaxInt {
		return math.MaxInt, nil
	}
	return int(n), nil
}

// probit is the inverse of the standard normal CDF (the quantile function), via the
// Beasley-Springer/Moro approximation. It is accurate to better than 1e-9 across the central region
// and well into the tails, enough for the z-values sample-size formulas need.
func probit(p float64) float64 {
	if p <= 0 {
		return math.Inf(-1)
	}
	if p >= 1 {
		return math.Inf(1)
	}
	// Coefficients for the rational approximation (Acklam).
	a := []float64{-3.969683028665376e+01, 2.209460984245205e+02, -2.759285104469687e+02, 1.383577518672690e+02, -3.066479806614716e+01, 2.506628277459239e+00}
	b := []float64{-5.447609879822406e+01, 1.615858368580409e+02, -1.556989798598866e+02, 6.680131188771972e+01, -1.328068155288572e+01}
	c := []float64{-7.784894002430293e-03, -3.223964580411365e-01, -2.400758277161838e+00, -2.549732539343734e+00, 4.374664141464968e+00, 2.938163982698783e+00}
	d := []float64{7.784695709041462e-03, 3.224671290700398e-01, 2.445134137142996e+00, 3.754408661907416e+00}
	const plow = 0.02425
	const phigh = 1 - plow
	var x float64
	switch {
	case p < plow:
		q := math.Sqrt(-2 * math.Log(p))
		x = (((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) /
			((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	case p <= phigh:
		q := p - 0.5
		r := q * q
		x = (((((a[0]*r+a[1])*r+a[2])*r+a[3])*r+a[4])*r + a[5]) * q /
			(((((b[0]*r+b[1])*r+b[2])*r+b[3])*r+b[4])*r + 1)
	default:
		q := math.Sqrt(-2 * math.Log(1-p))
		x = -(((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) /
			((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	}
	// One Halley refinement step against the true CDF to sharpen the tails.
	e := phi(x) - p
	u := e * math.Sqrt(2*math.Pi) * math.Exp(x*x/2)
	x = x - u/(1+x*u/2)
	return x
}

// --- 3. provenance ---

// Provenance records the conditions under which an evaluation ran, so a Report can be reproduced and
// audited: which model version, at what temperature and seed, when, and a hash of the exact case set
// scored. Pin ModelID and set Temperature to 0 for the most reproducible baseline. CaseSetHash is
// filled by Run from the cases regardless of what the caller supplies.
type Provenance struct {
	ModelID     string    `json:"model_id"`
	Temperature float64   `json:"temperature"`
	Seed        int64     `json:"seed"`
	Timestamp   time.Time `json:"timestamp"`
	CaseSetHash string    `json:"case_set_hash"`
}

// HashCases returns a stable lowercase-hex sha256 over the case set, in order. Each case contributes
// its Name, Input, and the JSON encoding of its Want, so the hash changes if any case's identity,
// input, or expectation changes but stays stable across runs of the same set. Use it to confirm two
// Reports scored the same cases before comparing them. A Want JSON cannot encode (a NaN or infinite
// float, a channel, a func) contributes its Go type and %#v form instead, marked apart from JSON;
// for a func or channel that form is an address, so it is stable only within one process.
func HashCases(cases []Case) string {
	h := sha256.New()
	for _, c := range cases {
		want, err := json.Marshal(c.Want)
		if err != nil {
			want = fmt.Appendf([]byte("\x01"), "%T:%#v", c.Want, c.Want)
		}
		fmt.Fprintf(h, "%d:%s\x00%d:%s\x00%d:%s\x00", len(c.Name), c.Name, len(c.Input), c.Input, len(want), want)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// --- 5. governance-compliance metric ---

// GovernanceHeld passes iff compliant(ctx, out) is true. The caller supplies compliant to inspect the
// run's governed outcome (for example the final message or the trajectory's recorded decision) and
// report whether the containment layer kept it compliant.
//
// The point is the two-number reporting pattern it enables. Run a raw judgment metric (did the model
// decide correctly?) alongside GovernanceHeld (did the governed outcome stay within policy?) and
// report both, for example "model correct 91%, governance held 100%". The gap between the two
// quantifies the value of containment: it is the fraction of runs where the model erred but the
// governance layer still caught it, so the outcome remained compliant despite the model's mistake.
// This metric stays lean and does not couple to the govern package; the predicate is the only seam.
// It receives the evaluation's context, so a predicate that reads a store or a governance log can
// honour cancellation. A nil compliant makes Run return an error wrapping agent.ErrConfig.
func GovernanceHeld(name string, compliant func(ctx context.Context, out RunOutput) bool) Metric {
	if compliant == nil {
		return misconfigured(name, "GovernanceHeld %q: nil predicate", name)
	}
	return Metric{Name: name, Fn: func(ctx context.Context, _ Case, out RunOutput) bool {
		return compliant(ctx, out)
	}}
}

// String renders a comparison as a readable table: rate delta, raw and adjusted p-values, and the
// significance direction per metric. Metrics keep the ascending order Compare produced.
func (c Comparison) String() string {
	var b strings.Builder
	b.WriteString("compare: old -> new (BH-adjusted, significant at adj p<0.05)\n")
	for _, m := range c.Metrics {
		flag := " "
		if m.Significant {
			flag = "*"
		}
		fmt.Fprintf(&b, "  %s %-22s %.0f%%->%.0f%% (%+.0fpp)  p=%.4g adj=%.4g  %s\n",
			flag, m.Metric, m.OldRate*100, m.NewRate*100, m.RateDelta*100, m.PValue, m.AdjustedP, m.Direction)
	}
	return b.String()
}
