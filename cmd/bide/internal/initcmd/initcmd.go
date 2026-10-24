// Package initcmd is bide init: it scaffolds a project that builds, runs with no API key and no
// network, and shows each part of bide once: a model call on a scripted model (with the lines that
// swap in a real one), a side-effect node behind a 2-of-3 approval gate, a SQLite journal, a flow
// config bide check reads, and the evidence bide view and bide audit read.
package initcmd

import (
	"embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bide-ai/bide/internal/term"
)

//go:embed template
var tmpl embed.FS

// files maps each template file to the file it becomes in the project.
var files = map[string]string{
	"main.go.tmpl":      "main.go",
	"main_test.go.tmpl": "main_test.go",
	"color.go.tmpl":     "color.go",
	"flow.yaml":         "flow.yaml",
	"README.md.tmpl":    "README.md",
	"gitignore.tmpl":    ".gitignore",
}

// agentFiles is files for bide init -agent: an agent with a taint policy instead of a plan flow.
var agentFiles = map[string]string{
	"agent/main.go.tmpl":      "main.go",
	"agent/main_test.go.tmpl": "main_test.go",
	"color.go.tmpl":           "color.go",
	"agent/README.md.tmpl":    "README.md",
	"gitignore.tmpl":          ".gitignore",
}

// gateText is the README's paragraph on the escalation gate, strict or single.
const (
	strictGateText = "  The escalation gate here is two of three signed decisions (`ops`, `finance`, `risk`), and the person the\n  run acts for (the customer, its requester) never counts toward them (`ExcludePrincipal`)."
	singleGateText = "  The escalation gate here is ONE unsigned decision (`bide init -single-approver`, for a team of one). That is\n  the weaker choice: nothing is signed, and the person whose input provoked the call is not kept from\n  approving it. Use `agent.ApprovalPolicy{Need: 2, Approvers: ..., ExcludePrincipal: true}` as soon as you\n  have a second person."
)

// placeholders replaces the template's markers in a single pass.
func placeholders(name, single, gateText string) *strings.Replacer {
	return strings.NewReplacer("__NAME__", name, "__SINGLE__", single, "__GATE_TEXT__", gateText)
}

// placeholderNames are the markers a template holds; a project name that spells one is refused, so what the
// project says about itself is never a marker's replacement by accident.
var placeholderNames = []string{"__NAME__", "__SINGLE__", "__GATE_TEXT__"}

// checkName refuses a name that is not a module path (see modName) whose elements Go would take: none empty,
// none starting or ending with a dot, none with "..", and none spelling a template marker.
func checkName(name string) error {
	for _, p := range placeholderNames {
		if strings.Contains(name, p) {
			return fmt.Errorf("%q spells %s, a marker of the project template; name the project another way with -module", name, p)
		}
	}
	if !modName.MatchString(name) {
		return fmt.Errorf("%q is not a module path; name one with -module", name)
	}
	for _, e := range strings.Split(name, "/") {
		if e == "" || strings.HasPrefix(e, ".") || strings.HasSuffix(e, ".") || strings.Contains(e, "..") {
			return fmt.Errorf("%q is not a module path (the element %q is empty or begins or ends with a dot); name one with -module", name, e)
		}
	}
	return nil
}

