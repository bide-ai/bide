// Command docsnip checks that every Go code block in bide's documentation compiles against the
// current code, so the docs cannot drift silently when an API changes.
//
// It reads every ```go block of the markdown files it is given (README.md and everything under
// docs/ by default) and type-checks each one with go/types against the packages of the Go
// workspace at -root, which resolves bide's own modules through go.work the way CI builds them.
// Export data comes from one "go list -export" run, so a warm build cache makes the check take
// seconds.
//
// A block is compiled as one of:
//
//   - a complete file, when it starts with a package clause; it must compile as written;
//   - top-level declarations, compiled as a package of their own;
//   - statements, compiled as the body of a function;
//   - declarations (imports, types, funcs) followed by statements.
//
// In the last three, a package the block uses as a qualifier without importing it is imported
// automatically when its name is unambiguous: every package of the standard library and of the
// workspace modules is known by name (a workspace package wins over a standard-library one,
// and a shorter standard-library path over a longer one). Unused variables and imports, and
// unused expression values (a block listing values), are allowed, since such blocks are
// excerpts. The elisions "{ ... }" after a space (a function body, compiled as one that panics),
// "T{...}" (a composite literal, compiled as T{}) and a line holding only "..." compile. A
// function declared without a body is an error outside an api block.
//
// An HTML comment directly above a block (blank lines may separate them) annotates it; it does
// not show when the markdown is rendered:
//
//	<!-- docsnip: setup a *agent.Agent; store agent.Durable -->
//	<!-- docsnip: api agent -->
//	<!-- docsnip: skip illustrative pseudo-code -->
//
// A setup gives the block the identifiers it uses but does not declare. Its items are separated
// by semicolons or newlines outside brackets: `import "path"` (or import name "path"),
// "returns T" (the results of the function that wraps a statement block, for a block that
// returns), a declaration starting with type, func, var or const, or the shorthand
// "name[, name] T" for a var. Errors in a setup are reported at the directive.
//
// An api block lists declarations of one package (a name or an import path, optionally followed
// by "; setup items"), which is dot-imported; each declaration must match the package's own (see
// api.go). A skip excludes a block that is not meant to compile, with the reason; a skip on a
// block that compiles is reported, so the annotation is deleted once the block is real code.
// Blocks with identical annotation and code (the README and its translations) are compiled
// once, and a finding lists every copy.
//
// Usage, from the repository root:
//
//	go run ./internal/tools/docsnip [-root dir] [-v] [path ...]
//
// Each path is a markdown file or a directory searched for *.md files, relative to -root. The
// exit status is 0 when every block compiles, 1 on findings, and 2 when the check could not run.
package main

import (
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("docsnip", flag.ContinueOnError)
	fl.SetOutput(stderr)
	root := fl.String("root", ".", "repository root: the go command resolves imports here, and paths are relative to it")
	verbose := fl.Bool("v", false, "list every block and how it was compiled")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	paths := fl.Args()
	if len(paths) == 0 {
		paths = []string{"README.md", "docs"}
	}
	files, err := markdownFiles(*root, paths)
	if err != nil {
		fmt.Fprintf(stderr, "docsnip: %v\n", err)
		return 2
	}
	var blocks []Block
	var findings []Finding
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(*root, f))
		if err != nil {
			fmt.Fprintf(stderr, "docsnip: %v\n", err)
			return 2
		}
		bs, err := Extract(f, data)
		if err != nil {
			findings = append(findings, Finding{Msg: err.Error()})
			continue
		}
		blocks = append(blocks, bs...)
	}
	c, err := NewChecker(*root)
	if err != nil {
		fmt.Fprintf(stderr, "docsnip: %v\n", err)
		return 2
	}
	r, err := c.Check(blocks)
	if err != nil {
		fmt.Fprintf(stderr, "docsnip: %v\n", err)
		return 2
	}
	if *verbose {
		for _, b := range blocks {
			loc := fmt.Sprintf("%s:%d", b.File, b.Line)
			fmt.Fprintf(stdout, "%s: %s\n", loc, r.Status[loc])
		}
	}
	for _, f := range findings {
		fmt.Fprintln(stdout, f.Msg)
	}
	for _, f := range r.Findings {
		fmt.Fprintln(stdout, f)
	}
	var kinds []string
	for _, k := range []Kind{KindProgram, KindFile, KindDecls, KindStmts, KindMixed, KindAPI} {
		if n := r.Kinds[k]; n > 0 {
			kinds = append(kinds, fmt.Sprintf("%s %d", k, n))
		}
	}
	fmt.Fprintf(stderr, "docsnip: %d go blocks in %d files, %d distinct: %d compiled (%s), %d skipped\n",
		r.Blocks, len(files), r.Unique, r.Checked, strings.Join(kinds, ", "), r.Skipped)
	if n := len(findings) + len(r.Findings); n > 0 {
		fmt.Fprintf(stderr, "docsnip: %d findings. Fix the block; give it the identifiers it uses with <!-- docsnip: setup ... -->; or, for pseudo-code, <!-- docsnip: skip reason -->\n", n)
		return 1
	}
	return 0
}

// markdownFiles expands paths (files, or directories searched for *.md) into sorted,
// slash-separated paths relative to root.
func markdownFiles(root string, paths []string) ([]string, error) {
	seen := map[string]bool{}
	for _, p := range paths {
		full := filepath.Join(root, p)
		info, err := os.Stat(full)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			seen[filepath.ToSlash(filepath.Clean(p))] = true
			continue
		}
		err = filepath.WalkDir(full, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			if !d.IsDir() && strings.HasSuffix(d.Name(), ".md") {
				rel, err := filepath.Rel(root, path)
				if err != nil {
					return err
				}
				seen[filepath.ToSlash(rel)] = true
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	files := make([]string, 0, len(seen))
	for f := range seen {
		files = append(files, f)
	}
	sort.Strings(files)
	return files, nil
}
