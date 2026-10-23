package sqlite

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/capledger/capledgertest"
)

// The cap ledger across real processes on one SQLite file (C2, C10): worker processes are
// re-executions of this test binary (TestMain dispatches them), each opening the file and reserving
// against one window.

const capDBEnv = "BIDE_CAPLEDGER_SQLITE"

func TestMain(m *testing.M) {
	if capledgertest.IsWorker() {
		os.Exit(capledgertest.WorkerMain(func(context.Context) (*agent.Journal, func(), error) {
			s, err := Open(os.Getenv(capDBEnv))
			if err != nil {
				return nil, nil, err
			}
			return agenttest.MustJournal(s), func() { _ = s.Close() }, nil
		}))
	}
	os.Exit(m.Run())
}

func TestCapLedger_AcrossProcessesOnSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "caps.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := capledgertest.Params{Procs: 4, Own: 12, Shared: 6, Limit: 30, Namespace: fmt.Sprintf("mp-%d", time.Now().UnixNano())}
	capledgertest.Run(t, agenttest.MustJournal(s), p, []string{capDBEnv + "=" + path})
}
