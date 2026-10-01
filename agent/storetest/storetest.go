// Package storetest checks that an agent.Store meets the requirements the journal builds on (A1 to
// A8, documented on agent.Store), that a Journal over it writes and checks the journal format
// header, and that records round-trip byte for byte. Every store in this module runs it, and a new
// backend should too:
//
//	func TestStore(t *testing.T) {
//	    db := openTestDatabase(t)
//	    storetest.Run(t, func(t *testing.T) agent.Store { return myStoreOn(db) })
//	}
//
// open may be called several times in one test; every handle it returns must reach the same data
// (a new connection to the same database, or the same in-memory store), since the suite checks
// what one handle's writes look like through another. Run IDs are unique per call, so the suite
// can run repeatedly against a persistent backend.
package storetest

import (
	"testing"

	"github.com/bide-ai/bide/agent"
)

// Run runs the whole suite: the store requirements (A1 to A8), the Lister contract when the store
// implements agent.Lister (with RunFilter.LeaseLapsed over its leases, or over none without
// agent.Leaser), the journal header checks, the Journal's shared in-flight steps and
// claims, and the record-fidelity suite over a Journal on the store.
func Run(t *testing.T, open func(t *testing.T) agent.Store) {
	t.Run("A1_UniqueNames", func(t *testing.T) { uniqueNames(t, open) })
	t.Run("A1_AtMostOnceEffect", func(t *testing.T) { atMostOnceEffect(t, open) })
	t.Run("A2_PrefixClosed", func(t *testing.T) { prefixClosed(t, open) })
	t.Run("A3_RetrySameBytes", func(t *testing.T) { retrySameBytes(t, open(t)) })
	t.Run("A4_ReadYourWrites", func(t *testing.T) { readYourWrites(t, open) })
	t.Run("A5_ByteFidelity", func(t *testing.T) { byteFidelity(t, open(t)) })
	t.Run("A6_Immutable", func(t *testing.T) { immutable(t, open(t)) })
	t.Run("A7_Context", func(t *testing.T) { honorsContext(t, open(t)) })
	t.Run("A8_BreakEarly", func(t *testing.T) { breakEarly(t, open(t)) })
	t.Run("A8_WriteInsideLoad", func(t *testing.T) { writeInsideLoad(t, open(t)) })
	if _, ok := agent.Capability[agent.Lister](open(t)); ok {
		t.Run("Lister", func(t *testing.T) { lister(t, open(t)) })
		t.Run("Lister_LeaseLapsed", func(t *testing.T) { leaseLapsed(t, open(t)) })
	}
	t.Run("Header_First", func(t *testing.T) { headerFirst(t, open(t)) })
	t.Run("Header_ConcurrentFirstWriters", func(t *testing.T) { concurrentFirstWriters(t, open) })
	t.Run("Header_UnsupportedFormatRefused", func(t *testing.T) { unsupportedFormat(t, open(t)) })
	t.Run("Header_HeaderlessRefused", func(t *testing.T) { headerless(t, open(t)) })
	t.Run("Header_ReadRacingFirstWrite", func(t *testing.T) { readRacingFirstWrite(t, open) })
	t.Run("Journal_SharedInFlightSteps", func(t *testing.T) { sharedFlights(t, open(t)) })
	t.Run("Journal_AmbiguousClaimReused", func(t *testing.T) { ambiguousClaim(t, open(t)) })
	t.Run("Fidelity", func(t *testing.T) {
		RunDurable(t, func(t *testing.T) agent.Durable { return journal(t, open(t)) })
	})
}

// journal returns a new Journal over s.
func journal(t *testing.T, s agent.Store) *agent.Journal {
	t.Helper()
	j, err := agent.NewJournal(s)
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}
	return j
}
