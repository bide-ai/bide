// Package doctor is bide doctor: it checks the things a first run depends on (the Go toolchain,
// the bide version, SQLite, an optional Postgres DSN, model API key variables, the module cache
// and, inside a bide init project, that the project builds) and prints a result for each with a
// hint for fixing a failure. It never prints the value of an environment variable.
package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bide-ai/bide/internal/approveline"
	"github.com/bide-ai/bide/internal/term"
	"github.com/bide-ai/bide/store/postgres"
	"github.com/bide-ai/bide/store/sqlite"
)

// Status is the outcome of one check.
type Status int

const (
	// OK means the check passed.
	OK Status = iota
	// Warn means something is off, but a first run can go on.
	Warn
	// Fail means a first run will not work until this is fixed.
	Fail
	// Skip means the check did not apply, or could not run (offline).
	Skip
)

// Result is what one check found: its Status, a one-line Detail, and for a Warn or Fail a Fix.
type Result struct {
	Name   string
	Status Status
	Detail string
	Fix    string
}

// Env is everything the checks reach outside the process through, so a test can replace it.
type Env struct {
	Version string                                                                      // the bide release ("" for a build from source)
	Dir     string                                                                      // the directory a project check looks in
	Getenv  func(string) string                                                         // environment variables
	Run     func(ctx context.Context, dir, name string, args ...string) ([]byte, error) // run a command, combined output
	Latest  func(ctx context.Context) (string, error)                                   // the latest bide release tag
	Offline bool                                                                        // skip checks that need the network
	// PostgresEnv names the variable holding a Postgres DSN to try; "" skips the check.
	PostgresEnv string
}

// minGo is the oldest Go release bide builds with.
var minGo = [3]int{1, 27, 0}

// keyVars are the model API key variables bide's adapters and scaffolds read. Only whether one is
// set is ever reported.
var keyVars = []string{"OPENAI_API_KEY", "OPENROUTER_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY"}

