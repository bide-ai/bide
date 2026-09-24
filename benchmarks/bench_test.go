package benchmarks

import (
	"testing"

	"github.com/dayna/go-agents/chaos"
)

// TestChaos_Comparison runs the crash-injection benchmark across SDKs and logs a table.
// go-agents must hold at-most-once; the others are measured, not assumed.
func TestChaos_Comparison(t *testing.T) {
	t.Log("chaos benchmark — non-idempotent side effect under crash injection (at-most-once):")
	for _, c := range []struct {
		name string
		sys  chaos.System
	}{
		{"go-agents", chaos.GoAgents()},
		{"trpc-agent-go", TRPC()},
		{"naive-loop", chaos.NaiveReference()},
	} {
		rep := chaos.Verify(c.name, c.sys, 200)
		t.Log("  " + rep.String())
	}
}
