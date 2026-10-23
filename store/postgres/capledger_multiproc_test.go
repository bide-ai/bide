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

// A real crash: one worker is SIGKILLed in the middle of its reservations and a replacement of the same
// name resumes them. The ledger keeps its shape, no attempt holds two slots, the window is exactly full,
// and a reconcile pass releases nothing.
func TestCapLedger_SIGKILLMidReservationPostgres(t *testing.T) {
	s, _ := openTestStore(t)
	p := capledgertest.Params{Procs: 3, Own: 10, Shared: 4, Limit: 20, Namespace: fmt.Sprintf("mpk-%d", time.Now().UnixNano()), Kill: true}
	capledgertest.Run(t, agenttest.MustJournal(s), p, nil)
}

// Releases across processes: every worker releases its first own attempt and races the others through a
// reconcile pass over one shared slot, which is released exactly once.
func TestCapLedger_ReleaseAcrossProcessesOnPostgres(t *testing.T) {
	s, _ := openTestStore(t)
	p := capledgertest.Params{Procs: 4, Own: 8, Shared: 4, Limit: 20, Namespace: fmt.Sprintf("mpr-%d", time.Now().UnixNano()), Release: true}
	capledgertest.Run(t, agenttest.MustJournal(s), p, nil)
}
