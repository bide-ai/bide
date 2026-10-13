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
	"flow.yaml":         "flow.yaml",
	"README.md.tmpl":    "README.md",
	"gitignore.tmpl":    ".gitignore",
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
		fmt.Fprint(fs.Output(), `usage: bide init [-module <path>] [-dev <bide checkout>] <dir>

Writes a new project into dir, which must not exist or must be empty: main.go, main_test.go,
flow.yaml, README.md, .gitignore and go.mod. Then: cd <dir> && go mod tidy && go run .

`)
		fs.PrintDefaults()
	}
	module := fs.String("module", "", "the module path (default: the directory's name)")
	dev := fs.String("dev", "", "a bide checkout to build against (replace directives), for a bide built from source")
	color := term.Flag(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	es := term.For(stderr, *color) // error lines on stderr
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
	if !modName.MatchString(name) {
		fmt.Fprintf(stderr, "%sbide init: %s\n", es.ErrorPrefix(), es.Hint(fmt.Sprintf("%q is not a module path; name one with -module", name), "; "))
		return 2
	}
	gomod, err := goMod(name, version, *dev)
	if err != nil {
		fmt.Fprintln(stderr, es.ErrorPrefix()+"bide init:", es.Hint(err.Error(), "; "))
		return 2
	}
	if err := write(dir, name, gomod); err != nil {
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
func write(dir, name, gomod string) error {
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
	for src, dst := range files {
		b, err := tmpl.ReadFile("template/" + src)
		if err != nil {
			return err
		}
		// The template is written with LF line endings on every OS: a Windows checkout of bide may
		// hold the embedded files with CRLF (core.autocrlf).
		out := strings.ReplaceAll(strings.ReplaceAll(string(b), "\r\n", "\n"), "__NAME__", name)
		if err := os.WriteFile(filepath.Join(dir, dst), []byte(out), 0o644); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644)
}
