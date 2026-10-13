//go:build !bide_audit_only

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/bide-ai/bide/cmd/bide/internal/demo"
	"github.com/bide-ai/bide/cmd/bide/internal/initcmd"
	"github.com/bide-ai/bide/internal/runview"
)

// fullCommands are the commands that need a store driver or the YAML decoder. A bide_audit_only
// build leaves them out (auditonly.go).
func fullCommands() []command {
	return []command{
		{
			name:    "init",
			summary: "scaffold a project: a gated plan flow on SQLite, with evidence for bide audit",
			run: func(args []string, stdout, stderr io.Writer) int {
				return initcmd.Main(releaseVersion(), args, stdout, stderr)
			},
			exits: commonExits,
		},
		{
			name:    "demo",
			summary: "kill an agent mid-payment and watch it not charge twice (no API key)",
			run: func(args []string, stdout, stderr io.Writer) int {
				return demo.Main(args, []string{"demo"}, stdout, stderr)
			},
			exits: commonExits,
		},
		{
			name:    "view",
			summary: "a read-only local page for one run of a journal, or a flow config's graph",
			run: func(args []string, stdout, stderr io.Writer) int {
				return runview.Main(args, stdout, stderr, stores)
			},
			exits: commonExits,
		},
		{
			name:    "check",
			summary: "check plan flow configs, for CI: one file:node: message line per problem",
			run:     runCheck,
			exits:   commonExits,
		},
	}
}

// runCheck is bide check: bide view's -config -check on each file. It exits 1 if any file has an
// error (with -strict, any problem) or cannot be read.
func runCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bide check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: bide check [-strict] <flow.yaml|flow.json>...

Checks each plan flow config without running it, and prints one "file:node: message" line per
problem ("file:from->to: message" for an edge or arm). The check is structural: types and
registered blocks are checked when a program serves the page with its plan.Registry (package
plan/planview). It is the check bide view -config shows on the graph.

A config's assert lines are checked too, by reachability on the declared graph:

  every path to <node> passes approval >= K [of N]
  every path to <node> passes <node>
  every path to a terminal passes <node>
  no side effect after <node>
  <node> reachable
  <node> unreachable

A violated assertion is an error, printed with a shortest counterexample path ("counterexample:
a -> b -> c"); so is an assertion that is not in this grammar, names an undeclared node, or cannot
be decided. A file with assertions ends with a line counting their verdicts.

`)
		fs.PrintDefaults()
	}
	strict := fs.Bool("strict", false, "exit 1 on a warning too")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, "bide check: no flow config given")
		fs.Usage()
		return 2
	}
	code := 0
	for _, file := range fs.Args() {
		a := []string{"-config", file, "-check"}
		if *strict {
			a = append(a, "-strict")
		}
		code = max(code, runview.Main(a, stdout, stderr, stores))
	}
	return code
}
