//go:build unix

package postgres

import (
	"fmt"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/capledger/capledgertest"
)

// The cap ledger across real processes on one Postgres database (C2, C10): worker processes are
// re-executions of this test binary (TestMain dispatches them), each reserving against one window.
// Skips without PG_DSN.
func TestCapLedger_AcrossProcessesOnPostgres(t *testing.T) {
	s, _ := openTestStore(t)
	p := capledgertest.Params{Procs: 4, Own: 12, Shared: 6, Limit: 30, Namespace: fmt.Sprintf("mp-%d", time.Now().UnixNano())}
	capledgertest.Run(t, agenttest.MustJournal(s), p, nil)
}
