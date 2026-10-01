// Command modelsync keeps bide's Go code and its TLA+ models (spec/tla) in step. It is the path
// rule of milestone M4 in docs/design/formal-models.md (section 6.3).
//
// The Go code a model describes is wrapped in region markers that name the model, by its
// directory under spec/tla, and the model actions (PlusCal labels or TLA+ operators) the region
// implements:
//
//	// protocol:claims begin Claim ClaimRetry ClaimInsert
//	...
//	// protocol:claims end
//
// modelsync runs two checks.
//
// The consistency check reads the working tree. Every marker is well formed and paired (regions
// of one model do not nest; regions of different models may overlap); every action a marker names
// is defined in the model's spec (spec/tla/<model>/<Model>.tla) and listed in the model-to-code
// map of spec/tla/README.md; and every action the map lists has at least one marker, unless the
// README lists it as having no Go region. The README marks a map table with the comment
// "<!-- modelsync: map <model> -->" on the line before it, and the actions without Go code with
// "<!-- modelsync: no-code <model> <Action> ... -->". A renamed action, in the spec, the map or
// the code, fails one of these.
//
// The path rule (with -base) diffs the merge base of -base and -head against -head. If a change
// touches a marked region of a model (a line inside it, its markers, or lines deleted from it)
// and changes no file under spec/tla/<model>/, the check fails, unless a commit message in the
// range or a description file (-body) holds an override line:
//
//	Protocol-Impact: none (<reason>)
//	Protocol-Impact: claims,flows none (<reason>)
//
// The first form covers every model; the second only the models it names. The reason is
// required. Each override used is printed as a warning (a GitHub annotation under Actions), so it
// is visible in the check's log and on the pull request.
//
// Usage, from the repository root:
//
//	go run ./internal/tools/modelsync                          # the consistency check only
//	go run ./internal/tools/modelsync -base origin/main        # and the path rule against main
//	go run ./internal/tools/modelsync -base B -head H -body pr-body.txt
//
// The exit status is 0 when clean, 1 on findings, and 2 when the check could not run.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// listFlag collects a repeated string flag.
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(s string) error { *l = append(*l, s); return nil }

func run(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("modelsync", flag.ContinueOnError)
	fl.SetOutput(stderr)
	root := fl.String("root", ".", "repository root (a git work tree)")
	base := fl.String("base", "", "base revision for the path rule; empty runs the consistency check only")
	head := fl.String("head", "HEAD", "head revision for the path rule")
	var bodies listFlag
	fl.Var(&bodies, "body", "file holding a pull request description to read override lines from (repeatable)")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if fl.NArg() != 0 {
		fmt.Fprintf(stderr, "modelsync: unexpected arguments %q\n", fl.Args())
		return 2
	}
	r := &report{out: stdout, annotate: os.Getenv("GITHUB_ACTIONS") == "true"}

	models, err := loadModels(*root, r)
	if err != nil {
		fmt.Fprintf(stderr, "modelsync: %v\n", err)
		return 2
	}
	regions, err := scanTree(*root, models, r)
	if err != nil {
		fmt.Fprintf(stderr, "modelsync: %v\n", err)
		return 2
	}
	checkConsistency(models, regions, r)

	if *base != "" {
		var texts []source
		for _, b := range bodies {
			data, err := os.ReadFile(b)
			if err != nil {
				fmt.Fprintf(stderr, "modelsync: %v\n", err)
				return 2
			}
			texts = append(texts, source{name: "description " + b, text: string(data)})
		}
		if err := checkPathRule(*root, *base, *head, models, texts, r); err != nil {
			fmt.Fprintf(stderr, "modelsync: %v\n", err)
			return 2
		}
	}

	if r.errors > 0 {
		fmt.Fprintf(stdout, "modelsync: %d finding(s). See spec/tla/README.md (Keeping the code and the models in step).\n", r.errors)
		return 1
	}
	fmt.Fprintln(stdout, "modelsync: ok")
	return 0
}

// report prints findings, as GitHub annotations when running under Actions.
type report struct {
	out      io.Writer
	annotate bool
	errors   int
}

func (r *report) errorf(format string, a ...any) {
	r.errors++
	msg := fmt.Sprintf(format, a...)
	if r.annotate {
		fmt.Fprintf(r.out, "::error title=modelsync::%s\n", msg)
		return
	}
	fmt.Fprintf(r.out, "error: %s\n", msg)
}

func (r *report) warnf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	if r.annotate {
		fmt.Fprintf(r.out, "::warning title=modelsync override::%s\n", msg)
		return
	}
	fmt.Fprintf(r.out, "warning: %s\n", msg)
}

func (r *report) infof(format string, a ...any) {
	fmt.Fprintf(r.out, format+"\n", a...)
}
