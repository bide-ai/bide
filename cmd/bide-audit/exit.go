package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/bide-ai/bide/audit"
)

// The exit statuses. Only exitVerified means verified; a script must treat every other status as a
// failure. exitUnusable in particular is never "retry later": a tampered "format" field yields it.
const (
	exitVerified    = 0 // verified (a produce verb: the artifact was written)
	exitNotVerified = 1 // the inputs were read and understood, and they do not verify
	exitUsage       = 2 // a bad flag or argument; nothing was read
	exitNoVerdict   = 3 // no verdict: the -checker gave none, or an internal error
	exitUnusable    = 4 // an input is unreadable or unusable
)

// The error classes a verb reports. exitFor maps them to exit statuses.
var (
	// errUsage is a command line the CLI does not read in full. Nothing is read after it.
	errUsage = errors.New("usage error")
	// errNotVerified is a verdict: the inputs were read and understood, and they do not hold.
	errNotVerified = errors.New("not verified")
	// errUnusable is an input the CLI cannot read or use: a missing or unreadable file, one over
	// -max-input-bytes, JSON that does not read strictly, an artifact of an unsupported format, or
	// an artifact of the wrong type for the flag it was given to.
	errUnusable = errors.New("input unusable")
	// errNoVerdict is a check that ran but gave no verdict: the -checker failed, or the CLI itself
	// did (an output it could not write, an internal error).
	errNoVerdict = errors.New("no verdict")
)

// exitFor maps a verb's error to its exit status. It is the only place a status is chosen, and it
// is written in precedence order: a usage error is exclusive (a verb that reports one reads nothing
// else), and otherwise a not-verified verdict outranks an unusable input, which outranks no
// verdict. errors.Is searches every branch of a joined error, so a verb that met several
// conditions reports the highest-ranked of them. An error of no known class is an internal error:
// no verdict, never 0.
func exitFor(err error) int {
	switch {
	case err == nil:
		return exitVerified
	case errors.Is(err, errUsage):
		return exitUsage
	case errors.Is(err, errNotVerified):
		return exitNotVerified
	case errors.Is(err, errUnusable), errors.Is(err, audit.ErrFormat),
		errors.Is(err, fs.ErrNotExist), errors.Is(err, fs.ErrPermission):
		return exitUnusable
	default:
		return exitNoVerdict
	}
}

// cliError is an error with its class. shown is set when the verb already printed it as a FAIL or
// ERROR line, so it is not printed again as "error: ...".
type cliError struct {
	class error
	err   error
	shown bool
}

func (e *cliError) Error() string   { return e.err.Error() }
func (e *cliError) Unwrap() []error { return []error{e.class, e.err} }

// unusable classes err as an unusable input. err stays in the chain, so a caller can still ask
// errors.Is(err, fs.ErrNotExist) or errors.Is(err, audit.ErrFormat).
func unusable(err error) error { return &cliError{class: errUnusable, err: err} }

// internal classes err as no verdict. The cause is flattened to its message, so a file-system
// error on an output (a -out the CLI could not write) is not mistaken for an unusable input.
func internal(err error) error {
	return &cliError{class: errNoVerdict, err: errors.New(err.Error())}
}

// abort unwinds a verb once its outcome is decided: a usage error, or a not-verified verdict,
// which nothing found later can outrank.
type abort struct{}

// cli is one invocation: where its output goes and the errors its verb has met.
type cli struct {
	stdout, stderr io.Writer
	out            io.Writer // the verb's report lines: stdout, or a buffer under -json
	buf            bytes.Buffer
	json           bool
	verb           string
	errs           []error
	artifact       json.RawMessage // a produce verb's artifact, embedded in the -json report
}

func (c *cli) printf(format string, args ...any) { fmt.Fprintf(c.out, format, args...) }
func (c *cli) println(args ...any)               { fmt.Fprintln(c.out, args...) }

// note records err, if any, and reports whether it was nil. A not-verified err decides the
// outcome, so note unwinds the verb.
func (c *cli) note(err error) bool {
	if err == nil {
		return true
	}
	c.errs = append(c.errs, err)
	if errors.Is(err, errNotVerified) {
		panic(abort{})
	}
	return false
}

// clean reports whether the verb has met no error so far.
func (c *cli) clean() bool { return len(c.errs) == 0 }

// fail prints a "FAIL: ..." verdict line and unwinds the verb with a not-verified verdict.
func (c *cli) fail(format string, args ...any) {
	c.failLine("FAIL: " + fmt.Sprintf(format, args...))
}

