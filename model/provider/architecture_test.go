package provider

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The provider kit is adapter plumbing. No core package, nor any package under one, may depend
// on it, directly or transitively: the core defines the Model contract, and adapters build on the
// kit to meet it. go list -deps reports the non-test import graph, so a test that exercises the
// kit against core behaviour (middleware's retry classification of provider errors) may still
// import it.
func TestCoreDoesNotImportProvider(t *testing.T) {
	const kit = "github.com/bide-ai/bide/model/provider"
	core := []string{"agent", "schema", "middleware", "audit", "plan", "eval"}
	args := []string{"list", "-deps", "-f", `{{.ImportPath}}{{range .Imports}} {{.}}{{end}}`}
	for _, c := range core {
		args = append(args, "github.com/bide-ai/bide/"+c+"/...")
	}
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("go list: %v\n%s", err, ee.Stderr)
		}
		t.Fatalf("go list: %v", err)
	}
	var checked int
	for line := range strings.Lines(string(out)) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		pkg := fields[0]
		if pkg == kit || strings.HasPrefix(pkg, kit+"/") {
			// Every package listed is a core package or one of their dependencies.
			t.Errorf("%s is in the core packages' dependency graph", pkg)
		}
		if !isCore(pkg, core) {
			continue
		}
		checked++
		for _, imp := range fields[1:] {
			if imp == kit || strings.HasPrefix(imp, kit+"/") {
				t.Errorf("core package %s imports %s", pkg, imp)
			}
		}
	}
	if checked < len(core) {
		t.Fatalf("go list reported %d core packages, want at least %d:\n%s", checked, len(core), out)
	}
}

// isCore reports whether pkg is one of the core packages or under one.
func isCore(pkg string, core []string) bool {
	for _, c := range core {
		p := "github.com/bide-ai/bide/" + c
		if pkg == p || strings.HasPrefix(pkg, p+"/") {
			return true
		}
	}
	return false
}
