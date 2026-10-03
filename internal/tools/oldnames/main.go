// Command oldnames fails when a name the pre-1.0 API redesign removed (docs/design/api-v1.md,
// section 10.2) appears in bide's Go code, its godoc, or its documentation: the transitional
// names (RunMessage, Build, ResolveHaltRef, ...), the string entry points they replaced (RunSaga,
// RunResult, ...), the pause aliases, the Durable interface and its shims, the old Tool method
// set's SpecOf, the context decorators, the mcp package's old path, and agent.ScriptedModel.
//
// It scans every .go and .md file below the root, except the migrate tool (which rewrites
// these names and so must name them), this command, CHANGELOG.md, the release notes
// (docs/releases) and the dated design records (docs/design), which describe the API as it was.
// With -godoc, it also runs `go doc -all` on every package of the root module and checks its
// output, the API as a user reads it, for the same names and for the word "transitional".
//
// Usage, from the repository root:
//
//	go run ./internal/tools/oldnames [-root dir] [-godoc]
//
// The exit status is 0 when clean, 1 on findings, and 2 when the check could not run.
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
	fl := flag.NewFlagSet("oldnames", flag.ContinueOnError)
	fl.SetOutput(stderr)
	root := fl.String("root", ".", "repository root; every .go and .md file below it is checked")
	godoc := fl.Bool("godoc", false, "also check `go doc -all` of every package of the root module")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if fl.NArg() != 0 {
		fmt.Fprintf(stderr, "oldnames: unexpected arguments %q\n", fl.Args())
		return 2
	}
	findings, err := CheckTree(*root)
	if err != nil {
		fmt.Fprintln(stderr, "oldnames:", err)
		return 2
	}
	if *godoc {
		more, err := CheckGodoc(*root)
		if err != nil {
			fmt.Fprintln(stderr, "oldnames:", err)
			return 2
		}
		findings = append(findings, more...)
	}
	for _, f := range findings {
		fmt.Fprintln(stdout, f)
	}
	if len(findings) > 0 {
		fmt.Fprintf(stdout, "oldnames: %d findings. Use the current name (docs/design/api-v1.md, section 10.2; CHANGELOG.md)\n", len(findings))
		return 1
	}
	return 0
}
