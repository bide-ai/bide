// Command bide-view opens a local, read-only web page that shows a run from a bide journal: a
// plan flow's declared topology with the run's path drawn over it, or an agent run's timeline of
// model turns, tool calls (with their Safety), attempts, approvals, halts and its end state, and
// whether the journal verifies against a signed tree head or an evidence package.
//
//	bide-view -db run.db                       # the run list
//	bide-view -db run.db -run refund-42        # one run
//	bide-view -journal run.json                # an exported journal (audit.ExportJournal)
//	bide-view -db run.db -evidence evidence.json -pubkey ed25519:<hex>
//	bide-view -db flow.db -topology topology.json -digest <flow.Digest()>
//
// It opens the SQLite file read-only (sqlite.OpenReadOnly) and serves on a loopback address
// only (-addr, default 127.0.0.1:8484). The page needs no network: its HTML, CSS and graph are
// served by the binary.
package main

import (
	"io"
	"os"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/store/sqlite"
)

func main() {
	os.Exit(runview.Main(os.Args[1:], os.Stdout, os.Stderr, func(path string) (agent.Store, io.Closer, error) {
		s, err := sqlite.OpenReadOnly(path)
		if err != nil {
			return nil, nil, err
		}
		return s, s, nil
	}))
}
