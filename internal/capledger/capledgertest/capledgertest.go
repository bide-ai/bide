// Package capledgertest gives the tests of bide's stores a cap ledger over a Journal on their store:
// they run it across real processes (store/sqlite, store/postgres) and under an audited store, which
// the unexported ledger cannot be reached for from another module. It is internal: the engine and the
// exported API do not use the ledger yet.
package capledgertest

import (
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/capledger"
	"github.com/bide-ai/bide/internal/journalhook"
)

// New returns a cap ledger over j, reading now from clock; bound is how many windows later than the
// newest recorded one a first intent may be (0 for one).
func New(j *agent.Journal, clock func() time.Time, bound int) *capledger.Ledger {
	return journalhook.CapLedger(j, clock, bound)
}
