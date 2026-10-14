package doctor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestLatestRelease: the latest release is the tag GitHub's API names; any other answer is an
// error, never an empty or guessed tag. The transport is replaced, so nothing leaves the process.
func TestLatestRelease(t *testing.T) {
	old := http.DefaultClient.Transport
	t.Cleanup(func() { http.DefaultClient.Transport = old })
	answer := func(code int, body string) {
		http.DefaultClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != "/repos/bide-ai/bide/releases/latest" || r.Header.Get("Accept") != "application/vnd.github+json" {
				t.Errorf("request %s with Accept %q", r.URL, r.Header.Get("Accept"))
			}
			return &http.Response{StatusCode: code, Status: http.StatusText(code), Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
		})
	}
	answer(200, `{"tag_name":"v0.16.0","name":"ignored"}`)
	if tag, err := latestRelease(context.Background()); err != nil || tag != "v0.16.0" {
		t.Errorf("latestRelease = %q, %v", tag, err)
	}
	answer(403, `{"message":"rate limited"}`)
	if _, err := latestRelease(context.Background()); err == nil || !strings.Contains(err.Error(), "github answered") {
		t.Errorf("a 403: %v", err)
	}
	answer(200, `not json`)
	if _, err := latestRelease(context.Background()); err == nil {
		t.Error("a body that is not JSON succeeded")
	}
	http.DefaultClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("no route") })
	if _, err := latestRelease(context.Background()); err == nil || !strings.Contains(err.Error(), "no route") {
		t.Errorf("a transport failure: %v", err)
	}
}

// TestVersionCheckBranches: each way the release check ends, with its status and words.
func TestVersionCheckBranches(t *testing.T) {
	latest := func(tag string, err error) func(context.Context) (string, error) {
		return func(context.Context) (string, error) { return tag, err }
	}
	for _, c := range []struct {
		name    string
		version string
		offline bool
		latest  func(context.Context) (string, error)
		status  Status
		detail  string
	}{
		{"source build", "", false, nil, Skip, "built from source"},
		{"development build", "v0.16.0-dev", false, nil, Skip, "development build"},
		{"offline", "v0.15.0", true, nil, Skip, "offline"},
		{"unreachable", "v0.15.0", false, latest("", errors.New("down")), Skip, "could not be reached"},
		{"incomparable", "v0.15.0", false, latest("nightly", nil), Skip, "versions not comparable"},
		{"behind", "v0.15.0", false, latest("v0.16.0", nil), Warn, "v0.15.0 is behind the latest release v0.16.0"},
		{"current", "v0.16.0", false, latest("v0.16.0", nil), OK, "v0.16.0 (latest)"},
		{"ahead", "v0.17.0", false, latest("v0.16.0", nil), OK, "(latest)"},
	} {
		r := checkVersion(context.Background(), Env{Version: c.version, Offline: c.offline, Latest: c.latest})
		if r.Status != c.status || !strings.Contains(r.Detail, c.detail) {
			t.Errorf("%s: %+v, want %v containing %q", c.name, r, c.status, c.detail)
		}
		if (c.status == Warn) != (r.Fix != "") {
			t.Errorf("%s: fix %q (a warning gives one, an ok or skip does not)", c.name, r.Fix)
		}
	}
}

// TestSQLiteUnwritableTemp: with no usable temporary directory the check fails and says to fix
// TMPDIR, rather than passing without writing a journal.
func TestSQLiteUnwritableTemp(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "gone"))
	r := checkSQLite()
	if r.Status != Fail || !strings.Contains(r.Detail, "temporary directory") || !strings.Contains(r.Fix, "TMPDIR") {
		t.Errorf("%+v", r)
	}
}

// TestPostgresSkipped: with no variable named, or none set, the check is skipped and says which.
func TestPostgresSkipped(t *testing.T) {
	env := fakeEnv(t, "go version go1.27.0 x/y", nil)
	env.PostgresEnv = ""
	if r := checkPostgres(context.Background(), env); r.Status != Skip || !strings.Contains(r.Detail, "(none named)") {
		t.Errorf("no variable named: %+v", r)
	}
	env.PostgresEnv = "MY_DSN"
	if r := checkPostgres(context.Background(), env); r.Status != Skip || !strings.Contains(r.Detail, "no DSN in MY_DSN") {
		t.Errorf("variable unset: %+v", r)
	}
}

