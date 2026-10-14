// Command approvepage writes the approve page's files (approvals.PageFiles) to a directory, with a
// SHA256SUMS file in the format sha256sum -c reads, for an organization that hosts the page on an
// origin of its own. The build is deterministic: the same release yields the same bytes, so the sums
// published with a release are the sums of what this command writes, and a deployment verifies them
// before it serves the page:
//
//	go run github.com/bide-ai/bide/approvals/cmd/approvepage -out ./site
//	(cd site && sha256sum -c SHA256SUMS)
//
// A config.json naming the intake, {"intake": "https://app.example.com/approvals"}, goes next to the
// files; it is the deployment's, not the release's, and is not in SHA256SUMS.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bide-ai/bide/approvals"
)

func main() {
	out := flag.String("out", "", "directory to write the page into (created if absent)")
	flag.Parse()
	if *out == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: approvepage -out <dir>")
		os.Exit(2)
	}
	if err := write(*out); err != nil {
		fmt.Fprintln(os.Stderr, "approvepage:", err)
		os.Exit(1)
	}
}

func write(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	files := approvals.PageFiles()
	sums := approvals.PageSHA256()
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	slices.Sort(names)
	var list strings.Builder
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), files[n], 0o644); err != nil {
			return err
		}
		fmt.Fprintf(&list, "%s  %s\n", sums[n], n)
	}
	return os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(list.String()), 0o644)
}
