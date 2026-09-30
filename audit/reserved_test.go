package audit

import (
	"testing"

	"github.com/bide-ai/bide/agent"
)

// The audit package's leaves share a run's journal with the engine and with developer-chosen
// steps, so each leaf name starts with a prefix agent reserves: a Step cannot take it.
func TestLeafNames_AreReserved(t *testing.T) {
	for _, name := range []string{policyLeafName("d"), convergenceLeafName("d"), grantLeafName("d"), runCertLeafName("r")} {
		if !agent.IsReservedStepName(name) {
			t.Fatalf("leaf name %q is not reserved by agent", name)
		}
	}
}
