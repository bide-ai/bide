// Command migrate rewrites Go code written against bide v0.10's API to the pre-1.0 API
// (docs/design/api-v1.md, section 10.2): it renames the transitional names, rewrites the call
// sites of the string entry points, the builder methods, the Durable interface and the old tool
// constructors, and moves the names that changed package. It is how bide itself was migrated,
// and how an application moves to the new API.
//
// It works from type information: it loads the packages of one module (with their tests)
// through the go command, type-checks them, and rewrites each call by what it calls, so a
// method named Run on a type of your own is never touched. Run it on code that compiles against
// the old API; it also handles code that is partly migrated (a rebase that brought old call sites
// in), where the old names no longer resolve: an unresolved call is matched by its receiver's
// type. A site it cannot rewrite safely is reported, with its position, for a person to finish.
//
// Usage, from the root of the module to migrate:
//
//	go run github.com/bide-ai/bide/internal/tools/migrate@<version> [-n] [-rules r1,r2] [packages]
//
// Packages default to ./... . After rewriting, it type-checks the module against the new API
// (-bide: a version, or the directory of a bide checkout; by default the version it was built
// from), and lists every error as a site that needs a person. With -md, the arguments are
// markdown files or directories (README.md and docs by default), whose Go code blocks are
// rewritten instead (see MigrateMarkdown). -n reports what would change without writing. -rules
// selects rewrite classes (default: all; -list prints them). Rewritten files are gofmt'ed. The
// exit status is 0 when every site was rewritten and the result type-checks, 1 when some sites
// need a person (they are listed, with a count), and 2 when the tool could not run.
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
	fl := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fl.SetOutput(stderr)
	dir := fl.String("C", ".", "the module's root directory")
	dry := fl.Bool("n", false, "report what would change, write nothing")
	only := fl.String("rules", "", "comma-separated rewrite classes to run (default all)")
	list := fl.Bool("list", false, "list the rewrite classes and exit")
	bide := fl.String("bide", "", "the new bide API to type-check the rewritten code against: a version, or the directory of a bide checkout (default: the version this command was built from)")
	md := fl.Bool("md", false, "rewrite the Go code blocks of markdown files (the arguments: files or directories, relative to -C; default README.md and docs) instead of packages")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	all := Rules()
	if *list {
		for _, r := range all {
			fmt.Fprintf(stdout, "%-12s %s\n", r.Name, r.Doc)
		}
		return 0
	}
	rules, err := selectRules(all, *only)
	if err != nil {
		fmt.Fprintln(stderr, "migrate:", err)
		return 2
	}
	var out map[string][]byte
	var res *Run
	if *md {
		files, err := markdownFiles(*dir, fl.Args())
		if err == nil {
			out, res, err = MigrateMarkdown(*dir, files, rules)
		}
		if err != nil {
			fmt.Fprintln(stderr, "migrate:", err)
			return 2
		}
	} else {
		patterns := fl.Args()
		if len(patterns) == 0 {
			patterns = []string{"./..."}
		}
		out, res, err = MigrateModule(*dir, patterns, rules)
		if err == nil {
			var fs []Finding
			fs, err = CheckModule(*dir, patterns, out, *bide)
			res.Findings = append(res.Findings, fs...)
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, "migrate:", err)
		return 2
	}
	paths := make([]string, 0, len(out))
	for p := range out {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if !*dry {
			if err := os.WriteFile(p, out[p], 0o644); err != nil {
				fmt.Fprintln(stderr, "migrate:", err)
				return 2
			}
		}
		fmt.Fprintln(stdout, "rewrote", p)
	}
	names := make([]string, 0, len(res.Counts))
	for n := range res.Counts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(stdout, "%s: %d sites\n", n, res.Counts[n])
	}
	for _, f := range res.Findings {
		fmt.Fprintln(stdout, f)
	}
	if len(res.Findings) > 0 {
		unchecked := 0
		for _, f := range res.Findings {
			if f.Rule == "check" {
				unchecked++
			}
		}
		fmt.Fprintf(stdout, "%d sites need a person (%d of them: the code does not type-check against the new API after the rewrite, or was not checked)\n", len(res.Findings), unchecked)
		return 1
	}
	return 0
}

func selectRules(all []Rule, only string) ([]Rule, error) {
	if only == "" {
		return all, nil
	}
	want := map[string]bool{}
	for _, n := range strings.Split(only, ",") {
		want[strings.TrimSpace(n)] = true
	}
	var rules []Rule
	for _, r := range all {
		if want[r.Name] {
			rules = append(rules, r)
			delete(want, r.Name)
		}
	}
	for n := range want {
		return nil, fmt.Errorf("unknown rule %q (see -list)", n)
	}
	return rules, nil
}

// markdownFiles lists the markdown files of paths (files or directories, relative to root;
// README.md and docs by default), as absolute paths.
func markdownFiles(root string, paths []string) ([]string, error) {
	if len(paths) == 0 {
		paths = []string{"README.md", "docs"}
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, p := range paths {
		p = filepath.Join(abs, p)
		err := filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			if !d.IsDir() && strings.HasSuffix(path, ".md") {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(files)
	return files, nil
}
