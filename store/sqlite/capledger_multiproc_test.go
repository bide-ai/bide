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
	"github.com/bide-ai/bide/internal/capledger/capledgerproc"
)

// The cap ledger across real processes on one SQLite file (C2, C10): worker processes are
// re-executions of this test binary (TestMain dispatches them), each opening the file and reserving
// against one window.

const capDBEnv = "BIDE_CAPLEDGER_SQLITE"

func TestMain(m *testing.M) {
	if capledgerproc.IsWorker() {
		os.Exit(capledgerproc.WorkerMain(func(context.Context) (*agent.Journal, func(), error) {
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
	p := capledgerproc.Params{Procs: 4, Own: 12, Shared: 6, Limit: 30, Namespace: fmt.Sprintf("mp-%d", time.Now().UnixNano())}
	capledgerproc.Run(t, agenttest.MustJournal(s), p, []string{capDBEnv + "=" + path})
}

// A real crash: one worker is SIGKILLed in the middle of its reservations and a replacement of the same
// name resumes them. The ledger keeps its shape, no attempt holds two slots, the window is exactly full,
// and a reconcile pass releases nothing.
func TestCapLedger_SIGKILLMidReservationSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "caps.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := capledgerproc.Params{Procs: 3, Own: 10, Shared: 4, Limit: 20, Namespace: fmt.Sprintf("mpk-%d", time.Now().UnixNano()), Kill: true}
	capledgerproc.Run(t, agenttest.MustJournal(s), p, []string{capDBEnv + "=" + path})
}

// Releases across processes: every worker releases its first own attempt and races the others through a
// reconcile pass over one shared slot, which is released exactly once.
func TestCapLedger_ReleaseAcrossProcessesOnSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "caps.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := capledgerproc.Params{Procs: 4, Own: 8, Shared: 4, Limit: 20, Namespace: fmt.Sprintf("mpr-%d", time.Now().UnixNano()), Release: true}
	capledgerproc.Run(t, agenttest.MustJournal(s), p, []string{capDBEnv + "=" + path})
}