// failLine prints line as the verdict and unwinds the verb with a not-verified verdict.
func (c *cli) failLine(line string) {
	c.println(line)
	c.note(&cliError{class: errNotVerified, err: errors.New(line), shown: true})
}

// usageError prints msg (if any) and the usage text, and unwinds the verb with a usage error.
func (c *cli) usageError(msg string) {
	if msg != "" {
		fmt.Fprintln(c.stderr, msg)
	}
	fmt.Fprint(c.stderr, usageText)
	c.errs = append(c.errs, &cliError{class: errUsage, err: errors.New("usage error"), shown: true})
	panic(abort{})
}

// call runs a verb and returns every error it met, joined. A panic other than abort is a bug in the
// CLI: it is reported as an internal error, no verdict, never as a status a script could read as a
// verdict (Go's own exit status for a panic is 2, the usage status).
func (c *cli) call(verb func([]string), args []string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(abort); !ok {
				c.errs = append(c.errs, internal(fmt.Errorf("internal error: %v\n%s", r, debug.Stack())))
			}
		}
		err = errors.Join(c.errs...)
	}()
	verb(args)
	return nil
}

// flagSet returns a verb's flag set, with the -max-input-bytes, -max-clock-skew and -json flags
// every verb shares.
func (c *cli) flagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	fs.Int64Var(&maxInputBytes, "max-input-bytes", defaultMaxInputBytes, "the largest input file, in bytes, the verb reads; a larger one is an error")
	fs.DurationVar(&maxClockSkew, "max-clock-skew", audit.DefaultClockSkew, "how far past this machine's clock a signed head's timestamp may be")
	fs.BoolVar(&c.json, "json", c.json, "print one JSON report on stdout instead of the text report")
	return fs
}

// parse parses a verb's flags and unwinds with a usage error unless it read the whole command line.
// Flag parsing stops at the first argument that is not a flag, so a stray argument would otherwise
// drop every flag after it (a -digest or -checker the auditor asked for) and the verb would still
// report a verdict. A help request verifies nothing, so it is a usage error as well rather than 0,
// which the verify verbs reserve for "verified".
func (c *cli) parse(fs *flag.FlagSet, args []string) {
	if err := fs.Parse(args); err != nil {
		c.errs = append(c.errs, &cliError{class: errUsage, err: err, shown: true}) // fs printed it
		panic(abort{})
	}
	if c.json {
		c.out = &c.buf // the report lines go into the JSON report, not onto stdout
	}
	if fs.NArg() > 0 {
		c.usageError(fmt.Sprintf("%s: unexpected argument %q: every input is a flag, and no flag after an argument is read", fs.Name(), fs.Arg(0)))
	}
	if maxInputBytes < 1 {
		c.usageError(fmt.Sprintf("%s: -max-input-bytes must be at least 1, got %d", fs.Name(), maxInputBytes))
	}
	if maxClockSkew < 0 {
		c.usageError(fmt.Sprintf("%s: -max-clock-skew must not be negative, got %s", fs.Name(), maxClockSkew))
	}
}

// verbs maps each verb to its function.
var verbs = map[string]func(*cli, []string){
	"prove":                  (*cli).prove,
	"verify":                 (*cli).verify,
	"verify-governance":      (*cli).verifyGovernance,
	"verify-governed-action": (*cli).verifyGovernedAction,
	"verify-convergence":     (*cli).verifyConvergence,
	"verify-quorum":          (*cli).verifyQuorum,
	"verify-run":             (*cli).verifyRun,
	"verify-evidence":        (*cli).verifyEvidence,
	"verify-approvals":       (*cli).verifyApprovals,
	"prove-absent":           (*cli).proveAbsent,
	"verify-absent":          (*cli).verifyAbsent,
}

// produceVerbs are the verbs whose exit 0 means "the artifact was written", not "verified".
var produceVerbs = map[string]bool{"prove": true, "prove-absent": true}

