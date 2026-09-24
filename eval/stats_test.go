package eval_test

import (
	"context"
	"math"
	"sync/atomic"
	"testing"

	agent "github.com/dayna/go-agents"
	"github.com/dayna/go-agents/eval"
)

// reportFor builds a Report whose single overall metric has the given passes/runs, so comparison
// tests can be driven from plain numbers without running the harness.
func reportFor(passes, runs int) eval.Report {
	var n int64
	run := func(_ context.Context, _ string) eval.RunOutput {
		if atomic.AddInt64(&n, 1) <= int64(passes) {
			return eval.RunOutput{Final: agent.UserText("PASS")}
		}
		return eval.RunOutput{Final: agent.UserText("nope")}
	}
	return eval.Run(context.Background(), run, []eval.Case{{Name: "c", Input: "x"}},
		[]eval.Metric{eval.Contains("PASS")}, eval.Options{Runs: runs})
}

// TestCompare_ClearRegression: a large drop (95/100 -> 70/100) is significant and flagged a
// regression; the reference two-proportion z-test p-value is well below 0.05.
func TestCompare_ClearRegression(t *testing.T) {
	old := reportFor(95, 100)
	new := reportFor(70, 100)
	cmp := eval.Compare(old, new)
	if len(cmp.Metrics) != 1 {
		t.Fatalf("want 1 metric compared, got %d", len(cmp.Metrics))
	}
	m := cmp.Metrics[0]
	if !m.Significant || m.Direction != "regression" {
		t.Fatalf("95/100 -> 70/100 should be a significant regression, got significant=%v dir=%q p=%.4g adj=%.4g",
			m.Significant, m.Direction, m.PValue, m.AdjustedP)
	}
	if m.RateDelta > -0.24 || m.RateDelta < -0.26 {
		t.Fatalf("rate delta should be about -0.25, got %.4f", m.RateDelta)
	}
	// Reference: two-proportion z-test for 95/100 vs 70/100 gives p about 4.7e-6.
	if m.PValue > 1e-4 {
		t.Fatalf("p-value should be tiny (~4.7e-6), got %.4g", m.PValue)
	}
}

// TestCompare_TinyChangeNotSignificant: 48/100 -> 52/100 is noise, not significant, direction flat.
func TestCompare_TinyChangeNotSignificant(t *testing.T) {
	old := reportFor(48, 100)
	new := reportFor(52, 100)
	cmp := eval.Compare(old, new)
	m := cmp.Metrics[0]
	if m.Significant || m.Direction != "flat" {
		t.Fatalf("48/100 -> 52/100 should be flat/insignificant, got significant=%v dir=%q p=%.4g",
			m.Significant, m.Direction, m.PValue)
	}
	// Reference: two-proportion z-test p about 0.57.
	if m.PValue < 0.4 || m.PValue > 0.7 {
		t.Fatalf("p-value should be about 0.57, got %.4g", m.PValue)
	}
}

// TestTwoProportionP_Reference checks the z-test against a hand-computable value: 90/100 vs 80/100
// gives z = 0.10/sqrt(0.85*0.15*0.02) = 1.9803..., two-sided p about 0.04767.
func TestTwoProportionP_Reference(t *testing.T) {
	p := eval.TwoProportionPForTest(90, 100, 80, 100)
	if math.Abs(p-0.04767) > 5e-4 {
		t.Fatalf("two-proportion p for 90/100 vs 80/100 = %.5f, want about 0.04767", p)
	}
}

// TestFishersExact_TeaTasting checks the classic Fisher tea-tasting table [[3,1],[1,3]]: the
// two-sided exact p-value is 0.4857142857..., a well-known reference value.
func TestFishersExact_TeaTasting(t *testing.T) {
	p := eval.FishersExactForTest(3, 1, 1, 3)
	if math.Abs(p-0.4857142857) > 1e-6 {
		t.Fatalf("Fisher tea-tasting [[3,1],[1,3]] = %.10f, want 0.4857142857", p)
	}
}

// TestFishersExact_SmallReference checks a second hand-verifiable table [[8,2],[1,5]]: the
// two-sided exact p-value is 0.03497 (matching R's fisher.test).
func TestFishersExact_SmallReference(t *testing.T) {
	p := eval.FishersExactForTest(8, 2, 1, 5)
	if math.Abs(p-0.03497) > 1e-3 {
		t.Fatalf("Fisher [[8,2],[1,5]] = %.5f, want about 0.03497", p)
	}
}

