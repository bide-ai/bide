package benchmarks

import (
	"sync"
	"sync/atomic"
	"testing"
)

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

// A crash is the process dying at the failed AppendEvent: nothing after that point can happen.
// ADK runs the agent in a scheduler goroutine that, after the runner stops on the persist error,
// may take the "event processed" branch of a select instead of the cancelled-context branch and
// go on to execute the function call. The adapter must not count that as a charge, or the same
// crash schedule fires once on one run and twice on another. The race is rare (about 1 in 6,000
// crashes at the function-call event), so this drives many of them in parallel.
func TestADK_NoChargeAfterTheCrash(t *testing.T) {
	if testing.Short() {
		t.Skip("drives 100,000 crashed runs")
	}
	const workers, per = 50, 2000
	var late atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				run := ADK().NewRun()
				// Append 2 is the model's function-call event: the crash lands before the tool runs.
				if crashed := run.Step(2); !crashed {
					t.Error("a crash at append 2 did not crash")
					return
				}
				if run.Fired() != 0 {
					late.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if n := late.Load(); n > 0 {
		t.Fatalf("%d of %d runs charged after the crash at the function-call event", n, workers*per)
	}
}
