package benchmarks

import "testing"

// TestFairness_ADKResumeOfCompleteRunIsNoop proves the ADK adapter is not a strawman: ADK's
// event persistence + history replay genuinely de-dupes. Resuming a *completed* run (the
// charge was durably recorded) does NOT re-fire — so the double-fires the chaos benchmark
// finds are specifically the execute→persist window, not a rigged failure. This is the same
// fairness discipline applied to trpc (fairness_test.go).
func TestFairness_ADKResumeOfCompleteRunIsNoop(t *testing.T) {
	run := ADK().NewRun().(*adkRun)

	if run.Step(0); *run.fired != 1 { // complete run: charge fires once and persists
		t.Fatalf("clean run must fire exactly once, got %d", *run.fired)
	}
	if run.Step(0); *run.fired != 1 { // resume on the same session must be a genuine no-op
		t.Fatalf("resume of a completed run re-fired the charge: fired=%d", *run.fired)
	}
}
