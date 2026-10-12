package agent

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCoreHasNoAdapterImports locks the ports-and-adapters boundary: the core domain
// must depend only on its ports (interfaces) and pure support libs, never on a
// concrete adapter. Dependencies point INWARD. If this fails, an adapter import crept
// into the core and the hexagon is rotting (the failure agenticenv shipped: its core
// drags Temporal/Weaviate into every binary).
//
// It is an allowlist: every package in the runtime import graph (go list -deps) is in the
// standard library or in the core module, so a dependency of any name fails it, not only the ones
// someone thought to forbid. Within the core module, the model adapters and middleware are
// adapters too, and the agent package may not import them.
func TestCoreHasNoAdapterImports(t *testing.T) {
	for _, p := range nonStdDeps(t, "github.com/bide-ai/bide/agent") {
		if p.module != "github.com/bide-ai/bide" {
			t.Errorf("core depends on %s (module %q): the core is the standard library and its own packages only", p.path, p.module)
		}
		for _, bad := range []string{"bide-ai/bide/model/", "bide-ai/bide/middleware"} {
			if strings.Contains(p.path, bad) {
				t.Errorf("core imports adapter %s: dependencies must point inward (ports, not adapters)", p.path)
			}
		}
	}
}

// TestCoreModuleRequiresNothing: the core module's go.mod requires no module, and its go.sum is
// empty, so a consumer of the core sees nothing but bide in go.sum, go mod graph or a dependency
// scan. Read standalone, with no workspace.
func TestCoreModuleRequiresNothing(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go unavailable: %v", err)
	}
	cmd := exec.Command("go", "mod", "edit", "-json")
	cmd.Dir = ".." // the core module's root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go mod edit -json: %v", err)
	}
	var mod struct{ Require []struct{ Path string } }
	if err := json.Unmarshal(out, &mod); err != nil {
		t.Fatal(err)
	}
	for _, r := range mod.Require {
		t.Errorf("the core module requires %s; it requires the standard library only", r.Path)
	}
	if b, err := os.ReadFile(filepath.Join("..", "go.sum")); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		t.Errorf("the core module's go.sum is not empty:\n%s", b)
	}
}

// dep is one package of an import graph and the module that holds it.
type dep struct{ path, module string }

// nonStdDeps returns the packages of pkg's runtime import graph outside the standard library, read
// with no workspace so only the core module's own requirements count.
func nonStdDeps(t *testing.T, pkg string) []dep {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go unavailable: %v", err)
	}
	cmd := exec.Command("go", "list", "-deps", "-json=ImportPath,Standard,Module", pkg)
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	var deps []dep
	n := 0
	for dec := json.NewDecoder(bytes.NewReader(out)); dec.More(); n++ {
		var p struct {
			ImportPath string
			Standard   bool
			Module     *struct{ Path string }
		}
		if err := dec.Decode(&p); err != nil {
			t.Fatal(err)
		}
		if p.Standard {
			continue
		}
		d := dep{path: p.ImportPath}
		if p.Module != nil {
			d.module = p.Module.Path
		}
		deps = append(deps, d)
	}
	if n == 0 {
		t.Fatal("go list reported no packages")
	}
	return deps
}
