// Command changelog manages bide's changelog fragments (changes/unreleased, see CONTRIBUTING.md).
//
// A pull request adds one fragment file instead of editing CHANGELOG.md, so pull requests do not
// conflict on it. A fragment is a "Section: <name>" line (Added, Changed, Fixed, Removed or
// Security), a blank line, and the entry text as one paragraph, with anything shaped like an HTML
// tag ("<time>", "List<T>") in backticks, since the docs site reads a bare one as HTML.
//
// Usage, from the repository root:
//
//	go run ./internal/tools/changelog lint [-root dir]
//	go run ./internal/tools/changelog assemble -version 0.16.0 -date 2026-10-20 [-root dir] [-n]
//	go run ./internal/tools/changelog check -base SHA -head SHA [-labels a,b]
//
// lint parses every fragment. assemble folds the fragments into CHANGELOG.md as the section for
// the version, updates its compare links, and deletes the fragments; with -n it prints the new
// section and changes nothing. check is the pull request gate: a change to code a published
// module ships needs an added fragment, unless the pull request has the no-changelog label.
//
// The exit status is 0 when clean, 1 on findings, and 2 when the command could not run.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: changelog lint|assemble|check [flags]")
		return 2
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "repository root")
	version := fs.String("version", "", "assemble: the release version, like 0.16.0")
	date := fs.String("date", "", "assemble: the release date, like 2026-10-20")
	dry := fs.Bool("n", false, "assemble: print the new section, change nothing")
	base := fs.String("base", "", "check: the pull request's base commit")
	head := fs.String("head", "", "check: the pull request's head commit")
	labels := fs.String("labels", "", "check: the pull request's labels, comma-separated")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	frags, err := ReadFragments(*root)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	switch args[0] {
	case "lint":
		fmt.Fprintf(stdout, "changelog: %d fragments, all valid\n", len(frags))
		return 0
	case "assemble":
		path := filepath.Join(*root, "CHANGELOG.md")
		b, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		out, err := Assemble(string(b), frags, *version, *date)
		if err != nil {
			fmt.Fprintln(stderr, "changelog:", err)
			return 1
		}
		if *dry {
			fmt.Fprint(stdout, Section(frags))
			return 0
		}
		if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		for _, f := range frags {
			if err := os.Remove(filepath.Join(*root, FragmentDir, f.Name)); err != nil {
				fmt.Fprintln(stderr, err)
				return 2
			}
		}
		fmt.Fprintf(stdout, "changelog: %d fragments assembled into CHANGELOG.md as [%s]\n", len(frags), strings.TrimPrefix(*version, "v"))
		return 0
	case "check":
		if *base == "" || *head == "" {
			fmt.Fprintln(stderr, "changelog check: -base and -head are required")
			return 2
		}
		all, err := gitDiff(*root, *base, *head, "")
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		added, err := gitDiff(*root, *base, *head, "AM")
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		switch {
		case !NeedsFragment(all):
			fmt.Fprintln(stdout, "changelog: no published code changed; no fragment needed")
		case HasFragment(added):
			fmt.Fprintln(stdout, "changelog: the change adds a fragment")
		case hasLabel(*labels, "no-changelog"):
			fmt.Fprintln(stdout, "changelog: the pull request has the no-changelog label")
		default:
			fmt.Fprintf(stderr, "changelog: this change touches code a published module ships and adds no fragment.\n"+
				"Add %s/<slug>.md (see CONTRIBUTING.md, \"Changelog\"), or give the pull request the no-changelog label.\n", FragmentDir)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stderr, "changelog: unknown command %q\n", args[0])
	return 2
}

func hasLabel(labels, want string) bool {
	for _, l := range strings.Split(labels, ",") {
		if strings.TrimSpace(l) == want {
			return true
		}
	}
	return false
}

// gitDiff lists the files changed from the merge base of base and head to head; filter is a git
// --diff-filter value, or empty for every change. Renames are listed as a delete and an add.
func gitDiff(root, base, head, filter string) ([]string, error) {
	args := []string{"-C", root, "diff", "--name-only", "--no-renames"}
	if filter != "" {
		args = append(args, "--diff-filter="+filter)
	}
	out, err := exec.Command("git", append(args, base+"..."+head)...).Output()
	if err != nil {
		return nil, fmt.Errorf("git diff %s...%s: %w", base, head, err)
	}
	return strings.Split(strings.TrimRight(string(out), "\n"), "\n"), nil
}
