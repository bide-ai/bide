package doctor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bide-ai/bide/internal/term"
)

// fakeEnv is an Env whose go command answers goOut, whose latest release is latest, and whose
// environment is vars.
func fakeEnv(t *testing.T, goOut string, vars map[string]string) Env {
	t.Helper()
	cache := t.TempDir()
	return Env{
		Version: "v0.15.0",
		Dir:     t.TempDir(),
		Getenv:  func(k string) string { return vars[k] },
		Run: func(_ context.Context, _, name string, args ...string) ([]byte, error) {
			if name == "go" && len(args) == 2 && args[0] == "env" && args[1] == "GOTOOLCHAIN" {
				if tc := vars["go env GOTOOLCHAIN"]; tc != "" {
					return []byte(tc + "\n"), nil
				}
				return []byte("auto\n"), nil
			}
			if name == "go" && len(args) > 0 && args[0] == "env" {
				return []byte(cache + "\n" + cache + "\n"), nil
			}
			if goOut == "" {
				return nil, errors.New(`exec: "go": executable file not found in $PATH`)
			}
			return []byte(goOut), nil
		},
		Latest:      func(context.Context) (string, error) { return "v0.15.0", nil },
		PostgresEnv: "BIDE_POSTGRES_DSN",
	}
}

func byName(rs []Result, name string) Result {
	for _, r := range rs {
		if r.Name == name {
			return r
		}
	}
	return Result{}
}

func TestGoVersion(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want Status
	}{
		{"go version go1.27.0 darwin/arm64", OK},
		{"go version go1.28.1 linux/amd64", OK},
		{"go version go1.26.4 linux/amd64", Warn}, // GOTOOLCHAIN=auto fetches 1.27 for a project
		{"", Fail}, // go not on PATH
	} {
		r := byName(Check(context.Background(), fakeEnv(t, tc.out, nil)), "Go")
		if r.Status != tc.want {
			t.Errorf("go version %q: status %v (%s), want %v", tc.out, r.Status, r.Detail, tc.want)
		}
		if r.Status == Fail && tc.out == "" && !strings.Contains(r.Fix, "go.dev/dl") {
			t.Errorf("go version %q: fix %q does not say where to get Go", tc.out, r.Fix)
		}
	}
}

func TestVersionCheck(t *testing.T) {
	env := fakeEnv(t, "go version go1.27.0 x/y", nil)
	env.Latest = func(context.Context) (string, error) { return "v0.16.0", nil }
	if r := checkVersion(context.Background(), env); r.Status != Warn || !strings.Contains(r.Fix, "brew upgrade") {
		t.Errorf("behind the latest: %+v", r)
	}
	env.Latest = func(context.Context) (string, error) { return "", errors.New("no network") }
	if r := checkVersion(context.Background(), env); r.Status != Skip {
		t.Errorf("network down: %+v; want Skip, not Fail", r)
	}
	env.Offline = true
	env.Latest = func(context.Context) (string, error) { t.Fatal("the network was used under -offline"); return "", nil }
	if r := checkVersion(context.Background(), env); r.Status != Skip {
		t.Errorf("offline: %+v", r)
	}
	env.Offline = false
	for _, v := range []string{"", "v0.0.0-20261013163946-12ba0949abff+dirty"} {
		env.Version = v
		if r := checkVersion(context.Background(), env); r.Status != Skip {
			t.Errorf("version %q (a source build): %+v; want Skip", v, r)
		}
	}
}

func TestSQLite(t *testing.T) {
	if r := checkSQLite(); r.Status != OK {
		t.Errorf("%+v", r)
	}
}

// Key variables are reported by name, and their values are never printed.
func TestKeysNamesOnly(t *testing.T) {
	const secret = "sk-this-must-never-be-printed"
	env := fakeEnv(t, "go version go1.27.0 x/y", map[string]string{"OPENAI_API_KEY": secret, "BIDE_POSTGRES_DSN": ""})
	var out bytes.Buffer
	Report(&out, term.New(false), Check(context.Background(), env))
	if strings.Contains(out.String(), secret) {
		t.Fatalf("a key value was printed:\n%s", out.String())
	}
	if r := byName(Check(context.Background(), env), "model keys"); r.Status != OK || !strings.Contains(r.Detail, "OPENAI_API_KEY") {
		t.Errorf("model keys: %+v", r)
	}
	if r := byName(Check(context.Background(), fakeEnv(t, "go version go1.27.0 x/y", nil)), "model keys"); r.Status != Skip {
		t.Errorf("no keys: %+v; want Skip (the scripted demos need none)", r)
	}
}

// A DSN may hold a password: a failed connection never prints it.
func TestPostgresNeverPrintsTheDSN(t *testing.T) {
	const dsn = "postgres://user:hunter2@127.0.0.1:1/db?sslmode=disable&connect_timeout=1"
	env := fakeEnv(t, "go version go1.27.0 x/y", map[string]string{"BIDE_POSTGRES_DSN": dsn})
	r := checkPostgres(context.Background(), env)
	if r.Status != Fail || strings.Contains(r.Detail+r.Fix, "hunter2") {
		t.Fatalf("%+v", r)
	}
}