// modName is what a module path may hold, for the name bide init derives from the directory.
var modName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~/-]*$`)

// Main runs bide init with args (after "init") and returns the exit status: 0 when the project was
// written or for -h, 1 if it could not be, 2 for a usage error. version is the version of bide that
// runs ("" or "(devel)" for a build outside a release): the project requires bide at it.
func Main(version string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bide init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: bide init [-agent [-single-approver]] [-module <path>] [-dev <bide checkout>] <dir>

Writes a new project into dir, which must not exist or must be empty: main.go, color.go, main_test.go,
flow.yaml, README.md, .gitignore and go.mod. Then: cd <dir> && go mod tidy && go run .

With -agent the project is an agent instead of a plan flow: a tool that reads untrusted text, a refund,
and a taint policy that requires every tool and run to state its provenance, so a side effect asked for after the agent has read the text waits for an escalation
gate of two of three signed decisions, not counting the requester. -single-approver makes that gate one
unsigned decision, for a team of one; it is the weaker choice, and the README says so. A plan flow is not
covered by the taint policy.

`)
		fs.PrintDefaults()
	}
	module := fs.String("module", "", "the module path (default: the directory's name)")
	agentProject := fs.Bool("agent", false, "write an agent with a taint policy (an escalation gate of two of three signed decisions) instead of a plan flow")
	single := fs.Bool("single-approver", false, "with -agent: an escalation gate of one unsigned decision, for a team of one (weaker)")
	dev := fs.String("dev", "", "a bide checkout to build against (replace directives), for a bide built from source")
	color := term.Flag(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	es := term.For(stderr, *color) // error lines on stderr
	if *single && !*agentProject {
		fmt.Fprintln(stderr, es.ErrorPrefix()+"bide init: -single-approver is for -agent")
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, es.ErrorPrefix()+"bide init: give exactly one directory")
		fs.Usage()
		return 2
	}
	dir := fs.Arg(0)
	name := *module
	if name == "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			fmt.Fprintln(stderr, es.ErrorPrefix()+"bide init:", err)
			return 1
		}
		name = filepath.Base(abs)
	}
	if err := checkName(name); err != nil {
		fmt.Fprintf(stderr, "%sbide init: %s\n", es.ErrorPrefix(), es.Hint(err.Error(), "; "))
		return 2
	}
	gomod, err := goMod(name, version, *dev)
	if err != nil {
		fmt.Fprintln(stderr, es.ErrorPrefix()+"bide init:", es.Hint(err.Error(), "; "))
		return 2
	}
	if err := write(dir, name, gomod, *agentProject, *single); err != nil {
		fmt.Fprintln(stderr, es.ErrorPrefix()+"bide init:", err)
		return 1
	}
	st := term.For(stdout, *color)
	fmt.Fprintf(stdout, "%swrote %s (module %s). Next:\n\n  %s\n  %s\n  %s\n\nthen see %s.\n",
		st.Pass(), st.Bold(dir), name, st.Path("cd "+dir), st.Path("go mod tidy"), st.Path("go run ."), st.Path("README.md"))
	return 0
}

// goMod is the project's go.mod: it requires bide at version, or, with dev, at the v0.0.0
// placeholder with replace directives to the checkout. A development build without dev has no
// version to name, so it is refused rather than writing one that does not exist.
func goMod(name, version, dev string) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "module %s\n\ngo 1.27.0\n\n", name)
	switch {
	case dev != "":
		abs, err := filepath.Abs(dev)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(filepath.Join(abs, "store", "sqlite", "go.mod")); err != nil {
			return "", fmt.Errorf("-dev %s is not a bide checkout (no store/sqlite/go.mod)", dev)
		}
		fmt.Fprintf(&b, "require (\n\tgithub.com/bide-ai/bide v0.0.0\n\tgithub.com/bide-ai/bide/store/sqlite v0.0.0\n)\n\n")
		fmt.Fprintf(&b, "replace github.com/bide-ai/bide => %s\n\nreplace github.com/bide-ai/bide/store/sqlite => %s\n",
			abs, filepath.Join(abs, "store", "sqlite"))
	case version != "" && version != "(devel)" && version != "devel":
		fmt.Fprintf(&b, "require (\n\tgithub.com/bide-ai/bide %s\n\tgithub.com/bide-ai/bide/store/sqlite %s\n)\n", version, version)
	default:
		return "", errors.New("this bide was built from source and has no release version to require; pass -dev <path to the bide checkout>, or install a release (go install github.com/bide-ai/bide/cmd/bide@latest)")
	}
	return b.String(), nil
}

// write writes the project into dir, which must not exist or must be empty.
func write(dir, name, gomod string, agentProject, single bool) error {
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	case len(entries) > 0:
		return fmt.Errorf("%s is not empty", dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	set := files
	if agentProject {
		set = agentFiles
	}
	for src, dst := range set {
		b, err := tmpl.ReadFile("template/" + src)
		if err != nil {
			return err
		}
		// The template is written with LF line endings on every OS: a Windows checkout of bide may
		// hold the embedded files with CRLF (core.autocrlf).
		gate, gateText := "false", strictGateText
		if single {
			gate, gateText = "true", singleGateText
		}
		// One pass over the template, so no replacement is scanned again: a name or a paragraph that holds
		// text spelled like another placeholder is written as it is.
		out := placeholders(name, gate, gateText).Replace(strings.ReplaceAll(string(b), "\r\n", "\n"))
		if err := os.WriteFile(filepath.Join(dir, dst), []byte(out), 0o644); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644)
}
