package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// userModule writes a module of a user of bide, pinned to the old API (a replace to the oldapi
// stub), with src as its one file.
func userModule(t *testing.T, src string) string {
	t.Helper()
	old, err := filepath.Abs("testdata/oldapi")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	mod := "module example.com/user\n\ngo 1.27\n\nrequire github.com/bide-ai/bide v0.10.0\n\nreplace github.com/bide-ai/bide v0.10.0 => " + old + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "user.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const newAPIUser = `package user

import "github.com/bide-ai/bide/agent"

func build(m agent.Model, j *agent.Journal) (*agent.Agent, error) { return agent.New(m, j) }
`

const brokenUser = `package user

import "github.com/bide-ai/bide/agent"

func build(m agent.Model, j *agent.Journal) *agent.Agent { return agent.New(m, j).WithMaxTurns(n) }
`

// A user's module is checked against the new API (here a directory holding it), not the old one
// its go.mod still names: code written for the new API passes, and code that does not compile is
// reported, each error with its position.
func TestCheckModule_AgainstTheNewAPI(t *testing.T) {
	t.Setenv("GOWORK", "off")
	nw, err := filepath.Abs("testdata/newapi")
	if err != nil {
		t.Fatal(err)
	}
	dir := userModule(t, brokenUser) // as on disk before the rewrite: findings name its lines
	user := filepath.Join(dir, "user.go")
	fs, err := CheckModule(dir, []string{"./..."}, map[string][]byte{user: []byte(newAPIUser)}, nw)
	if err != nil || len(fs) != 0 {
		t.Fatalf("new-API code: findings %v, err %v; want none", fs, err)
	}
	fs, err = CheckModule(dir, []string{"./..."}, map[string][]byte{user: []byte(brokenUser)}, nw)
	if err != nil || len(fs) == 0 {
		t.Fatalf("code that does not compile: findings %v, err %v; want them reported", fs, err)
	}
	for _, f := range fs {
		if f.Rule != "check" || f.Pos.Filename != user || f.Pos.Line != 5 {
			t.Errorf("finding %v, want a check finding at user.go:5", f)
		}
	}
}

// With no new API to check against (a command built from a checkout, no -bide), the run says it
// was not checked: that is a finding, so the exit status is not 0.
func TestCheckModule_UncheckedIsAFinding(t *testing.T) {
	t.Setenv("GOWORK", "off")
	dir := t.TempDir()
	mod := "module example.com/user\n\ngo 1.27\n\nrequire github.com/bide-ai/bide v0.10.0\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o644); err != nil {
		t.Fatal(err)
	}
	fs, err := CheckModule(dir, []string{"./..."}, nil, "")
	if err != nil || len(fs) != 1 || !strings.Contains(fs[0].Msg, "not type-checked") {
		t.Fatalf("findings %v, err %v; want one saying the code was not checked", fs, err)
	}
}

// bide's own modules build against the tree they are in: the check needs no -bide there.
func TestCheckModule_BideItself(t *testing.T) {
	t.Setenv("GOWORK", "off")
	nw := copyTree(t, "testdata/newapi")
	f := filepath.Join(nw, "cases", "x", "x.go")
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, []byte(strings.Replace(brokenUser, "package user", "package x", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	fs, err := CheckModule(nw, []string{"./cases/..."}, nil, "")
	if err != nil || len(fs) == 0 || fs[0].Pos.Line != 5 {
		t.Fatalf("findings %v, err %v; want the error at x.go:5", fs, err)
	}
}

// A user's docs name bide's packages without importing them, as bide's own docs do: the
// markdown mode resolves them through the bide modules the user's module requires (it used to
// resolve only the module's own packages, and left such blocks alone, silently).
func TestMigrateMarkdown_UserModuleResolvesBide(t *testing.T) {
	t.Setenv("GOWORK", "off")
	dir := userModule(t, "package user\n")
	md := filepath.Join(dir, "README.md")
	doc := "# Use\n\n<!-- docsnip: setup model agent.Model; journal *agent.Journal -->\n```go\na, err := agent.Build(model, journal)\n```\n"
	if err := os.WriteFile(md, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, err := MigrateMarkdown(dir, []string{md}, Rules())
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out[md]); !strings.Contains(got, "agent.New(model, journal)") {
		t.Fatalf("the block was not rewritten:\n%s", got)
	}
}

// A rebase brings old call sites into code already migrated: the old New, used as one value,
// no longer type-checks against the new New (two results), and is still recognised and rewritten.
func TestRebase_OldNewAsOneValue(t *testing.T) {
	t.Setenv("GOWORK", "off")
	nw := copyTree(t, "testdata/newapi")
	f := filepath.Join(nw, "cases", "rebase", "rebase_test.go")
	src := `package rebase

import (
	"testing"

	"github.com/bide-ai/bide/agent"
)

func TestRebase(t *testing.T) {
	var m agent.Model
	j, _ := agent.NewJournal(agent.NewMemStore())
	a := agent.New(m, j)
	_ = a
}
`
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, err := MigrateModule(nw, []string{"./cases/rebase/..."}, Rules())
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out[f]); !strings.Contains(got, "a := agenttest.MustNew(m, j)") {
		t.Fatalf("the old New was not rewritten:\n%s", got)
	}
}

// The migrated cases with CRLF line endings (a Windows checkout) are left alone by a second run:
// gofmt writes LF, which alone is no change (a rule's edit that writes the same text, as the
// rename rule's can, used to count as one).
func TestIdempotent_CRLF(t *testing.T) {
	t.Setenv("GOWORK", "off")
	nw := copyTree(t, "testdata/newapi")
	err := filepath.WalkDir("testdata/golden/cases", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.Contains(p, string(filepath.Separator)+"manual"+string(filepath.Separator)) {
			return err
		}
		rel, _ := filepath.Rel("testdata/golden", strings.TrimSuffix(p, ".golden"))
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		dst := filepath.Join(nw, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n")), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := MigrateModule(nw, []string{"./cases/..."}, Rules())
	if err != nil {
		t.Fatal(err)
	}
	for p := range out {
		t.Errorf("a second run rewrote %s", p)
	}
}