func TestProject(t *testing.T) {
	env := fakeEnv(t, "go version go1.27.0 x/y", nil)
	if r := checkProject(context.Background(), env); r.Status != Skip {
		t.Errorf("no project: %+v", r)
	}
	for _, f := range []string{"go.mod", "flow.yaml"} {
		if err := os.WriteFile(filepath.Join(env.Dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if r := checkProject(context.Background(), env); r.Status != OK {
		t.Errorf("builds: %+v", r)
	}
	env.Run = func(context.Context, string, string, ...string) ([]byte, error) {
		return []byte("main.go:3:2: no required module provides package github.com/bide-ai/bide/agent\n"), errors.New("exit status 1")
	}
	if r := checkProject(context.Background(), env); r.Status != Fail || !strings.Contains(r.Fix, "go mod tidy") {
		t.Errorf("does not build: %+v; want Fail with go mod tidy", r)
	}
}

func TestReport(t *testing.T) {
	rs := []Result{
		{Name: "Go", Status: OK, Detail: "Go 1.27.0"},
		{Name: "SQLite", Status: Fail, Detail: "cannot open", Fix: "check TMPDIR"},
	}
	var plain bytes.Buffer
	if code := Report(&plain, term.New(false), rs); code != 1 {
		t.Errorf("exit %d with a failed check; want 1", code)
	}
	if strings.Contains(plain.String(), "\x1b") || !strings.Contains(plain.String(), "FAIL  SQLite") || !strings.Contains(plain.String(), "fix: check TMPDIR") {
		t.Errorf("plain report:\n%s", plain.String())
	}
	var color bytes.Buffer
	Report(&color, term.New(true), rs)
	if !strings.Contains(color.String(), "✓") || !strings.Contains(color.String(), "✗") {
		t.Errorf("colored report lacks the marks:\n%s", color.String())
	}
	var okOut bytes.Buffer
	if code := Report(&okOut, term.New(false), rs[:1]); code != 0 || !strings.Contains(okOut.String(), "READY") {
		t.Errorf("all ok: exit %d\n%s", code, okOut.String())
	}
}

func TestMainUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main("", []string{"-h"}, &out, &errb); code != 0 {
		t.Errorf("-h: %d", code)
	}
	if code := Main("", []string{"extra"}, &out, &errb); code != 2 {
		t.Errorf("an argument: %d; want 2", code)
	}
}

// TestOldGoByToolchain: an old Go only fails where it cannot fetch Go 1.27 itself, and the fix text
// gives the same two ways out as docs/getting-started.md.
func TestOldGoByToolchain(t *testing.T) {
	for _, tc := range []struct {
		toolchain string
		want      Status
	}{
		{"auto", Warn},
		{"go1.21+auto", Warn},
		{"local", Fail},
		{"go1.26.1", Fail},
		{"go1.26.1+path", Fail},
	} {
		env := fakeEnv(t, "go version go1.26.4 linux/amd64", map[string]string{"go env GOTOOLCHAIN": tc.toolchain})
		r := byName(Check(context.Background(), env), "Go")
		if r.Status != tc.want {
			t.Errorf("GOTOOLCHAIN=%s: status %v (%s), want %v", tc.toolchain, r.Status, r.Detail, tc.want)
		}
		if tc.want == Fail && (!strings.Contains(r.Fix, "GOTOOLCHAIN=go1.27.0") || !strings.Contains(r.Fix, "go.dev/dl")) {
			t.Errorf("GOTOOLCHAIN=%s: fix %q", tc.toolchain, r.Fix)
		}
		if tc.want == Warn && !strings.Contains(r.Detail, "GOTOOLCHAIN") {
			t.Errorf("GOTOOLCHAIN=%s: detail %q does not say why it is only a warning", tc.toolchain, r.Detail)
		}
	}
}

var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

// TestReportLayout: on a color terminal the check names line up whatever the mark, and no line is
// wider than 120 columns, even a long fix or a long directory.
func TestReportLayout(t *testing.T) {
	long := strings.Repeat("a long sentence that runs on ", 12)
	rs := []Result{
		{Name: "Go", Status: OK, Detail: "Go 1.27.0"},
		{Name: "bide", Status: Skip, Detail: "built from source"},
		{Name: "SQLite", Status: Warn, Detail: long, Fix: long},
		{Name: "module cache", Status: Fail, Detail: "x", Fix: long},
		{Name: "Postgres", Status: Skip, Detail: "no DSN"},
	}
	for _, on := range []bool{false, true} {
		var b bytes.Buffer
		Report(&b, term.New(on), rs)
		col := -1 // reset per mode
		for _, l := range strings.Split(strings.TrimRight(ansiRE.ReplaceAllString(b.String(), ""), "\n"), "\n") {
			if w := utf8.RuneCountInString(l); w > 120 {
				t.Errorf("color=%v: %d columns: %q", on, w, l)
			}
			for _, name := range []string{"Go ", "bide ", "SQLite ", "module cache ", "Postgres "} {
				if i := strings.Index(l, "  "+name); i >= 0 && !strings.Contains(l, "fix:") {
					i = utf8.RuneCountInString(l[:i]) // columns, not bytes
					if col >= 0 && i != col {
						t.Errorf("color=%v: name %q at column %d, others at %d: %q", on, name, i, col, l)
					}
					col = i
				}
			}
		}
	}
}
