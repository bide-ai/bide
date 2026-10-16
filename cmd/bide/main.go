// Command bide is the command line of bide, a Go framework for AI agents that move money: one
// program for everyone who does not import the library.
//
//	bide init refund               scaffold a project: a gated plan flow, ready for go run .
//	bide demo                      kill an agent mid-payment and watch it not charge twice
//	bide doctor                    check that a first run can work, with a fix for each failure
//	bide sample                    write a sample journal (14 runs, evidence, flow configs) and open bide view on it
//	bide view -db run.db           a read-only local page for one run of a journal
//	bide edit flow.yaml            a local page for a flow config: edit its source, see the check and what an edit weakens, save
//	bide approve -db run.db -list  list the calls waiting for a decision; sign one with -run -call -as -key
//	bide keygen -out ops.key       create an approver key pair for bide approve
//	bide migrate -postgres-env VAR create or check a Postgres journal's schema as its owner
//	bide check flow.yaml           check a plan flow config, for CI
//	bide notify test|listen|relay  set up and test the webhook notifier, and relay it to Slack and PagerDuty
//	bide audit verify-evidence ... produce and verify audit proofs offline
//	bide version                   the version and the artifact formats bide audit reads
//	bide help [command]            the commands, or one command's flags
//
// Each command parses its own flags with the standard library flag package (single-dash flags,
// such as -db and -evidence). Exit status: 0 success, 1 failure, 2 usage error, for every command;
// bide audit keeps its own statuses, where only 0 means verified (see bide help audit).
//
// Built with -tags bide_audit_only, the program holds only audit, version and help, and its import
// graph is the standard library and the core module: no store driver, no YAML library. That is the
// build for a verifier who wants nothing else in the binary (see docs/guides/security-model.md).
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"slices"
	"strings"

	"github.com/bide-ai/bide/internal/auditcli"
	"github.com/bide-ai/bide/internal/term"
)

// version is the release version, set by a release build with -ldflags "-X main.version=v1.2.3".
var version = ""

// releaseVersion is the release this program was built from: the stamped version, or the module
// version the go command records (go install ...@v0.14.0); "" for a build from source.
func releaseVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return ""
}

// command is one bide subcommand.
type command struct {
	name    string
	summary string
	// run runs the command with its arguments (after the command's name) and returns the exit
	// status.
	run func(args []string, stdout, stderr io.Writer) int
	// exits documents the command's exit statuses, for help and the CLI reference.
	exits string
	// verbs, if not empty, are the verbs of a command that has them (audit, notify): help and the
	// CLI reference print each one's flags too.
	verbs []string
}

// commonExits are the exit statuses of every command but audit.
const commonExits = `0  success
1  failure: an input cannot be read or the command failed; for check, an error was found
   (with -strict, any problem)
2  usage error: a bad flag or argument; nothing was read`

// auditExits are bide audit's exit statuses (internal/auditcli).
const auditExits = `0  verified (for prove and prove-absent: the artifact was written)
1  read and understood, and not verified
2  usage error: a bad flag or argument; nothing was read
3  no verdict: the -checker gave none, or an internal error
4  an input is unreadable or unusable`

// coreCommands are in every build, bide_audit_only included.
var coreCommands = []command{
	{
		name:    "audit",
		summary: "produce and verify audit proofs offline",
		run: func(args []string, stdout, stderr io.Writer) int {
			return auditcli.Main(version, args, stdout, stderr)
		},
		exits: auditExits,
		verbs: auditcli.Verbs(),
	},
	{
		name:    "version",
		summary: "print the version and the artifact formats bide audit reads (-json for JSON)",
		run:     runVersion,
		exits:   commonExits,
	},
}

// commands lists every command of this build, in the order help shows them.
var commands = append(fullCommands(), coreCommands...)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run runs one command line (without the program name) and returns its exit status.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stdout)
		return 0
	}
	name, rest := args[0], args[1:]
	switch name {
	case "help", "-h", "-help", "--help":
		return help(rest, stdout, stderr)
	}
	c, ok := find(name)
	if !ok {
		fmt.Fprintf(stderr, "%sbide: unknown command %q\n\n", term.For(stderr, term.Auto).ErrorPrefix(), name)
		usage(stderr)
		return 2
	}
	if len(rest) == 1 && isHelp(rest[0]) {
		return help([]string{name}, stdout, stderr)
	}
	return c.run(rest, stdout, stderr)
}

func isHelp(arg string) bool { return arg == "-h" || arg == "-help" || arg == "--help" }

func find(name string) (command, bool) {
	for _, c := range commands {
		if c.name == name {
			return c, true
		}
	}
	return command{}, false
}

// usage prints the command list: on a terminal the headings and command names are bold and the
// summaries dim. A command's own -h text (flag package output) stays plain.
func usage(w io.Writer) {
	st := term.For(w, term.Auto)
	fmt.Fprintf(w, "bide: build AI agents that move money, and account for every action. %s\n\n  bide <command> [flags]\n\n%s\n", st.Heading("Usage:"), st.Heading("Commands:"))
	for _, c := range commands {
		fmt.Fprintf(w, "  %s%s %s\n", st.Bold(c.name), strings.Repeat(" ", max(0, 8-len(c.name))), st.Dim(c.summary))
	}
	fmt.Fprintf(w, "\nRun %s for a command's flags and exit statuses.\n", st.Bold("bide help <command>"))
}

// help prints help for the command (and, for audit, the verb) args name, or the command list, and
// exits 0; an unknown command is a usage error.
func help(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stdout)
		return 0
	}
	c, ok := find(args[0])
	if !ok || len(args) > 2 || (len(args) == 2 && !slices.Contains(c.verbs, args[1])) {
		fmt.Fprintf(stderr, "%sbide help: unknown command %q\n\n", term.For(stderr, term.Auto).ErrorPrefix(), strings.Join(args, " "))
		usage(stderr)
		return 2
	}
	fmt.Fprint(stdout, commandHelp(c, args[1:], term.For(stdout, term.Auto)))
	return 0
}

// commandHelp is the help text of c (with verb, of one audit verb): the command's own -h output
// and its exit statuses. Each command prints its usage and flags (FlagSet.PrintDefaults) for -h,
// so this is generated from the flag sets the command parses. Only the "Exit status:" heading is
// styled (by st); the flag package's text stays plain.
func commandHelp(c command, verb []string, st term.Style) string {
	var out strings.Builder
	_ = c.run(append(verb, "-h"), &out, &out) // -h exits 0 or 2 by the command's own rules; help is 0
	if len(verb) == 0 {
		fmt.Fprintf(&out, "\n%s\n%s\n", st.Heading("Exit status:"), indent(c.exits))
	}
	return out.String()
}

func indent(s string) string { return "  " + strings.ReplaceAll(s, "\n", "\n  ") }

func runVersion(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bide version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: bide version [-json] [-color auto|always|never]")
		fs.PrintDefaults()
	}
	asJSON := fs.Bool("json", false, "print the version and the artifact formats as JSON")
	color := term.Flag(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "%sbide version: unexpected argument %q\n", term.For(stderr, term.Auto).ErrorPrefix(), fs.Arg(0))
		fs.Usage()
		return 2
	}
	a := []string{"-version", "-color", color.String()}
	if *asJSON {
		a = append(a, "-json")
	}
	return auditcli.Main(version, a, stdout, stderr)
}
