// Command bide-view moved: it is now bide view, a subcommand of the bide command. This program
// only says so, for one release, so that go install github.com/bide-ai/bide/cmd/bide-view@latest
// does not keep installing the last viewer silently. Install bide instead:
//
//	go install github.com/bide-ai/bide/cmd/bide@latest
//	bide view -db run.db
package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	fmt.Fprintf(os.Stderr, "bide-view is now bide view. Install bide with\n\n\tgo install github.com/bide-ai/bide/cmd/bide@latest\n\nor brew install bide-ai/tap/bide, then run:\n\n\tbide view %s\n", strings.Join(os.Args[1:], " "))
	os.Exit(2)
}
