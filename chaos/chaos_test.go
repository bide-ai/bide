package chaos

import "testing"

// The Bide durable loop passes: the charge fires at most once under every crash schedule.
func TestVerify_GoAgentsPasses(t *testing.T) {
	rep := Verify("Bide", GoAgents(), 500)
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
	t.Log("  " + Verify("Bide", GoAgents(), 300).String())
	t.Log("  " + Verify("naive-loop", NaiveReference(), 300).String())
}
