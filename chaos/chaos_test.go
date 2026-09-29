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
