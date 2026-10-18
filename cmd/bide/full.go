//go:build !bide_audit_only

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/bide-ai/bide/cmd/bide/internal/approvecmd"
	"github.com/bide-ai/bide/cmd/bide/internal/demo"
	"github.com/bide-ai/bide/cmd/bide/internal/doctor"
	"github.com/bide-ai/bide/cmd/bide/internal/editcmd"
	"github.com/bide-ai/bide/cmd/bide/internal/identitycmd"
	"github.com/bide-ai/bide/cmd/bide/internal/initcmd"
	"github.com/bide-ai/bide/cmd/bide/internal/journalcmd"
	"github.com/bide-ai/bide/cmd/bide/internal/migratecmd"
	"github.com/bide-ai/bide/cmd/bide/internal/notifycmd"
	"github.com/bide-ai/bide/cmd/bide/internal/samplecmd"
	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/internal/term"
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
			name:    "doctor",
			summary: "check that a first run can work: Go, SQLite, the module cache, model keys, your project",
			run: func(args []string, stdout, stderr io.Writer) int {
				return doctor.Main(releaseVersion(), args, stdout, stderr)
			},
			exits: commonExits,
		},
		{
			name:    "sample",
			summary: "write a sample journal of varied runs, with evidence and flow configs, and open bide view on it",
			run:     samplecmd.Main,
			exits:   commonExits,
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
			name:    "edit",
			summary: "a local page for a flow config: the graph, the approval gates, the assertions, and what an edit weakens; edit the source and save",
			run:     editcmd.Main,
			exits:   commonExits,
		},
		{
			name:    "notify",
			summary: "set up and test the webhook notifier: test a receiver, run a local one, relay to Slack and PagerDuty",
			run:     notifycmd.Main,
			exits:   commonExits,
			verbs:   notifycmd.Verbs(),
		},
		{
			name:    "journal",
			summary: "erase personal data from finished runs of a journal, accountably: a dry-run plan, then a signed record of each erasure",
			run:     journalcmd.Main,
			exits:   commonExits,
			verbs:   journalcmd.Verbs(),
		},
		{
			name:    "approve",
			summary: "record your signed approval (or -reject) of a call that waits for a decision; -list shows what waits",
			run: func(args []string, stdout, stderr io.Writer) int {
				return approvecmd.Main(args, os.Stdin, stdout, stderr, approveStores)
			},
			exits: commonExits,
		},
		{
			name:    "identity",
			summary: "pre-flight a certificate-bound approval setup and say why a certificate would be refused (check), keep the CRLs of a policy's issuers current and archived (fetch-crl), and reset a poisoned rollback mark",
			run:     identitycmd.Main,
			exits:   identityExits,
			verbs:   identitycmd.Verbs(),
		},
		{
			name:    "keygen",
			summary: "create an approver key pair for bide approve and print the public key policies and verifiers use",
			run:     approvecmd.KeygenMain,
			exits:   commonExits,
		},
		{
			name:    "migrate",
			summary: "create or check a Postgres journal's schema as its owner, so the service needs no DDL rights",
			run:     migratecmd.Main,
			exits:   commonExits,
		},
		{
			name:    "check",
			summary: "check plan flow configs, for CI: one file:node: message line per problem",
			run:     runCheck,
			exits:   commonExits,
		},
	}
}

// identityExits are the exit statuses of bide identity: the common ones, and check's two more.
const identityExits = commonExits + `
   for identity check: 1 means the setup will not work as configured (a check failed)
3  identity check: a file could not be read or the tool could not run
4  identity check: nothing failed, but part was left unchecked (-no-status, -no-expect): not ready`

// runCheck is bide check: bide view's -config -check on each file. It exits 1 if any file has an
// error (with -strict, any problem) or cannot be read. With -diff it compares one file with an
// earlier version (bide view -config <file> -diff <old>): a page, or with -check the changes as
// text.
func runCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bide check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: bide check [-strict] <flow.yaml|flow.json>...
       bide check <flow.yaml|flow.json> -diff <old config|git revision> [-check [-fail-on-weaken]]

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

