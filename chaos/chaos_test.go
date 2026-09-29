package chaos

import "testing"

// The Bide durable loop passes: the charge fires at most once under every crash schedule.
func TestVerify_BidePasses(t *testing.T) {
	rep := Verify("Bide", Bide(), 500)
	t.Log(rep)
	if !rep.OK() {
		t.Fatalf("Bide failed the chaos benchmark: %+v", rep)
	}
}

// The naive at-least-once reference FAILS — proving the harness is non-vacuous (it catches
// a real double-fire, not just always-passing).
func TestVerify_NaiveReferenceFails(t *testing.T) {
	rep := Verify("naive-loop", NaiveReference(), 500)
	t.Log(rep)
	if rep.OK() {
		t.Fatal("naive at-least-once loop unexpectedly passed — the harness would be vacuous")
	}
	if rep.MaxFired < 2 {
		t.Fatalf("expected a double-fire (maxFired>=2), got %d", rep.MaxFired)
	}
}

// A side-by-side the benchmark prints (go test ./chaos -run Benchmark -v).
func TestVerify_Benchmark(t *testing.T) {
	t.Log("chaos benchmark — at-most-once side effect under crash injection:")
	t.Log("  " + Verify("Bide", Bide(), 300).String())
	t.Log("  " + Verify("naive-loop", NaiveReference(), 300).String())
}

// Writes is the sweep's bound: Verify crashes at write 1..Writes()+2 and draws every randomized
// crash point from that range, so a write past the bound is never crashed at. Each reference
// adapter must report the writes a clean run makes: the last one crashes, the one after it does
// not (no write is left to crash at).
func TestReferenceSystems_WritesMatchACleanRun(t *testing.T) {
	for _, c := range []struct {
		name string
		sys  System
	}{{"Bide", Bide()}, {"naive-loop", NaiveReference()}} {
		w := c.sys.Writes()
		if !c.sys.NewRun().Step(w) {
			t.Errorf("%s: Writes() = %d, but a crash at write %d does not happen: a clean run makes fewer writes", c.name, w, w)
		}
		if c.sys.NewRun().Step(w + 1) {
			t.Errorf("%s: Writes() = %d, but a crash at write %d still happens: a clean run makes more writes", c.name, w, w+1)
		}
	}
}

// fakeSystem is a scripted System for testing the harness itself. A run crashes at any write
// index up to writes, fires the side effect on a Step when fire says so, and ends on the first
// Step that does not crash.
type fakeSystem struct {
	writes int
	fire   func(crashAt, step int) bool
}

func (f fakeSystem) Writes() int   { return f.writes }
func (f fakeSystem) NewRun() Run   { return &fakeRun{sys: f} }
func (r *fakeRun) Fired() int      { return r.fired }
func (r *fakeRun) Step(c int) bool { return r.step(c) }

type fakeRun struct {
	sys   fakeSystem
	steps int
	fired int
}

func (r *fakeRun) step(crashAt int) bool {
	if r.sys.fire(crashAt, r.steps) {
		r.fired++
	}
	r.steps++
	return crashAt > 0 && crashAt <= r.sys.writes
}

// A system that never performs the side effect never double-fires, but it has not done the
// work either: a run that ends, crash-free or after resuming, must have fired exactly once.
func TestVerify_NeverFiringFails(t *testing.T) {
	rep := Verify("never", fakeSystem{writes: 3, fire: func(int, int) bool { return false }}, 50)
	if rep.OK() {
		t.Fatalf("a system that never fires passed: %v", rep)
	}
}

// A system that fires on a clean run but loses the side effect across a crash (the resume ends
// without performing it) fails too: completing after a resume must also mean fired once.
func TestVerify_LostOnResumeFails(t *testing.T) {
	sys := fakeSystem{writes: 3, fire: func(crashAt, step int) bool { return crashAt == 0 && step == 0 }}
	rep := Verify("loses-it", sys, 50)
	if rep.OK() {
		t.Fatalf("a system that loses the side effect across a crash passed: %v", rep)
	}
}

// The two passing shapes stay passing: fired once on every completed run.
func TestVerify_FiresOnceOnCompletionPasses(t *testing.T) {
	// Fires on the step that completes (the first Step with no crash in range).
	sys := fakeSystem{writes: 3, fire: func(crashAt, _ int) bool { return crashAt == 0 || crashAt > 3 }}
	if rep := Verify("once", sys, 50); !rep.OK() {
		t.Fatalf("a system that fires once per completed run failed: %+v", rep)
	}
}
