package agent

import (
	"os"
	"os/exec"
	"path/filepath"
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
	out, err := exec.Command("go", "list", "-deps", "github.com/bide-ai/bide/agent").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	deps := string(out)

	forbidden := []string{
		"bide-ai/bide/model/", // provider adapters
		"bide-ai/bide/store/", // persistence adapters
		"bide-ai/bide/trace",  // OTel adapter
		"bide-ai/bide/middleware",
		"bide-ai/bide/govern", // gsm-backed governor (Tier-2 edge), its own module
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

// TestCoreModuleHasNoGSM locks the module boundary that keeps gsm out of the core: govern is its
// own module (github.com/bide-ai/bide/govern), so the core module's graph, read standalone with no
// workspace, must not name gsm at all. A consumer of the core then never sees gsm in go.sum, go
// mod graph, or a dependency scan.
func TestCoreModuleHasNoGSM(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go unavailable: %v", err)
	}
	cmd := exec.Command("go", "mod", "graph")
	cmd.Dir = ".." // the core module's root
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go mod graph: %v", err)
	}
	if strings.Contains(string(out), "blackwell-systems/gsm") {
		t.Errorf("the core module's graph names gsm; it belongs to the govern module:\n%s", out)
	}
	for _, f := range []string{"go.mod", "go.sum"} {
		b, err := os.ReadFile(filepath.Join("..", f))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "blackwell-systems/gsm") {
			t.Errorf("the core module's %s names gsm", f)
		}
	}
}