// TestCacheCheck: GOPATH and GOMODCACHE are checked, a GOPATH list entry by entry; a directory that
// does not exist yet passes when its parent is writable; one that is a file fails by name.
func TestCacheCheck(t *testing.T) {
	root := t.TempDir()
	env := fakeEnv(t, "go version go1.27.0 x/y", nil)
	goEnv := func(out string, err error) {
		env.Run = func(context.Context, string, string, ...string) ([]byte, error) { return []byte(out), err }
	}

	goEnv(root+"\n"+filepath.Join(root, "not-yet", "pkg", "mod")+"\n", nil)
	if r := checkCache(context.Background(), env); r.Status != OK || r.Detail != "2 directories writable" {
		t.Errorf("a missing directory under a writable one: %+v", r)
	}
	goEnv(root+"\n\n", nil)
	if r := checkCache(context.Background(), env); r.Status != OK || r.Detail != "1 directory writable" {
		t.Errorf("one directory: %+v", r)
	}
	list := root + string(os.PathListSeparator) + root
	goEnv(list+"\n"+root+"\n", nil)
	if r := checkCache(context.Background(), env); r.Detail != "3 directories writable" {
		t.Errorf("a GOPATH list counts each entry: %+v", r)
	}

	file := filepath.Join(root, "afile")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	goEnv(root+"\n"+file+"\n", nil)
	r := checkCache(context.Background(), env)
	if r.Status != Fail || !strings.Contains(r.Detail, file+" (not a directory)") || strings.Contains(r.Detail, root+" (") || !strings.Contains(r.Fix, "go env -w GOPATH") {
		t.Errorf("a file where a directory belongs: %+v", r)
	}

	goEnv("", errors.New("no go"))
	if r := checkCache(context.Background(), env); r.Status != Skip || !strings.Contains(r.Detail, "go env did not run") {
		t.Errorf("go env fails: %+v", r)
	}
}

// TestWritable: a directory is writable when a file can be created in it, a read-only one is not,
// and a path through a file is not a directory.
func TestWritable(t *testing.T) {
	dir := t.TempDir()
	if err := writable(dir); err != nil {
		t.Errorf("a temp dir: %v", err)
	}
	if err := writable(filepath.Join(dir, "a", "b")); err != nil {
		t.Errorf("a path below a writable dir: %v", err)
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, ".bide-doctor-*"))
	if len(leftovers) != 0 {
		t.Errorf("the probe left %v behind", leftovers)
	}
	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		if err := writable(ro); err == nil || err.Error() != "cannot write there" {
			t.Errorf("a read-only dir: %v", err)
		}
	}
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writable(file); err == nil || err.Error() != "not a directory" {
		t.Errorf("a file: %v", err)
	}
}

// TestFirstLine: the first non-empty line of the output, else the error's text, else a note.
func TestFirstLine(t *testing.T) {
	boom := errors.New("exit status 1")
	for _, c := range []struct {
		out  string
		err  error
		want string
	}{
		{"\n  \n# pkg\nmore\n", boom, "# pkg"},
		{"", boom, "exit status 1"},
		{" \n", nil, "(no output)"},
	} {
		if got := firstLine(c.out, c.err); got != c.want {
			t.Errorf("firstLine(%q, %v) = %q, want %q", c.out, c.err, got, c.want)
		}
	}
}

