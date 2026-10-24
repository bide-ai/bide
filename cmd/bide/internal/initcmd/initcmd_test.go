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
	for _, f := range []string{"main.go", "color.go", "main_test.go", "flow.yaml", "README.md", ".gitignore", "go.mod"} {
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

// TestResetCommandIsShellSafe: the README's "Start over" command runs in sh and in zsh, each when it is
// installed (zsh refuses a glob that matches nothing, so the command names its files), both on a
// directory holding every file a run writes and on one holding none, and leaves only the keys.
func TestResetCommandIsShellSafe(t *testing.T) {
	b, err := tmpl.ReadFile("template/README.md.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	// A Windows checkout may hold the embedded template with CRLF (core.autocrlf); bide init
	// writes it with LF.
	_, after, _ := strings.Cut(strings.ReplaceAll(string(b), "\r\n", "\n"), "## Start over")
	_, block, _ := strings.Cut(after, "```sh\n")
	reset, _, _ := strings.Cut(block, "\n```")
	if !strings.HasPrefix(reset, "rm ") || strings.Contains(reset, "\n") {
		t.Fatalf("the Start over block is not one rm command:\n%s", reset)
	}
	runFiles := []string{"run.db", "run.db-wal", "run.db-shm", "refunds.log", "evidence.json", "approver-keys.json", ".crashed-notify"}
	var shells []string
	for _, sh := range []string{"sh", "zsh"} {
		if _, err := exec.LookPath(sh); err == nil {
			shells = append(shells, sh)
		}
	}
	if len(shells) == 0 {
		t.Skip("no sh or zsh on this system")
	}
	for _, sh := range shells {
		for _, present := range []bool{true, false} {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "keys"), 0o700); err != nil {
				t.Fatal(err)
			}
			keep := filepath.Join(dir, "keys", "keys.json")
			if err := os.WriteFile(keep, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if present {
				for _, f := range runFiles {
					if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			cmd := exec.Command(sh, "-c", reset)
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s, run files present %v: %v\n%s", sh, present, err, out)
			}
			for _, f := range runFiles {
				if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
					t.Errorf("%s: %s survived the reset", sh, f)
				}
			}
			if _, err := os.Stat(keep); err != nil {
				t.Errorf("%s: the reset removed keys/keys.json: %v", sh, err)
			}
		}
	}
}

// TestNextStepsColor: the next steps bide init prints are plain text by default (not a terminal)
// and with -color=never, and in bold cyan with -color=always.
func TestNextStepsColor(t *testing.T) {
	for _, flag := range []string{"", "-color=never"} {
		dir := filepath.Join(t.TempDir(), "refund")
		args := []string{"-dev", checkout, dir}
		if flag != "" {
			args = append([]string{flag}, args...)
		}
		code, out := initCmd("", args...)
		want := "wrote " + dir + " (module refund). Next:\n\n  cd " + dir + "\n  go mod tidy\n  go run .\n\nthen see README.md.\n"
		if code != 0 || out != want {
			t.Errorf("bide init %s: exit %d\n%q\nwant\n%q", flag, code, out, want)
		}
	}
	dir := filepath.Join(t.TempDir(), "refund")
	code, out := initCmd("", "-color=always", "-dev", checkout, dir)
	for _, want := range []string{"\x1b[32m✓\x1b[0m wrote ", "\x1b[1;36mcd " + dir + "\x1b[0m", "\x1b[1;36mgo mod tidy\x1b[0m", "\x1b[1;36mgo run .\x1b[0m"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Errorf("bide init -color=always: exit %d, output lacks %q:\n%q", code, want, out)
		}
	}
	if code, _ := initCmd("", "-color=on", "-dev", checkout, filepath.Join(t.TempDir(), "x")); code != 2 {
		t.Errorf("-color=on: exit %d, want 2", code)
	}
}

// TestAgentScaffoldBuildsRunsAndVerifies: the agent scaffold, in both gate shapes, built against this
// checkout, passes go vet and its own tests (the story: the refund waits for the escalation gate, the
// approvers decide, the refund runs once, a second run repeats nothing, and the declarations are written).
func TestAgentScaffoldBuildsRunsAndVerifies(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go unavailable: %v", err)
	}
	for name, args := range map[string][]string{"strict": {"-agent"}, "single approver": {"-agent", "-single-approver"}} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "support")
			if code, out := initCmd("", append(args, "-dev", checkout, dir)...); code != 0 {
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
		})
	}
}

// TestAgentScaffoldGateShape: the default escalation gate is two of three signed decisions that do not
// count the requester; -single-approver is the one-decision opt-out, said so in the README and in the code;
// the flow scaffold carries no policy.
func TestAgentScaffoldGateShape(t *testing.T) {
	read := func(dir, name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	strict := filepath.Join(t.TempDir(), "a")
	if code, out := initCmd("v0.14.0", "-agent", strict); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	main, readme := read(strict, "main.go"), read(strict, "README.md")
	for _, want := range []string{"singleApprover = false", "Need: 2, Approvers: approvers, ExcludePrincipal: true", "agent.Untrusted(\"inbound-email\")", "agent.WithTaintPolicy"} {
		if !strings.Contains(main, want) {
			t.Errorf("the strict main.go lacks %q", want)
		}
	}
	if !strings.Contains(readme, "two of three signed decisions") || strings.Contains(readme, "ONE unsigned decision") || strings.Contains(main+readme, "__") {
		t.Errorf("the strict README:\n%s", readme)
	}
	single := filepath.Join(t.TempDir(), "b")
	if code, out := initCmd("v0.14.0", "-agent", "-single-approver", single); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	main, readme = read(single, "main.go"), read(single, "README.md")
	if !strings.Contains(main, "singleApprover = true") || !strings.Contains(readme, "ONE unsigned decision") || !strings.Contains(readme, "the weaker choice") || strings.Contains(main+readme, "__") {
		t.Errorf("the single-approver scaffold:\n%s", readme)
	}
	flow := filepath.Join(t.TempDir(), "c")
	if code, out := initCmd("v0.14.0", flow); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if strings.Contains(read(flow, "main.go"), "WithTaintPolicy") {
		t.Error("the flow scaffold carries a taint policy, which does not cover flows")
	}
}

// TestSingleApproverNeedsAgent: -single-approver on its own is a usage error and writes nothing.
func TestSingleApproverNeedsAgent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "x")
	if code, out := initCmd("v0.14.0", "-single-approver", dir); code != 2 || !strings.Contains(out, "-single-approver is for -agent") {
		t.Errorf("exit %d, want 2 naming -agent\n%s", code, out)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("a usage error wrote the directory")
	}
}
