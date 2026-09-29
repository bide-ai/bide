package benchmarks

import (
	"testing"

	"github.com/bide-ai/bide/chaos"
)

// Writes is the chaos sweep's bound: Verify crashes at write 1..Writes()+2 and draws every
// randomized crash point from that range. For an adapter whose crash is injected at a write
// index, Writes must be the writes a clean run makes: a crash at the last one happens, and a
// crash one past it does not (no write is left to crash at). An over-count adds crash points
// that crash nothing; an under-count can leave a write that is never crashed at.
//
// eino and langchaingo are not here: they have no durable store to index a crash into, so any
// crashAt > 0 crashes the whole run (see eino.go and langchaingo.go) and Writes is only the
// sweep width.
func TestWrites_MatchACleanRun(t *testing.T) {
	for _, c := range []struct {
		name string
		sys  chaos.System
	}{{"trpc-agent-go", TRPC()}, {"adk-go", ADK()}} {
		w := c.sys.Writes()
		if !c.sys.NewRun().Step(w) {
			t.Errorf("%s: Writes() = %d, but a crash at write %d does not happen: a clean run makes fewer writes", c.name, w, w)
		}
		if c.sys.NewRun().Step(w + 1) {
			t.Errorf("%s: Writes() = %d, but a crash at write %d still happens: a clean run makes more writes", c.name, w, w+1)
		}
	}
}

// Every adapter's completed runs fire the charge: none reaches a terminal state without it, so
// the cross-SDK results measure double-fires, not systems skipping the work.
func TestComparison_NoAdapterCompletesWithoutFiring(t *testing.T) {
	for _, c := range []struct {
		name string
		sys  chaos.System
	}{
		{"Bide", chaos.Bide()},
		{"trpc-agent-go", TRPC()},
		{"langchaingo", LangChainGo()},
		{"eino", EinoGraph()},
		{"adk-go", ADK()},
		{"naive-loop", chaos.NaiveReference()},
	} {
		if rep := chaos.Verify(c.name, c.sys, 200); rep.Missed != 0 {
			t.Errorf("%s: %d runs completed without firing: %v", c.name, rep.Missed, rep)
		}
	}
}
