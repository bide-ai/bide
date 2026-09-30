// Command doccheck enforces bide's documentation policy over every module in the repository:
// every exported type, func, method, package-level const and var has a doc comment that starts
// with its name, and every package has a package comment ("Package name ..." outside package
// main). Exported identifiers in package main, test files and generated files are not checked.
//
// Findings listed in the allowlist (.doccheck-allow by default) are tolerated. The list may only
// shrink: an entry whose identifier is now documented, or no longer exists, fails the check
// until the entry is deleted. New code never gets an entry; document it instead.
//
// Usage, from the repository root:
//
//	go run ./internal/tools/doccheck [-root dir] [-allow file]
//
// The exit status is 0 when clean, 1 on findings or stale entries, and 2 when the check could
// not run.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("doccheck", flag.ContinueOnError)
	fl.SetOutput(stderr)
	root := fl.String("root", ".", "repository root; every Go package below it is checked")
	allow := fl.String("allow", ".doccheck-allow", "allowlist file; empty for none")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if fl.NArg() != 0 {
		fmt.Fprintf(stderr, "doccheck: unexpected arguments %q\n", fl.Args())
		return 2
	}
	list := &Allowlist{}
	if *allow != "" {
		data, err := os.ReadFile(*allow)
		if err != nil {
			fmt.Fprintf(stderr, "doccheck: %v\n", err)
			return 2
		}
		if list, err = ParseAllowlist(data); err != nil {
			fmt.Fprintf(stderr, "doccheck: %s: %v\n", *allow, err)
			return 2
		}
	}
	findings, err := Check(*root)
	if err != nil {
		fmt.Fprintf(stderr, "doccheck: %v\n", err)
		return 2
	}
	unlisted, stale := list.Apply(findings)
	for _, f := range unlisted {
		fmt.Fprintln(stdout, f)
	}
	for _, k := range stale {
		fmt.Fprintf(stdout, "%s: stale allowlist entry %q: the identifier is documented or gone; delete the line\n", *allow, k)
	}
	if len(unlisted) > 0 || len(stale) > 0 {
		fmt.Fprintf(stderr, "doccheck: %d undocumented, %d stale allowlist entries\n", len(unlisted), len(stale))
		return 1
	}
	return 0
}