// TestCompare_SmallSampleUsesFisher: with tiny counts the expected-cell rule selects Fisher's exact
// test. 5/6 vs 1/6 has an expected count below 5, and Fisher's two-sided p (about 0.0801) should
// match what Compare reports for that metric.
func TestCompare_SmallSampleUsesFisher(t *testing.T) {
	old := reportFor(5, 6)
	new := reportFor(1, 6)
	cmp := eval.Compare(old, new)
	fisher := eval.FishersExactForTest(5, 1, 1, 5)
	if math.Abs(cmp.Metrics[0].PValue-fisher) > 1e-9 {
		t.Fatalf("small-sample compare should use Fisher (%.6f), got %.6f", fisher, cmp.Metrics[0].PValue)
	}
}

// TestBHAdjust_Reference checks the Benjamini-Hochberg step-up against a worked example.
// p = [0.01, 0.02, 0.03, 0.04, 0.05] with m=5 gives, before the monotone floor,
// [0.05, 0.05, 0.05, 0.05, 0.05]; the enforced non-decreasing q-values are all 0.05.
func TestBHAdjust_Reference(t *testing.T) {
	adj := eval.BHAdjustForTest([]float64{0.01, 0.02, 0.03, 0.04, 0.05})
	for i, q := range adj {
		if math.Abs(q-0.05) > 1e-12 {
			t.Fatalf("bhAdjust[%d] = %.6f, want 0.05", i, q)
		}
	}
	// Ordering-preserving and monotone in rank: smaller raw p never gets a larger q than a larger raw p.
	adj2 := eval.BHAdjustForTest([]float64{0.9, 0.001, 0.5})
	// Sorted p: 0.001(rank1)->0.003, 0.5(rank2)->0.75, 0.9(rank3)->0.9. Back in input order:
	want := []float64{0.9, 0.003, 0.75}
	for i := range want {
		if math.Abs(adj2[i]-want[i]) > 1e-9 {
			t.Fatalf("bhAdjust2[%d] = %.6f, want %.6f", i, adj2[i], want[i])
		}
	}
}

// TestProbit_KnownQuantiles checks the inverse-normal against standard quantiles: z_0.975 ~ 1.95996
// and z_0.84 ~ 0.99446.
func TestProbit_KnownQuantiles(t *testing.T) {
	if z := eval.ProbitForTest(0.975); math.Abs(z-1.959964) > 1e-4 {
		t.Fatalf("probit(0.975) = %.6f, want about 1.95996", z)
	}
	if z := eval.ProbitForTest(0.84); math.Abs(z-0.994458) > 1e-4 {
		t.Fatalf("probit(0.84) = %.6f, want about 0.99446", z)
	}
	if z := eval.ProbitForTest(0.5); math.Abs(z) > 1e-9 {
		t.Fatalf("probit(0.5) = %.6f, want 0", z)
	}
}

// TestRequiredRuns checks the sample-size helper is sane and monotone: detecting a 5-point drop from
// 0.9 at alpha 0.05 power 0.8 needs a few hundred per arm, and a larger drop needs fewer runs.
func TestRequiredRuns(t *testing.T) {
	n5 := eval.RequiredRuns(0.9, 0.05, 0.05, 0.8)
	if n5 < 200 || n5 > 900 {
		t.Fatalf("detecting a 5-point drop from 0.9 should need a few hundred per arm, got %d", n5)
	}
	n15 := eval.RequiredRuns(0.9, 0.15, 0.05, 0.8)
	if n15 >= n5 {
		t.Fatalf("a larger drop should need fewer runs: 15pp=%d must be < 5pp=%d", n15, n5)
	}
	// Reference: the pooled-variance normal-approx formula (as specified) for baseline 0.9, drop to
	// 0.85 at alpha 0.05 two-sided and power 0.8 gives n about 686 per arm:
	// z_a*sqrt(2*0.875*0.125)=0.91669, z_b*sqrt(0.9*0.1+0.85*0.15)=0.39250,
	// (0.91669+0.39250)^2 / 0.05^2 = 685.6 -> ceil 686.
	if n5 != 686 {
		t.Fatalf("5-point drop from 0.9 should be 686 per arm (pooled formula), got %d", n5)
	}
}

