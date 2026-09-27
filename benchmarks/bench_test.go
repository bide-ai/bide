package benchmarks

import (
	"testing"

	"github.com/blackwell-systems/bide/chaos"
)

// TestChaos_Comparison runs the crash-injection benchmark across SDKs and logs a table.
// Bide must hold at-most-once; the others are measured, not assumed.
func TestChaos_Comparison(t *testing.T) {
	t.Log("chaos benchmark — non-idempotent side effect under crash injection (at-most-once):")
	for _, c := range []struct {
		name string
		sys  chaos.System
	}{
		{"Bide", chaos.GoAgents()},
		{"trpc-agent-go", TRPC()},
		{"langchaingo", LangChainGo()},
		{"eino", EinoGraph()},
		{"adk-go", ADK()},
		{"naive-loop", chaos.NaiveReference()},
	} {
		rep := chaos.Verify(c.name, c.sys, 200)
		t.Log("  " + rep.String())
	}
}
