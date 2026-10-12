// Command auditmain is the audit CLI as a program of its own, which the auditcli tests build to
// run a binary (exit statuses, the version stamp, files resolved from the working directory).
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
