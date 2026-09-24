package benchmarks

import "testing"

// Fairness: a CLEAN run (no crash) must genuinely resume as a no-op — resuming a completed
// run must NOT re-fire the charge. If it does, the adapter restarts-from-scratch (unfair).
func TestFairness_ResumeOfCompleteRunIsNoop(t *testing.T) {
	run := TRPC().NewRun()
	if crashed := run.Step(0); crashed {
		t.Fatal("clean run should not crash")
	}
	if run.Fired() != 1 {
		t.Fatalf("clean run fired %d, want 1", run.Fired())
	}
	// Resume the already-complete run: trpc should see it's done and NOT re-run charge.
	run.Step(0)
	if run.Fired() != 1 {
		t.Fatalf("resuming a COMPLETE run re-fired the charge (%d) — adapter is restarting, not resuming (UNFAIR)", run.Fired())
	}
}
