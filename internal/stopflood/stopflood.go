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

// Junk is the number of junk entries the guard writes at its larger size; the smaller size is a quarter of it.
const Junk = 10000

// Bound is the longest any measured call may take over Junk entries. The measured time is under 0.1 s on
// SQLite and Postgres, so the bound is about 25 times that: loose enough for a shared runner, and a cheap
// quadratic over Junk entries (seconds) fails it.
const Bound = 2 * time.Second

// Slack is the constant added to the growth check so that timings near the clock's noise do not fail it.
const Slack = 50 * time.Millisecond

// Times are the timings of one store holding some junk. Each is the least of three runs.
type Times struct {
	Set      time.Duration // SetStop of the global scope (the stop is lifted again between runs)
	State    time.Duration // StopControl.State
	First    time.Duration // a new StopSwitch's first synchronous Hold: reads and folds the whole run
	Steady   time.Duration // the next synchronous Hold: nothing new to read
	AfterNew time.Duration // a synchronous Hold after one new entry: reads it and folds the run again
}

// Linear reports whether large, measured at four times the junk of small, is consistent with cost linear in
// the number of entries: at most 8 times small plus Slack (linear gives about 4 times, quadratic 16).
func Linear(small, large time.Duration) bool { return large <= 8*small+Slack }

// Measure writes junk entries under the global scope's generation names in s, checks that a global stop can
// be set, is in force and has the generation after the junk, and returns the timings.
func Measure(ctx context.Context, t *testing.T, s agent.Store, junk int) Times {
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
	// (agent's setNamePrefix; the generation check below fails if this copy drifts from it).
	sum := sha256.Sum256([]byte(string(scope.Kind) + "\x00" + scope.Name + "\x00" + agent.CompensationAllow))
	prefix := "@stop/set/" + hex.EncodeToString(sum[:]) + "/"
	put := func(name string) {
		data, err := agent.JournalEntry(name, agent.Record{Kind: agent.StepValue, Result: []byte(`"junk"`)})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Insert(ctx, agent.StopControlRun, name, data); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= junk; i++ {
		put(prefix + fmt.Sprint(i))
	}
	var tm Times
	hold := func(sw *agent.StopSwitch) time.Duration {
		t0 := time.Now()
		if h := sw.Hold(ctx, agent.RunRef{RunID: "r"}, agent.StopQuery{Tool: "x"}, true, true); h != nil && h.Cause != agent.StopCauseStop {
			t.Fatalf("the switch could not read the run: %+v", h)
		}
		return time.Since(t0)
	}
	minOf := func(cur *time.Duration, i int, v time.Duration) {
		if i == 0 || v < *cur {
			*cur = v
		}
	}
	for i := range 3 {
		sw, err := agent.NewStopSwitch(c)
		if err != nil {
			t.Fatal(err)
		}
		minOf(&tm.First, i, hold(sw))
		minOf(&tm.Steady, i, hold(sw))
		put(prefix + "x" + fmt.Sprint(i)) // one new entry that is not a generation name
		minOf(&tm.AfterNew, i, hold(sw))
	}
	for i := range 3 {
		t0 := time.Now()
		e, err := c.SetStop(ctx, agent.StopRequest{Scope: scope, Reason: "incident", By: "ops", Host: "h"})
		minOf(&tm.Set, i, time.Since(t0))
		if err != nil {
			t.Fatalf("a flood of junk blocked the stop: %v", err)
		}
		t0 = time.Now()
		st, err := c.State(ctx)
		minOf(&tm.State, i, time.Since(t0))
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
		if _, err := c.LiftStop(ctx, e.ID, agent.LiftRequest{By: "ops2"}); err != nil {
			t.Fatal(err)
		}
	}
	// The junk sits on generation names 1..junk, so the stops are only written after it if the prefix is the real
	// one; each of the three global stops takes the next generation.
	recs, err := c.StopRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var gens []int
	for _, r := range recs {
		if r.Kind == "set" && r.Scope != nil && r.Scope.Kind == agent.StopGlobal {
			gens = append(gens, r.Gen)
		}
	}
	if len(gens) != 3 || gens[0] != junk+1 || gens[1] != junk+2 || gens[2] != junk+3 {
		t.Fatalf("the global stops' generations are %v, want %d..%d: the junk is not under the stop's generation names", gens, junk+1, junk+3)
	}
	t.Logf("%d junk entries: %+v", junk, tm)
	return tm
}

// Guard runs Measure on a fresh store from open at Junk/4 and at Junk entries and fails if any timing at Junk
// is past Bound or grew faster than linearly from the smaller size.
func Guard(ctx context.Context, t *testing.T, open func(t *testing.T) agent.Store) {
	t.Helper()
	small := Measure(ctx, t, open(t), Junk/4)
	large := Measure(ctx, t, open(t), Junk)
	for _, c := range []struct {
		name         string
		small, large time.Duration
		grows        bool
	}{
		{"SetStop", small.Set, large.Set, true},
		{"State", small.State, large.State, true},
		{"first Hold", small.First, large.First, true},
		{"Hold after a new entry", small.AfterNew, large.AfterNew, true},
		{"steady Hold", small.Steady, large.Steady, false},
	} {
		if c.large > Bound {
			t.Errorf("%s took %v over %d junk entries (bound %v)", c.name, c.large, Junk, Bound)
		}
		if c.grows && !Linear(c.small, c.large) {
			t.Errorf("%s grew faster than linearly: %v over %d junk entries, %v over %d", c.name, c.small, Junk/4, c.large, Junk)
		}
	}
}
