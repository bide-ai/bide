// Command bide-audit produces and verifies audit ProofBundles from the command line, for the
// auditor/compliance persona who does not write Go. The command is package auditcli; see its
// documentation for the verbs, the inputs and the exit statuses.
package main

import (
	"os"

	"github.com/bide-ai/bide/internal/auditcli"
)

// version is the release version, set by a release build with -ldflags "-X main.version=v1.2.3".
var version = ""

func main() {
	os.Exit(auditcli.Main(version, os.Args[1:], os.Stdout, os.Stderr))
}
