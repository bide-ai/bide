package benchmarks

import "testing"

// langchaingo has no resume, so the fairness check is: a single CLEAN invocation fires the
// side effect exactly once (the adapter runs the SDK correctly, not double-counting).
func TestFairness_LangChainGoCleanRunFiresOnce(t *testing.T) {
	run := LangChainGo().NewRun()
	if crashed := run.Step(0); crashed {
		t.Fatal("clean run should not crash")
	}
	if run.Fired() != 1 {
		t.Fatalf("clean langchaingo run fired %d, want exactly 1", run.Fired())
	}
}
