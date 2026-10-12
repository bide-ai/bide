package auditcli

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestDepsAreCore: the audit command builds from the standard library and the core module only
// (the core module's packages, and the modules the core module itself requires). It is an
// allowlist, so a dependency nobody thought to forbid fails it too. An auditor's trust in a bide
// audit verdict then rests on the core and the toolchain, not on a store driver or a CLI library.
func TestDepsAreCore(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go unavailable: %v", err)
	}
	cmd := exec.Command("go", "list", "-deps", "-json=ImportPath,Standard,Module", "github.com/bide-ai/bide/internal/auditcli")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	allowed := coreRequires(t)
	allowed["github.com/bide-ai/bide"] = true
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
			mod := "no module"
			if p.Module != nil {
				mod = p.Module.Path
			}
			t.Errorf("internal/auditcli depends on %s (%s), which is neither the standard library nor the core module", p.ImportPath, mod)
		}
	}
	if n == 0 {
		t.Fatal("go list reported no packages")
	}
}

// coreRequires returns the modules the core module's go.mod requires.
func coreRequires(t *testing.T) map[string]bool {
	t.Helper()
	cmd := exec.Command("go", "mod", "edit", "-json")
	cmd.Dir = "../.." // the core module's root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go mod edit -json: %v", err)
	}
	var mod struct{ Require []struct{ Path string } }
	if err := json.Unmarshal(out, &mod); err != nil {
		t.Fatal(err)
	}
	req := map[string]bool{}
	for _, r := range mod.Require {
		req[r.Path] = true
	}
	return req
}
