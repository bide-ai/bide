// Package stopflood is the shared body of the store tests that guard the cost of junk under a stop's
// generation names. It is test support: nothing outside the repository's tests imports it.
package stopflood

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// Junk is the number of junk entries the guard writes.
const Junk = 10000

// Bound is the longest a SetStop or a State() may take over Junk entries. The measured time is about a tenth
// of a second on SQLite and 0.15 s on Postgres; the bound is generous so a shared runner does not fail it,
// and still catches a cost that grew faster than the number of entries.
const Bound = 10 * time.Second

// Run writes junk entries under the global scope's generation names in s, then checks that a global stop
// can be set, that it is in force, and that SetStop and State() each took less than Bound.
func Run(ctx context.Context, t *testing.T, s agent.Store, junk int) {
	t.Helper()
	c, err := agent.NewStopControl(agenttest.MustJournal(s), agent.WithStopMode(agent.StopBasic))
	if err != nil {
		t.Fatal(err)
	}
	// A first stop of another scope writes the control run's header, so the junk is entered into a run a
	// writer finds in use.
	if _, err := c.SetStop(ctx, agent.StopRequest{Scope: agent.StopScope{Kind: agent.StopTool, Name: "first"}, Reason: "setup", By: "ops", Host: "h"}); err != nil {
		t.Fatal(err)
	}
	scope := agent.StopScope{Kind: agent.StopGlobal}
	// The generation names of the scope's sets: the hash of target kind, name and compensation, then the number
	// (agent's setNamePrefix; the check below fails if this copy drifts from it).
	sum := sha256.Sum256([]byte(string(scope.Kind) + "\x00" + scope.Name + "\x00" + agent.CompensationAllow))
	prefix := "@stop/set/" + hex.EncodeToString(sum[:]) + "/"
	for i := 1; i <= junk; i++ {
		name := prefix + fmt.Sprint(i)
		data, err := agent.JournalEntry(name, agent.Record{Kind: agent.StepValue, Result: []byte(`"junk"`)})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Insert(ctx, agent.StopControlRun, name, data); err != nil {
			t.Fatal(err)
		}
	}
	t0 := time.Now()
	e, err := c.SetStop(ctx, agent.StopRequest{Scope: scope, Reason: "incident", By: "ops", Host: "h"})
	setTook := time.Since(t0)
	if err != nil {
		t.Fatalf("a flood of junk blocked the stop: %v", err)
	}
	t0 = time.Now()
	st, err := c.State(ctx)
	stateTook := time.Since(t0)
	if err != nil {
		t.Fatal(err)
	}
	inForce := false
	for _, a := range st.Active() {
		inForce = inForce || a.ID == e.ID
	}
	if !inForce {
		t.Fatalf("the global stop %s is not in force", e.ID)
	}
	// The junk sits on generation names 1..junk, so the stop is only written there if the prefix is the real one.
	recs, err := c.StopRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[1].Gen != junk+1 {
		t.Fatalf("the stop's generation is not %d: the junk is not under the stop's generation names: %+v", junk+1, recs)
	}
	t.Logf("%d junk entries: SetStop %v, State %v", junk, setTook, stateTook)
	if setTook > Bound || stateTook > Bound {
		t.Fatalf("SetStop took %v and State took %v over %d junk entries (bound %v)", setTook, stateTook, junk, Bound)
	}
}
