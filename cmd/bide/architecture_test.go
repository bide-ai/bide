package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// TestAuditOnlyBuildIsCore: a bide_audit_only build's import graph is the standard library and the
// core module (its packages and the modules the core module requires): no store driver, no YAML
// library, nothing a verifier did not choose to trust. An allowlist, so a new dependency of any
// name fails it.
func TestAuditOnlyBuildIsCore(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go unavailable: %v", err)
	}
	allowed := map[string]bool{"github.com/bide-ai/bide": true, "github.com/bide-ai/bide/cmd/bide": true}
	for _, m := range requires(t, "../..", false) {
		allowed[m] = true
	}
	cmd := exec.Command("go", "list", "-deps", "-tags", "bide_audit_only", "-json=ImportPath,Standard,Module", ".")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	n := 0
	for dec.More() {
		var p struct {
			ImportPath string
			Standard   bool
			Module     *struct{ Path string }
		}
		if err := dec.Decode(&p); err != nil {
			t.Fatal(err)
		}
		n++
		if p.Standard {
			continue
		}
		if p.Module == nil || !allowed[p.Module.Path] {
			t.Errorf("the bide_audit_only build depends on %s, outside the standard library and the core module", p.ImportPath)
		}
	}
	if n == 0 {
		t.Fatal("go list reported no packages")
	}
}

// TestDirectRequires: the bide module requires bide's own modules and the YAML library, and no
// CLI framework or other library: the CLI is built on the standard library flag package. A new
// requirement has to be added here, on purpose.
func TestDirectRequires(t *testing.T) {
	want := []string{
		"github.com/bide-ai/bide",
		"github.com/bide-ai/bide/flowedit",
		"github.com/bide-ai/bide/store/postgres",
		"github.com/bide-ai/bide/store/sqlite",
		"go.yaml.in/yaml/v3",
	}
	if got := requires(t, ".", true); !slices.Equal(got, want) {
		t.Fatalf("cmd/bide/go.mod requires %v directly, want %v", got, want)
	}
}

// requires returns the modules dir/go.mod requires, sorted; with direct, only those not marked
// // indirect.
func requires(t *testing.T, dir string, direct bool) []string {
	t.Helper()
	cmd := exec.Command("go", "mod", "edit", "-json")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go mod edit -json in %s: %v", dir, err)
	}
	var mod struct {
		Require []struct {
			Path     string
			Indirect bool
		}
	}
	if err := json.Unmarshal(out, &mod); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, r := range mod.Require {
		if !direct || !r.Indirect {
			paths = append(paths, r.Path)
		}
	}
	slices.Sort(paths)
	return paths
}
