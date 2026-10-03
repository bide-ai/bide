package agent_test

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"sync"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

var errCommittedThenLost = errors.New("connection lost after commit")

// sweepStore stores the k-th new entry and then reports an error for it (A3's "complete on
// error" half), once.
type sweepStore struct {
	m      *agent.MemStore
	mu     sync.Mutex
	writes int
	k      int
	failed string
}

func (c *sweepStore) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	e, ok, err := c.m.Insert(ctx, runID, name, data)
	if err != nil || !ok {
		return e, ok, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes++
	if c.writes == c.k {
		c.failed = name
		return agent.Entry{}, false, errCommittedThenLost
	}
	return e, ok, nil
}
func (c *sweepStore) Get(ctx context.Context, r, n string) (agent.Entry, bool, error) {
	return c.m.Get(ctx, r, n)
}
func (c *sweepStore) Load(ctx context.Context, r string, a int64) iter.Seq2[agent.Entry, error] {
	return c.m.Load(ctx, r, a)
}

// For every write of a run with one side-effect call, the write commits and reports an error.
// The run is then driven again, in the same process (same store value) or in a new one (a
// different store value over the same data). The side effect must fire at most once, the run must
// end complete or halted, and it must not halt over an effect that never ran.
func TestCommitThenFailSweep(t *testing.T) {
	for k := 1; k <= 7; k++ {
		for _, sameProcess := range []bool{true, false} {
			t.Run(fmt.Sprintf("write%d/same=%v", k, sameProcess), func(t *testing.T) {
				ctx := context.Background()
				m := agent.NewMemStore()
				fired := 0
				charge := agent.MustFunc("charge", "", func(context.Context, struct{}) (string, error) { fired++; return "ok", nil })
				model := func() agent.Model {
					return agent.NewScriptedModel(agent.ToolTurn("c1", "charge", `{}`), agent.TextTurn("done"))
				}
				s := &sweepStore{m: m, k: k}
				j, _ := agent.NewJournal(s)
				_, err1 := agenttest.MustNew(model(), j, agent.WithTools(charge), agent.WithMaxConcurrency(1)).Run(ctx, "r", agent.UserText("hi"))
				var j2 *agent.Journal
				if sameProcess {
					j2, _ = agent.NewJournal(s) // same store value: shares the process's memory
				} else {
					j2, _ = agent.NewJournal(&sweepStore{m: m}) // a new process
				}
				_, err2 := agenttest.MustNew(model(), j2, agent.WithTools(charge), agent.WithMaxConcurrency(1)).Run(ctx, "r", agent.UserText("hi"))
				var halt *agent.OutcomeUnknown
				t.Logf("failed %q: first %v; second %v; fired %d", s.failed, err1, err2, fired)
				if fired > 1 {
					t.Fatalf("fired %d times", fired)
				}
				if err2 != nil && !errors.As(err2, &halt) {
					t.Errorf("second drive = %v, want complete or halted", err2)
				}
				if errors.As(err2, &halt) && fired == 0 {
					t.Errorf("halted over an effect that never ran: the claimant knew it did not start it")
				}
			})
		}
	}
}
