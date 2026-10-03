package main

import (
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
	dir := userModule(t, "package user\n")
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
