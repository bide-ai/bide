package agent

import (
	"os/exec"
	"strings"
	"testing"
)

// TestCoreHasNoAdapterImports locks the ports-and-adapters boundary: the core domain
// must depend only on its ports (interfaces) and pure support libs — never on a
// concrete adapter. Dependencies point INWARD. If this fails, an adapter import crept
// into the core and the hexagon is rotting (the failure agenticenv shipped: its core
// drags Temporal/Weaviate into every binary).
//
// go list -deps reports non-test dependencies, so this checks the runtime import graph.
func TestCoreHasNoAdapterImports(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/dayna/go-agents").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	deps := string(out)

	forbidden := []string{
		"dayna/go-agents/model/", // provider adapters
		"dayna/go-agents/store/", // persistence adapters
		"dayna/go-agents/trace",  // OTel adapter
		"dayna/go-agents/middleware",
		"dayna/go-agents/govern", // gsm-backed governor (Tier-2 edge)
	}
	for _, bad := range forbidden {
		if strings.Contains(deps, bad) {
			t.Errorf("core imports adapter %q — dependencies must point inward (ports, not adapters)", bad)
		}
	}

	// Also assert the core pulls no heavy infrastructure transitively.
	for _, infra := range []string{"opentelemetry", "modernc.org/sqlite", "jackc/pgx", "temporal", "weaviate", "blackwell-systems/gsm"} {
		if strings.Contains(deps, infra) {
			t.Errorf("core transitively depends on infrastructure %q — the core must stay dependency-light", infra)
		}
	}
}
