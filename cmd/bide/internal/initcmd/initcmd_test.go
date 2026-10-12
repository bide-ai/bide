package initcmd

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// checkout is the bide checkout this test runs in, for -dev.
const checkout = "../../../.."

func initCmd(version string, args ...string) (int, string) {
	var out bytes.Buffer
	code := Main(version, args, &out, &out)
	return code, out.String()
}

// TestScaffoldBuildsRunsAndVerifies: the scaffolded project, built against this checkout, passes
// go vet and its own test, which runs the story (pause, two signed approvals, one refund, nothing
// repeated on a second run) and verifies the evidence. This is what keeps the template working.
func TestScaffoldBuildsRunsAndVerifies(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go unavailable: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "refund")
	if code, out := initCmd("", "-dev", checkout, dir); code != 0 {
		t.Fatalf("bide init: exit %d\n%s", code, out)
	}
	for _, args := range [][]string{{"mod", "tidy"}, {"vet", "./..."}, {"test", "-count=1", "./..."}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go %s in the scaffold: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
}

// TestGoMod: a release build requires bide at its own version; a build from source needs -dev and
// then requires the placeholder with replace directives to the checkout.
func TestGoMod(t *testing.T) {
	m, err := goMod("refund", "v0.14.0", "")
	if err != nil || !strings.Contains(m, "github.com/bide-ai/bide v0.14.0") || !strings.Contains(m, "github.com/bide-ai/bide/store/sqlite v0.14.0") || strings.Contains(m, "replace") {
		t.Errorf("release go.mod (err %v):\n%s", err, m)
	}
	for _, v := range []string{"", "(devel)", "devel"} {
		if _, err := goMod("refund", v, ""); err == nil {
			t.Errorf("version %q without -dev: no error, want one (there is no version to require)", v)
		}
	}
	m, err = goMod("refund", "", checkout)
	if err != nil || !strings.Contains(m, "replace github.com/bide-ai/bide =>") || !strings.Contains(m, "replace github.com/bide-ai/bide/store/sqlite =>") {
		t.Errorf("-dev go.mod (err %v):\n%s", err, m)
	}
	if _, err := goMod("refund", "", t.TempDir()); err == nil {
		t.Error("-dev of a directory that is not a bide checkout: no error")
	}
}

// TestRefusals: a non-empty directory, a missing or extra argument, and a name that is not a module
// path are refused, and nothing is written.
func TestRefusals(t *testing.T) {
	full := t.TempDir()
	if err := os.WriteFile(filepath.Join(full, "keep.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out := initCmd("v0.14.0", full); code != 1 || !strings.Contains(out, "not empty") {
		t.Errorf("non-empty dir: exit %d, want 1 naming it\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(full, "go.mod")); err == nil {
		t.Error("bide init wrote into a non-empty directory")
	}
	for _, args := range [][]string{{}, {"a", "b"}, {"-module", "bad name", t.TempDir()}} {
		if code, _ := initCmd("v0.14.0", args...); code != 2 {
			t.Errorf("bide init %q: exit %d, want 2", args, code)
		}
	}
	if code, _ := initCmd("v0.14.0", "-h"); code != 0 {
		t.Errorf("bide init -h: exit %d, want 0", code)
	}
}

// TestFiles: the project holds every template file, with the module name filled in.
func TestFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "refund")
	if code, out := initCmd("v0.14.0", dir); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, f := range []string{"main.go", "main_test.go", "flow.yaml", "README.md", ".gitignore", "go.mod"} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "__NAME__") {
			t.Errorf("%s still holds the __NAME__ placeholder", f)
		}
		if strings.Contains(string(b), "\r") {
			t.Errorf("%s has a CR: the project is written with LF line endings", f)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "README.md")); !strings.HasPrefix(string(b), "# refund\n") {
		t.Errorf("README.md does not start with the project's name:\n%.80s", b)
	}
}