// Main runs bide doctor with args (after "doctor") and returns the exit status: 0 when nothing
// failed (warnings are fine), 1 when a check failed, 2 for a usage error. version is the bide
// release ("" for a build from source).
func Main(version string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bide doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: bide doctor [-dir <project>] [-offline] [-postgres-env <var>]
       bide doctor -declarations <file>
       bide doctor -diff <old.json> <new.json>

Checks that a first run of bide can work, and prints one line per check with a hint to fix a
failure:

  Go            the toolchain is installed and is Go 1.27 or later (older is a warning where
                GOTOOLCHAIN=auto lets go fetch 1.27 itself, and a failure where it does not)
  bide          this bide against the latest release (skipped offline or on a build from source)
  SQLite        a journal can be created and written in a temporary directory
  Postgres      with the variable named by -postgres-env set, its DSN connects (the value is never printed)
  model keys    which of OPENAI_API_KEY, OPENROUTER_API_KEY, ANTHROPIC_API_KEY, GEMINI_API_KEY and
                GOOGLE_API_KEY are set (names only, never values); none is fine for the scripted demos
  module cache  GOPATH and the module cache are writable, so go mod tidy can download bide
  project       in a bide init project (go.mod and flow.yaml in -dir), the project builds

With -declarations, doctor reads the taint and cap declarations a host dumped from its agent (a
document of format bide.declarations.v1) and prints what a reader should look at: side-effecting tools
in an agent with no taint policy, exempt tools, cap-carrying tools, an escalation gate weaker than
need >= 2 with the requester excluded. With -diff it compares two such documents (the one on your main
branch and the one on your branch) and lists every change as WEAKENED, changed or strengthened: a
raised cap limit, a removed or renamed cap rule, a removed Untrusted, a new TaintExempt, a lower need,
a requester no longer excluded. It exits 1 if anything is weakened, so a pipeline can gate on it. A
document written by a newer version, or one whose digest is not the digest of what it declares, is
refused. Neither report runs the first-run checks.

Exit status is 0 when nothing failed (a warning does not fail), and 1 when a check failed. On a
terminal the output is colored; -color=never, or NO_COLOR set to any value, turns color off.

`)
		fs.PrintDefaults()
	}
	dir := fs.String("dir", ".", "the directory to look for a bide init project in")
	offline := fs.Bool("offline", false, "skip the checks that need the network (the latest release)")
	declsFile := fs.String("declarations", "", "report on the taint and cap declarations in `file` (a document the host dumps) instead of the first-run checks")
	diff := fs.Bool("diff", false, "compare two declarations documents, `old.json new.json`, and exit 1 if anything is weakened")
	pgEnv := fs.String("postgres-env", "BIDE_POSTGRES_DSN", "the environment variable holding a Postgres DSN to try; the check runs only if it is set")
	color := term.Flag(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	switch {
	case *diff && *declsFile != "":
		fmt.Fprintf(stderr, "%sbide doctor: -diff and -declarations are separate reports\n", term.For(stderr, *color).ErrorPrefix())
		return 2
	case *diff:
		if fs.NArg() != 2 {
			fmt.Fprintf(stderr, "%sbide doctor: -diff takes two files: old.json new.json\n", term.For(stderr, *color).ErrorPrefix())
			return 2
		}
		return reportDiff(stdout, stderr, term.For(stdout, *color), fs.Arg(0), fs.Arg(1))
	case fs.NArg() > 0:
		fmt.Fprintf(stderr, "%sbide doctor: unexpected argument %q\n", term.For(stderr, *color).ErrorPrefix(), fs.Arg(0))
		fs.Usage()
		return 2
	case *declsFile != "":
		return reportDeclarations(stdout, stderr, term.For(stdout, *color), *declsFile)
	}
	if *pgEnv != "" && !approveline.ValidEnvName(*pgEnv) {
		fmt.Fprintf(stderr, "%sbide doctor: %s\n", term.For(stderr, *color).ErrorPrefix(), approveline.NotAnEnvName)
		fs.Usage()
		return 2
	}
	env := Env{Version: version, Dir: *dir, Getenv: os.Getenv, Run: run, Latest: latestRelease, Offline: *offline, PostgresEnv: *pgEnv}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	results := Check(ctx, env)
	return Report(stdout, term.For(stdout, *color), results)
}

// Check runs every check and returns the results in the order they print.
func Check(ctx context.Context, env Env) []Result {
	return []Result{
		checkGo(ctx, env),
		checkVersion(ctx, env),
		checkSQLite(),
		checkPostgres(ctx, env),
		checkKeys(env),
		checkCache(ctx, env),
		checkProject(ctx, env),
	}
}

// width is the widest a report line gets, so it fits a 1200px terminal without wrapping.
const width = 120

// Report prints results and a summary line, and returns the exit status: 1 if any failed. Long
// details and fixes wrap under their own column, and the columns line up by visible width: a
// colored mark is two columns wide, so a row without one is padded by two.
func Report(w io.Writer, st term.Style, results []Result) int {
	var ok, warn, fail, skip int
	pad := ""
	if st.On() {
		pad = "  "
	}
	const nameCol = 4 + 2 // the label and the gap before the name
	const detailCol = nameCol + 13 + 1
	for _, r := range results {
		var mark, label string
		switch r.Status {
		case OK:
			ok++
			mark, label = st.Pass(), st.OK("ok  ")
		case Warn:
			warn++
			mark, label = st.Bang(), st.Warn("warn")
		case Fail:
			fail++
			mark, label = st.Cross(), st.Fail("FAIL")
		case Skip:
			skip++
			mark, label = pad, st.Dim("skip")
		}
		indent := len(pad)
		fmt.Fprintf(w, "%s%s  %-13s %s\n", mark, label, r.Name, wrap(r.Detail, indent+detailCol, width))
		if r.Fix != "" {
			fmt.Fprintf(w, "%s      %s %s\n", pad, st.Dim("fix:"), wrap(r.Fix, indent+nameCol+len("fix: "), width))
		}
	}
	summary := fmt.Sprintf("%d ok, %d warning(s), %d failed, %d skipped", ok, warn, fail, skip)
	if fail > 0 {
		fmt.Fprintf(w, "\n%s%s  %s\n", st.Cross(), st.BoldFail("FAILED"), summary)
		return 1
	}
	fmt.Fprintf(w, "\n%s%s  %s\n", st.Pass(), st.BoldOK("READY"), summary)
	return 0
}

// wrap breaks text at spaces so that, printed after col columns, no line passes max. The next
// lines are indented to col. A word longer than the room stays whole.
func wrap(text string, col, max int) string {
	room := max - col
	var b strings.Builder
	n := 0
	for i, word := range strings.Fields(text) {
		wl := utf8.RuneCountInString(word)
		switch {
		case i == 0:
		case n+1+wl > room:
			b.WriteString("\n" + strings.Repeat(" ", col))
			n = 0
		default:
			b.WriteByte(' ')
			n++
		}
		b.WriteString(word)
		n += wl
	}
	return b.String()
}

var goVersionRE = regexp.MustCompile(`go(\d+)\.(\d+)(?:\.(\d+))?`)

// parseGo returns the version in the output of go version ("go version go1.27.0 darwin/arm64").
func parseGo(out string) ([3]int, bool) {
	m := goVersionRE.FindStringSubmatch(out)
	if m == nil {
		return [3]int{}, false
	}
	var v [3]int
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v, true
}

func less(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func checkGo(ctx context.Context, env Env) Result {
	r := Result{Name: "Go"}
	out, err := env.Run(ctx, "", "go", "version")
	if err != nil {
		r.Status, r.Detail = Fail, "the go command did not run: "+firstLine(string(out), err)
		r.Fix = "install Go 1.27 or later from https://go.dev/dl/ and make sure go is on your PATH"
		return r
	}
	v, ok := parseGo(string(out))
	switch {
	case !ok:
		r.Status, r.Detail = Warn, "could not read the version from: "+firstLine(string(out), nil)
		r.Fix = "check that go version prints go1.27 or later"
	case less(v, minGo) && canFetchToolchain(ctx, env):
		r.Status = Warn
		r.Detail = fmt.Sprintf("Go %d.%d.%d is older than 1.27: GOTOOLCHAIN=auto fetches 1.27 in a bide project", v[0], v[1], v[2])
		r.Fix = "to use 1.27 everywhere, install it from https://go.dev/dl/ or set GOTOOLCHAIN=go1.27.0"
	case less(v, minGo):
		r.Status, r.Detail = Fail, fmt.Sprintf("Go %d.%d.%d is older than Go 1.27, and GOTOOLCHAIN does not let go fetch a newer one", v[0], v[1], v[2])
		r.Fix = "install Go 1.27 or later from https://go.dev/dl/, or set GOTOOLCHAIN=go1.27.0 (bide's flow builder uses generic methods, which older Go does not have)"
	default:
		r.Status, r.Detail = OK, fmt.Sprintf("Go %d.%d.%d", v[0], v[1], v[2])
	}
	return r
}

// canFetchToolchain reports whether go may download a newer toolchain when a go.mod asks for one:
// GOTOOLCHAIN is auto, or ends in +auto. A pinned or local value, or one go cannot report, may not.
func canFetchToolchain(ctx context.Context, env Env) bool {
	out, err := env.Run(ctx, "", "go", "env", "GOTOOLCHAIN")
	if err != nil {
		return false
	}
	tc := strings.TrimSpace(string(out))
	return tc == "auto" || strings.HasSuffix(tc, "+auto")
}

func checkVersion(ctx context.Context, env Env) Result {
	r := Result{Name: "bide"}
	have := env.Version
	if have == "" {
		r.Status, r.Detail = Skip, "built from source: no release to compare"
		return r
	}
	if strings.ContainsAny(have, "-+") {
		r.Status, r.Detail = Skip, have+" is a development build: no release to compare"
		return r
	}
	if env.Offline {
		r.Status, r.Detail = Skip, have+" (offline: latest release not checked)"
		return r
	}
	lctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	latest, err := env.Latest(lctx)
	if err != nil {
		r.Status, r.Detail = Skip, have+" (the latest release could not be reached, so it was not checked)"
		return r
	}
	hv, ok1 := parseSemver(have)
	lv, ok2 := parseSemver(latest)
	switch {
	case !ok1 || !ok2:
		r.Status, r.Detail = Skip, fmt.Sprintf("%s (latest release %s; versions not comparable)", have, latest)
	case less(hv, lv):
		r.Status, r.Detail = Warn, fmt.Sprintf("%s is behind the latest release %s", have, latest)
		r.Fix = "brew upgrade bide-ai/tap/bide, or go install github.com/bide-ai/bide/cmd/bide@latest"
	default:
		r.Status, r.Detail = OK, have+" (latest)"
	}
	return r
}

var semverRE = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)`)

