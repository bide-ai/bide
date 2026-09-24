package benchmarks

import "testing"

// eino has no automatic crash-resume, so fairness = a clean invocation fires exactly once.
func TestFairness_EinoCleanRunFiresOnce(t *testing.T) {
	run := EinoGraph().NewRun()
	if crashed := run.Step(0); crashed {
		t.Fatal("clean run should not crash")
	}
	if run.Fired() != 1 {
		t.Fatalf("clean eino run fired %d, want 1", run.Fired())
	}
}