// TestHashCases confirms the case-set hash is stable, lowercase-hex, and changes when any case
// changes (name, input, or want).
func TestHashCases(t *testing.T) {
	base := []eval.Case{
		{Name: "a", Input: "one", Want: 1},
		{Name: "b", Input: "two", Want: "yes"},
	}
	h1 := eval.HashCases(base)
	h2 := eval.HashCases([]eval.Case{
		{Name: "a", Input: "one", Want: 1},
		{Name: "b", Input: "two", Want: "yes"},
	})
	if h1 != h2 {
		t.Fatalf("HashCases should be stable: %q vs %q", h1, h2)
	}
	if len(h1) != 64 {
		t.Fatalf("sha256 hex should be 64 chars, got %d", len(h1))
	}
	for _, r := range h1 {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Fatalf("hash should be lowercase hex, got %q", h1)
		}
	}
	changed := eval.HashCases([]eval.Case{
		{Name: "a", Input: "one", Want: 2}, // Want changed
		{Name: "b", Input: "two", Want: "yes"},
	})
	if changed == h1 {
		t.Fatalf("changing a case's Want should change the hash")
	}
	order := eval.HashCases([]eval.Case{base[1], base[0]})
	if order == h1 {
		t.Fatalf("case order should affect the hash")
	}
}

// TestProvenanceOnReport confirms Run stamps caller-supplied provenance and always fills the case
// hash itself.
func TestProvenanceOnReport(t *testing.T) {
	cases := []eval.Case{{Name: "c", Input: "x"}}
	run := func(_ context.Context, _ string) eval.RunOutput { return eval.RunOutput{Final: agent.UserText("ok")} }
	rep := eval.Run(context.Background(), run, cases, []eval.Metric{eval.NoError()},
		eval.Options{Runs: 1, Provenance: eval.Provenance{ModelID: "m-1", Temperature: 0, Seed: 42}})
	if rep.Provenance.ModelID != "m-1" || rep.Provenance.Seed != 42 {
		t.Fatalf("provenance not carried through: %+v", rep.Provenance)
	}
	if rep.Provenance.CaseSetHash != eval.HashCases(cases) {
		t.Fatalf("Run should compute CaseSetHash, got %q", rep.Provenance.CaseSetHash)
	}
}

// TestByTag confirms stratified reporting: cases tagged "hard" and "easy" report separate per-tag
// rates, and the harder subset scores lower.
func TestByTag(t *testing.T) {
	// The runner passes iff the input contains "easy".
	run := func(_ context.Context, input string) eval.RunOutput {
		if input == "easy-in" {
			return eval.RunOutput{Final: agent.UserText("YES")}
		}
		return eval.RunOutput{Final: agent.UserText("no")}
	}
	cases := []eval.Case{
		{Name: "e", Input: "easy-in", Tags: []string{"easy"}},
		{Name: "h", Input: "hard-in", Tags: []string{"hard"}},
	}
	rep := eval.Run(context.Background(), run, cases, []eval.Metric{eval.Contains("YES")}, eval.Options{Runs: 4})
	if rep.ByTag == nil {
		t.Fatal("expected ByTag to be populated")
	}
	easy := rep.ByTag["easy"]["contains:YES"].Rate
	hard := rep.ByTag["hard"]["contains:YES"].Rate
	if easy != 1.0 {
		t.Fatalf("easy tag rate = %v, want 1.0", easy)
	}
	if hard != 0.0 {
		t.Fatalf("hard tag rate = %v, want 0.0", hard)
	}
	if easy <= hard {
		t.Fatalf("per-tag rates should differ (easy %v > hard %v)", easy, hard)
	}
}

// TestGovernanceHeld demonstrates the two-number pattern: the model is wrong half the time (raw
// judgment metric), but the governance-held predicate is always satisfied, so the two rates differ.
func TestGovernanceHeld(t *testing.T) {
	var n int64
	// The model alternates a correct/incorrect judgment, but the governed outcome is always compliant.
	run := func(_ context.Context, _ string) eval.RunOutput {
		if atomic.AddInt64(&n, 1)%2 == 0 {
			return eval.RunOutput{Final: agent.UserText("CORRECT compliant")}
		}
		return eval.RunOutput{Final: agent.UserText("WRONG compliant")}
	}
	cases := []eval.Case{{Name: "c", Input: "x"}}
	rawJudgment := eval.Contains("CORRECT")
	governance := eval.GovernanceHeld("governance_held", func(out eval.RunOutput) bool {
		return contains(out.Final.Text(), "compliant")
	})
	rep := eval.Run(context.Background(), run, cases, []eval.Metric{rawJudgment, governance}, eval.Options{Runs: 4})

	rawRate := rep.Overall["contains:CORRECT"].Rate
	govRate := rep.Overall["governance_held"].Rate
	if rawRate != 0.5 {
		t.Fatalf("raw model judgment should be 0.5, got %v", rawRate)
	}
	if govRate != 1.0 {
		t.Fatalf("governance should hold every run, got %v", govRate)
	}
	if rawRate == govRate {
		t.Fatalf("the two numbers should differ: model correct %v, governance held %v", rawRate, govRate)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