func parseSemver(s string) ([3]int, bool) {
	m := semverRE.FindStringSubmatch(s)
	if m == nil {
		return [3]int{}, false
	}
	var v [3]int
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v, true
}

// latestRelease asks GitHub for the latest bide release tag.
func latestRelease(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/bide-ai/bide/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github answered %s", resp.Status)
	}
	var v struct {
		Tag string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v); err != nil {
		return "", err
	}
	return v.Tag, nil
}

func checkSQLite() Result {
	r := Result{Name: "SQLite"}
	dir, err := os.MkdirTemp("", "bide-doctor-*")
	if err != nil {
		r.Status, r.Detail = Fail, "cannot create a temporary directory: "+err.Error()
		r.Fix = "make the temporary directory writable (TMPDIR), or free disk space"
		return r
	}
	defer os.RemoveAll(dir)
	s, err := sqlite.Open(filepath.Join(dir, "doctor.db"))
	if err != nil {
		r.Status, r.Detail = Fail, "cannot open a journal: "+err.Error()
		r.Fix = "the journal needs a local, writable disk (SQLite's locking does not work over NFS); check TMPDIR"
		return r
	}
	defer s.Close()
	if _, _, err := s.Insert(context.Background(), "doctor", "check", []byte(`{}`)); err != nil {
		r.Status, r.Detail = Fail, "cannot write to a journal: "+err.Error()
		r.Fix = "check disk space and that the temporary directory is on a local disk"
		return r
	}
	r.Status, r.Detail = OK, "a journal can be created and written"
	return r
}

