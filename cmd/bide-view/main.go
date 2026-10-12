// Command bide-view opens a local, read-only web page that shows a run from a bide journal: a
// plan flow's declared topology with the run's path drawn over it, or an agent run's timeline of
// model turns, tool calls (with their Safety), attempts, approvals, halts and its end state, and
// whether the journal verifies against a signed tree head or an evidence package (with approver
// keys, whether each m-of-n approval gate's signatures verify).
//
//	go install github.com/bide-ai/bide/cmd/bide-view@latest
//
//	bide-view -db run.db                       # the run list of a SQLite journal
//	bide-view -db run.db -run refund-42        # one run
//	bide-view -postgres postgres://user:pass@host:5432/db -run refund-42
//	bide-view -journal run.json                # an exported journal (audit.ExportJournal)
//	bide-view -db run.db -evidence evidence.json -pubkey ed25519:<hex> -approver-keys keys.json
//	bide-view -db flow.db -topology topology.json -digest <flow.Digest()>
//
// It opens a SQLite file read-only (sqlite.OpenReadOnly) and a Postgres journal in sessions the
// server holds read-only (postgres.OpenReadOnly), and serves on a loopback address only (-addr,
// default 127.0.0.1:8484). The page needs no network: its HTML, CSS and graph are served by the
// binary. bide-view -h lists every flag.
package main

import (
	"context"
	"io"
	"os"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/store/postgres"
	"github.com/bide-ai/bide/store/sqlite"
)

func main() {
	os.Exit(runview.Main(os.Args[1:], os.Stdout, os.Stderr, stores))
}

// stores opens each kind of journal bide-view reads, read-only.
var stores = runview.Stores{
	SQLite: func(path string) (agent.Store, io.Closer, error) {
		s, err := sqlite.OpenReadOnly(path)
		if err != nil {
			return nil, nil, err
		}
		return s, s, nil
	},
	Postgres: func(dsn string) (agent.Store, io.Closer, error) {
		s, err := postgres.OpenReadOnly(context.Background(), dsn)
		if err != nil {
			return nil, nil, err
		}
		return s, s, nil
	},
}