// run runs one command line (without the program name) and returns its exit status. Every status
// the CLI exits with is chosen here, by exitFor.
func run(args []string, stdout, stderr io.Writer) int {
	c := &cli{stdout: stdout, stderr: stderr, out: stdout}
	top := flag.NewFlagSet("bide-audit", flag.ContinueOnError)
	top.SetOutput(stderr)
	top.Usage = func() { fmt.Fprint(stderr, usageText) }
	showVersion := top.Bool("version", false, "print the version and the artifact formats this version reads")
	top.BoolVar(&c.json, "json", false, "print one JSON report on stdout instead of the text report")
	if err := top.Parse(args); err != nil {
		return exitFor(errUsage)
	}
	if *showVersion {
		if top.NArg() > 0 {
			fmt.Fprintf(stderr, "bide-audit: -version takes no verb or argument, got %q\n", top.Arg(0))
			return exitFor(errUsage)
		}
		printVersion(stdout, c.json)
		return exitVerified
	}
	if top.NArg() == 0 {
		fmt.Fprint(stderr, usageText)
		return exitFor(errUsage)
	}
	c.verb = top.Arg(0)
	verb, ok := verbs[c.verb]
	if !ok {
		fmt.Fprintf(stderr, "bide-audit: unknown verb %q\n", c.verb)
		fmt.Fprint(stderr, usageText)
		return exitFor(errUsage)
	}
	err := c.call(func(a []string) { verb(c, a) }, top.Args()[1:])
	code := exitFor(err)
	var msgs []string
	for _, e := range c.errs {
		msgs = append(msgs, e.Error())
		var ce *cliError
		if errors.As(e, &ce) && ce.shown {
			continue
		}
		fmt.Fprintln(stderr, "error:", e)
	}
	if c.json {
		c.writeReport(code, msgs)
	}
	return code
}

// report is the -json output: one object on stdout.
type report struct {
	Tool     string          `json:"tool"`
	Version  string          `json:"version"`
	Verb     string          `json:"verb"`
	ExitCode int             `json:"exit_code"`
	Result   string          `json:"result"`
	Verified bool            `json:"verified"`
	Output   []string        `json:"output"`
	Errors   []string        `json:"errors"`
	Artifact json.RawMessage `json:"artifact,omitempty"`
}

// resultName names an exit status in the -json report.
func resultName(verb string, code int) string {
	switch code {
	case exitVerified:
		if produceVerbs[verb] {
			return "produced"
		}
		return "verified"
	case exitNotVerified:
		return "not_verified"
	case exitUsage:
		return "usage_error"
	case exitUnusable:
		return "unusable_input"
	default:
		return "no_verdict"
	}
}

func (c *cli) writeReport(code int, errs []string) {
	out := []string{}
	for _, l := range strings.Split(strings.TrimRight(c.buf.String(), "\n"), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	if errs == nil {
		errs = []string{}
	}
	rep := report{
		Tool: "bide-audit", Version: toolVersion(), Verb: c.verb, ExitCode: code,
		Result: resultName(c.verb, code), Verified: code == exitVerified && !produceVerbs[c.verb],
		Output: out, Errors: errs, Artifact: c.artifact,
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil { // a report of strings and valid JSON always marshals
		panic(err)
	}
	fmt.Fprintln(c.stdout, string(b))
}

// version is the release version. A release build may set it with
// -ldflags "-X main.version=v1.2.3"; otherwise it is read from the build's module information,
// which the go command stamps from the VCS tag.
var version = ""

func toolVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "devel"
}

// artifactFormat is one artifact this version reads, and the format it must carry.
type artifactFormat struct {
	Artifact string `json:"artifact"`
	Format   string `json:"format"`
	Verbs    string `json:"verbs"`
}

// readFormats lists every formatted artifact a verb reads. An artifact of any other format is
// refused with exit 4.
var readFormats = []artifactFormat{
	{"proof bundle", audit.ProofFormat, "verify, verify-governed-action, verify-convergence, verify-quorum"},
	{"absence bundle", audit.AbsenceFormat, "verify-absent"},
	{"run certificate", audit.RunCertificateFormat, "verify-run"},
	{"evidence package", audit.EvidenceFormat, "verify-evidence, verify-approvals"},
}

func printVersion(w io.Writer, asJSON bool) {
	if asJSON {
		b, err := json.MarshalIndent(struct {
			Tool    string           `json:"tool"`
			Version string           `json:"version"`
			Go      string           `json:"go"`
			Formats []artifactFormat `json:"formats"`
		}{"bide-audit", toolVersion(), runtime.Version(), readFormats}, "", "  ")
		if err != nil {
			panic(err)
		}
		fmt.Fprintln(w, string(b))
		return
	}
	fmt.Fprintf(w, "bide-audit %s (%s)\nartifact formats read:\n", toolVersion(), runtime.Version())
	for _, f := range readFormats {
		fmt.Fprintf(w, "  %-17s %s  (%s)\n", f.Artifact+":", f.Format, f.Verbs)
	}
}