With -diff it compares the config with an earlier version of it instead, and serves a read-only
local page (like bide view) that draws the new flow with added nodes and edges in green, removed
ones as dashed red ghosts and changed nodes in amber, and calls out a weakened approval policy (a
gate removed, its need lowered, an approver added without raising the need, exclude_principal
dropped, or its timeout removed or lengthened), a node that every path reached through a gate before and some path now reaches without one (whatever its safety), a new effect no gate guards, a lowered safety, a Switch condition changed
beside an ungated effect, a block changed on an ungated effect node (without the Registry), or an assert
line removed, loosened, or kept and no longer holding (violated, undecided, or holding only vacuously). -diff takes a file; when no file has that name, a git revision read with git show: <rev>:<path>
(git's own form), or a bare <rev> for the config's own path at that revision (bide check
flows/refund.yaml -diff main). With -check it serves nothing, prints one line per change and a
summary line, and exits 0: a diff is not a failure, unless -fail-on-weaken finds an approval gate
removed or weakened, or another weakening named above. Flags may follow the file names.

On a terminal the output is colored (a red cross on each error and its counterexample path in red,
a green check on assertions that all hold); piped or redirected it is plain text. -color=never, or
NO_COLOR set to any value, turns color off; -color=always keeps it on.

`)
		fs.PrintDefaults()
	}
	strict := fs.Bool("strict", false, "exit 1 on a warning too")
	diff := fs.String("diff", "", "compare the one config given with this earlier version: a file, or else a git revision (<rev>:<path>, or <rev> for the config's path at rev)")
	check := fs.Bool("check", false, "with -diff: serve nothing and print each change, then a summary line")
	failOnWeaken := fs.Bool("fail-on-weaken", false, "with -diff -check: exit 1 if an approval gate was removed or weakened (its need lowered, an approver added without raising the need, exclude_principal dropped, its timeout removed or lengthened, a node newly reachable without a gate or a lowered safety, a Switch condition changed beside an ungated effect, or an assert line removed, loosened, or kept and no longer holding)")
	addr := fs.String("addr", "127.0.0.1:8484", "with -diff: address to serve the page on, a loopback IP (127.0.0.1 or ::1) or localhost, with a port")
	color := term.Flag(fs)
	files, err := parseInterspersed(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	usage := func(msg string) int {
		fmt.Fprintln(stderr, term.For(stderr, *color).ErrorPrefix()+"bide check:", msg)
		fs.Usage()
		return 2
	}
	if len(files) == 0 {
		return usage("no flow config given")
	}
	if *diff == "" && (*check || *failOnWeaken) {
		return usage("-check and -fail-on-weaken need -diff")
	}
	if *diff != "" {
		if len(files) != 1 {
			return usage("-diff compares one flow config")
		}
		a := []string{"-config", files[0], "-diff", *diff, "-addr", *addr, "-color", color.String()}
		if *check {
			a = append(a, "-check")
		}
		if *failOnWeaken {
			a = append(a, "-fail-on-weaken")
		}
		if *strict {
			a = append(a, "-strict")
		}
		return runview.Main(a, stdout, stderr, stores)
	}
	code := 0
	for _, file := range files {
		a := []string{"-config", file, "-check", "-color", color.String()}
		if *strict {
			a = append(a, "-strict")
		}
		code = max(code, runview.Main(a, stdout, stderr, stores))
	}
	return code
}

// parseInterspersed parses fs's flags wherever they stand among the arguments (bide check
// flow.yaml -diff old.yaml), and returns the other arguments in order. Everything after "--" is an
// argument.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var rest []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		left := fs.Args()
		if len(left) == 0 {
			return rest, nil
		}
		if n := len(args) - len(left); n > 0 && args[n-1] == "--" {
			return append(rest, left...), nil
		}
		rest, args = append(rest, left[0]), left[1:]
	}
}