func checkPostgres(ctx context.Context, env Env) Result {
	r := Result{Name: "Postgres"}
	if env.PostgresEnv == "" || env.Getenv(env.PostgresEnv) == "" {
		name := env.PostgresEnv
		if name == "" {
			name = "(none named)"
		}
		r.Status, r.Detail = Skip, "no DSN in "+name+": SQLite needs no server"
		return r
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	s, err := postgres.Open(cctx, env.Getenv(env.PostgresEnv))
	if err != nil {
		r.Status = Fail
		// The DSN may hold a password: report the error class, not the text a driver echoes.
		r.Detail = "the DSN in " + env.PostgresEnv + " did not connect"
		r.Fix = "check the host, port, user, password, database and sslmode in the DSN, and that the server is running"
		return r
	}
	s.Close()
	r.Status, r.Detail = OK, "the DSN in "+env.PostgresEnv+" connects"
	return r
}

func checkKeys(env Env) Result {
	r := Result{Name: "model keys"}
	var set []string
	for _, k := range keyVars {
		if env.Getenv(k) != "" {
			set = append(set, k)
		}
	}
	if len(set) == 0 {
		r.Status, r.Detail = Skip, "none set: fine, bide demo and bide init run on a scripted model"
		r.Fix = "for a real model, export OPENAI_API_KEY (or OPENROUTER_API_KEY, ANTHROPIC_API_KEY, GEMINI_API_KEY)"
		return r
	}
	r.Status, r.Detail = OK, "set: "+strings.Join(set, ", ")
	return r
}

func checkCache(ctx context.Context, env Env) Result {
	r := Result{Name: "module cache"}
	out, err := env.Run(ctx, "", "go", "env", "GOPATH", "GOMODCACHE")
	if err != nil {
		r.Status, r.Detail = Skip, "go env did not run, so GOPATH was not checked"
		return r
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var bad []string
	checked := 0
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		// GOPATH can be a list.
		for _, d := range filepath.SplitList(l) {
			checked++
			if err := writable(d); err != nil {
				bad = append(bad, d+" ("+err.Error()+")")
			}
		}
	}
	if len(bad) > 0 {
		r.Status, r.Detail = Fail, "not writable: "+strings.Join(bad, "; ")
		r.Fix = "make these directories writable by you, or point GOPATH / GOMODCACHE at ones that are (go env -w GOPATH=$HOME/go)"
		return r
	}
	r.Status, r.Detail = OK, fmt.Sprintf("%d director%s writable", checked, map[bool]string{true: "y", false: "ies"}[checked == 1])
	return r
}

// writable reports whether a file can be created in dir, or in the nearest parent that exists
// (go creates the directory itself).
func writable(dir string) error {
	for {
		fi, err := os.Stat(dir)
		if err == nil {
			if !fi.IsDir() {
				return errors.New("not a directory")
			}
			f, err := os.CreateTemp(dir, ".bide-doctor-*")
			if err != nil {
				return errors.New("cannot write there")
			}
			f.Close()
			os.Remove(f.Name())
			return nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return err
		}
		dir = parent
	}
}

func checkProject(ctx context.Context, env Env) Result {
	r := Result{Name: "project"}
	if !exists(filepath.Join(env.Dir, "go.mod")) || !exists(filepath.Join(env.Dir, "flow.yaml")) {
		r.Status, r.Detail = Skip, "no bide init project in "+env.Dir+" (go.mod and flow.yaml): run bide doctor inside one to check it builds"
		return r
	}
	bctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	out, err := env.Run(bctx, env.Dir, "go", "build", "./...")
	if err != nil {
		r.Status, r.Detail = Fail, "go build ./... failed: "+firstLine(string(out), err)
		r.Fix = "run go mod tidy first (it downloads bide and its dependencies), then go build ./... for the full error"
		return r
	}
	r.Status, r.Detail = OK, "go build ./... succeeds in "+env.Dir
	return r
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// firstLine is the first non-empty line of out, or err's text.
func firstLine(out string, err error) string {
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	if err != nil {
		return err.Error()
	}
	return "(no output)"
}

// run runs a command and returns its combined output.
func run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}