// TestProjectBuildFails: in a project directory a failing build is reported by its first line and
// points at go mod tidy.
func TestProjectBuildFails(t *testing.T) {
	env := fakeEnv(t, "go version go1.27.0 x/y", nil)
	for _, f := range []string{"go.mod", "flow.yaml"} {
		if err := os.WriteFile(filepath.Join(env.Dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var gotDir string
	var gotArgs []string
	env.Run = func(_ context.Context, dir, name string, args ...string) ([]byte, error) {
		gotDir, gotArgs = dir, append([]string{name}, args...)
		return []byte("\nmain.go:3:1: missing go.sum entry\nmore\n"), errors.New("exit status 1")
	}
	r := checkProject(context.Background(), env)
	if r.Status != Fail || r.Detail != "go build ./... failed: main.go:3:1: missing go.sum entry" || !strings.Contains(r.Fix, "go mod tidy") {
		t.Errorf("%+v", r)
	}
	if gotDir != env.Dir || strings.Join(gotArgs, " ") != "go build ./..." {
		t.Errorf("ran %v in %q, want go build ./... in the project", gotArgs, gotDir)
	}
	env.Run = func(context.Context, string, string, ...string) ([]byte, error) { return nil, nil }
	if r := checkProject(context.Background(), env); r.Status != OK || !strings.Contains(r.Detail, env.Dir) {
		t.Errorf("a build that passes: %+v", r)
	}
}

// TestRunCommand: run returns a command's combined output, in the directory given.
func TestRunCommand(t *testing.T) {
	out, err := run(context.Background(), t.TempDir(), "go", "env", "GOOS")
	if err != nil || strings.TrimSpace(string(out)) == "" {
		t.Errorf("go env GOOS = %q, %v", out, err)
	}
	if out, err := run(context.Background(), "", "go", "nosuchsubcommand"); err == nil || !strings.Contains(string(out), "unknown command") {
		t.Errorf("a failing command: %q, %v; want its output and error", out, err)
	}
	if _, err := run(context.Background(), "", "bide-no-such-program"); err == nil {
		t.Error("a missing program succeeded")
	}
}

// TestMainRuns: the command runs every check against the real environment and prints one line per
// check; its exit status is 1 exactly when a line is a failure.
func TestMainRuns(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-never-printed")
	var out, errb bytes.Buffer
	code := Main("", []string{"-offline", "-color=never", "-dir", t.TempDir(), "-postgres-env", ""}, &out, &errb)
	text := out.String()
	for _, name := range []string{"Go", "bide", "SQLite", "Postgres", "model keys", "module cache", "project"} {
		if !strings.Contains(text, name) {
			t.Errorf("no line for %q:\n%s", name, text)
		}
	}
	if strings.Contains(text, "sk-never-printed") || !strings.Contains(text, "OPENAI_API_KEY") {
		t.Errorf("model keys not reported by name only:\n%s", text)
	}
	if failed := strings.Contains(text, "FAIL"); failed != (code == 1) {
		t.Errorf("exit %d with failure lines %v:\n%s", code, failed, text)
	}
	if errb.Len() != 0 {
		t.Errorf("stderr: %s", errb.String())
	}
}

// TestGoCheckEdges: a go command that does not run fails with the install hint; output with no
// version is a warning, not a pass; a missing patch number reads as .0.
func TestGoCheckEdges(t *testing.T) {
	ctx := context.Background()
	env := fakeEnv(t, "", nil) // go not found
	if r := checkGo(ctx, env); r.Status != Fail || !strings.Contains(r.Detail, "the go command did not run") || !strings.Contains(r.Fix, "https://go.dev/dl/") {
		t.Errorf("go missing: %+v", r)
	}
	if r := checkGo(ctx, fakeEnv(t, "something unrelated\n", nil)); r.Status != Warn || !strings.Contains(r.Detail, "could not read the version from: something unrelated") {
		t.Errorf("unreadable version: %+v", r)
	}
	if r := checkGo(ctx, fakeEnv(t, "go version go1.28 linux/amd64", nil)); r.Status != OK || r.Detail != "Go 1.28.0" {
		t.Errorf("go1.28: %+v", r)
	}
	if _, ok := parseGo("go version devel +abc"); ok {
		t.Error("parseGo read a version from a devel build")
	}
	if v, ok := parseGo("go version go1.27rc1 linux/amd64"); !ok || v != [3]int{1, 27, 0} {
		t.Errorf("parseGo of a release candidate = %v, %v", v, ok)
	}
}

// TestToolchainFetch: go may fetch a newer toolchain when GOTOOLCHAIN is auto or ends in +auto,
// not when it is pinned or local, and not when go cannot say.
func TestToolchainFetch(t *testing.T) {
	ask := func(out string, err error) bool {
		env := Env{Run: func(context.Context, string, string, ...string) ([]byte, error) { return []byte(out), err }}
		return canFetchToolchain(context.Background(), env)
	}
	for out, want := range map[string]bool{"auto\n": true, "go1.26.0+auto\n": true, "local\n": false, "go1.27.0\n": false, "": false} {
		if got := ask(out, nil); got != want {
			t.Errorf("GOTOOLCHAIN=%q: %v, want %v", strings.TrimSpace(out), got, want)
		}
	}
	if ask("auto", errors.New("go env failed")) {
		t.Error("a failing go env was taken as auto")
	}
}

// TestWrap: long text breaks at spaces so no line passes the width, later lines are indented to
// the column, and a word longer than the room stays whole.
func TestWrap(t *testing.T) {
	got := wrap("one two three four five six", 4, 16)
	for i, line := range strings.Split(got, "\n") {
		if i > 0 && !strings.HasPrefix(line, "    ") {
			t.Errorf("line %d %q is not indented to the column", i, line)
		}
		if len(line)+4 > 16 && i == 0 || len(line) > 16 {
			t.Errorf("line %d %q is past the width", i, line)
		}
	}
	if strings.Join(strings.Fields(got), " ") != "one two three four five six" {
		t.Errorf("wrapping changed the words: %q", got)
	}
	if got := wrap("short", 4, 80); got != "short" {
		t.Errorf("short text = %q", got)
	}
	if got := wrap("supercalifragilisticexpialidocious end", 4, 20); !strings.HasPrefix(got, "supercalifragilisticexpialidocious\n    end") {
		t.Errorf("long word = %q", got)
	}
}
