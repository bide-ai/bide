// Package chaos is a deterministic crash-injection benchmark for agent runtimes: it drives
// a NON-IDEMPOTENT side effect through a fault schedule and checks it fires at most once.
//
// It's the exportable form of Bide' internal DST — point it at any SDK by implementing
// System (wire that SDK to do one non-idempotent side effect against a fault-injectable,
// resumable store). The harness is what proves — or disproves — an at-most-once guarantee:
// a loop that relies on at-least-once + idempotency double-fires here, visibly. See
// Bide for the reference adapter that passes.
package chaos

import (
	"fmt"
	"math/rand/v2"
)

// Run is one scenario instance: a fresh store + zeroed side-effect counter. Step runs or
// resumes one attempt, injecting a crash at the crashAt-th durable write (0 = no crash);
// it returns crashed=true if that crash aborted the attempt (call Step again to resume),
// false when the attempt reaches a terminal state. Fired reports how many times the
// non-idempotent side effect actually executed across all Steps so far.
type Run interface {
	Step(crashAt int) (crashed bool)
	Fired() int
}

// System is one SDK under test: it builds fresh Runs and reports the number of durable
// writes in a clean run (the crash-sweep upper bound).
type System interface {
	NewRun() Run
	Writes() int
}

// Report is the outcome of a chaos run against one System.
type Report struct {
	Name       string
	Sweeps     int // crash points swept exhaustively
	Schedules  int // total crash schedules exercised (sweep + randomized)
	MaxFired   int // worst side-effect count observed (want 1)
	Violations int // schedules where the side effect fired more than once
	// Missed counts runs that reached a terminal state (crash-free, or after resuming) with the
	// side effect never fired. A system that never does the work never double-fires, so the
	// at-most-once check alone would pass it; a completed run must have fired exactly once.
	Missed int
}

// OK reports whether the side effect fired at most once on every schedule and exactly once on
// every run that reached a terminal state.
func (r Report) OK() bool { return r.Violations == 0 && r.MaxFired <= 1 && r.Missed == 0 }

// String formats the report as one line: the name, the sweep and schedule counts, the worst
// fire count, and a PASS or FAIL verdict.
func (r Report) String() string {
	verdict := "PASS ✓ (at-most-once held)"
	if !r.OK() {
		verdict = fmt.Sprintf("FAIL ✗ (%d double-fires, worst=%d)", r.Violations, r.MaxFired)
		if r.Missed > 0 {
			verdict = fmt.Sprintf("FAIL ✗ (%d double-fires, worst=%d, %d completed without firing)", r.Violations, r.MaxFired, r.Missed)
		}
	}
	return fmt.Sprintf("%-16s sweeps=%-3d schedules=%-5d maxFired=%d  %s",
		r.Name, r.Sweeps, r.Schedules, r.MaxFired, verdict)
}

// record scores one schedule's side-effect count. terminal is true when the run reached a
// terminal state; only then must it have fired (a run still crashing may not have got there).
func (r *Report) record(fired int, terminal bool) {
	if terminal && fired == 0 {
		r.Missed++
	}
	if fired > r.MaxFired {
		r.MaxFired = fired
	}
	if fired > 1 {
		r.Violations++
	}
}

// Verify runs one crash-free run, an exhaustive crash-point sweep, and `seeds` randomized
// multi-crash schedules against sys, and returns a Report. The invariant checked is that the
// non-idempotent side effect fires at most once, no matter where or how often it crashes, and
// exactly once on every run that reaches a terminal state. The crash-free run is checked but not
// counted in Sweeps or Schedules.
func Verify(name string, sys System, seeds int) Report {
	rep := Report{Name: name}
	bound := sys.Writes() + 2

	// Crash-free: with no crash injected, one Step runs the whole run, which must fire exactly once.
	clean := sys.NewRun()
	clean.Step(0)
	rep.record(clean.Fired(), true)

	// Exhaustive: a single crash at every write point, then resume to terminal.
	for crashAt := 1; crashAt <= bound; crashAt++ {
		run := sys.NewRun()
		crashed := run.Step(crashAt)
		for crashed {
			crashed = run.Step(0) // resume without further crashes
		}
		rep.Sweeps++
		rep.Schedules++
		rep.record(run.Fired(), true)
	}

	// Adversarial: randomized multi-crash schedules.
	for s := 0; s < seeds; s++ {
		rng := rand.New(rand.NewPCG(uint64(s)+1, 0x9E3779B97F4A7C15))
		run := sys.NewRun()
		terminal := false
		for attempt := 0; attempt < 64 && !terminal; attempt++ {
			terminal = !run.Step(rng.IntN(bound) + 1)
		}
		rep.Schedules++
		rep.record(run.Fired(), terminal)
	}
	return rep
}
