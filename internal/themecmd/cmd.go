// Package themecmd is `bide theme`: the command that checks theme files.
package themecmd

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/bide-ai/bide/internal/themeaccess"
	"github.com/bide-ai/bide/theme"
)

// Main runs `bide theme`: today the one verb, check. It returns the exit status: 0 when every file
// passes, 1 when a file cannot be read or fails a check, 2 for a usage error.
func Main(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "check" {
		if len(args) == 1 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help") {
			usage(stdout)
			return 0
		}
		usage(stderr)
		return 2
	}
	fs := flag.NewFlagSet("bide theme check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: bide theme check [-v] <theme.json>...")
		fs.PrintDefaults()
	}
	verbose := fs.Bool("v", false, "print every measure, not only the failures and warnings")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return 2
	}
	status := 0
	for _, path := range fs.Args() {
		data, err := readLimited(path)
		if err != nil {
			fmt.Fprintf(stderr, "bide theme check: %s\n", themeaccess.Escape(err.Error()))
			status = 1
			continue
		}
		th, report, err := theme.Parse(data)
		if err != nil && report.Theme == "" {
			fmt.Fprintf(stderr, "%s: %s\n", themeaccess.Escape(path), themeaccess.Escape(err.Error()))
			status = 1
			continue
		}
		name := report.Theme
		if th != nil {
			name = th.Name()
		}
		printReport(stdout, themeaccess.Escape(path), themeaccess.Escape(name), report, *verbose)
		if err != nil {
			status = 1
		}
	}
	return status
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: bide theme check [-v] <theme.json>...")
	fmt.Fprintln(w, "  check parses theme files and measures them against the rules in docs/design/themes.md")
	fmt.Fprintln(w, "  -v    print every measure, not only the failures and warnings")
}

// printReport writes the failed measures (errors, then warnings) and a summary; with verbose it
// writes every measure.
func printReport(w io.Writer, path, name string, r theme.Report, verbose bool) {
	errs, warns := r.Failures(theme.Error), r.Failures(theme.Warning)
	for _, m := range r.Measures {
		switch {
		case !m.Pass && m.Severity == theme.Error:
			fmt.Fprintf(w, "%s: error   %s %s: %s\n", path, m.Check, m.Subject, m.String())
		case !m.Pass:
			fmt.Fprintf(w, "%s: warning %s %s: %s\n", path, m.Check, m.Subject, m.String())
		case verbose:
			fmt.Fprintf(w, "%s: ok      %s %s: %s\n", path, m.Check, m.Subject, m.String())
		}
	}
	verdict := "ok"
	if len(errs) > 0 {
		verdict = "refused"
	}
	fmt.Fprintf(w, "%s: theme %q %s (%d measures, %d errors, %d warnings)\n", path, name, verdict, len(r.Measures), len(errs), len(warns))
}

// readLimited reads a theme file, refusing one over theme.MaxSize without reading the rest of it, so
// a device or a huge file named by mistake cannot exhaust memory.
func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, theme.MaxSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > theme.MaxSize {
		return nil, fmt.Errorf("%s: theme file is over the %d byte limit", path, theme.MaxSize)
	}
	return data, nil
}
