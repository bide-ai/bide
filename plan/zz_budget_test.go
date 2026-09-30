package plan_test

import (
	"context"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// Budget of a loop + switch + join flow: a fresh drive, a replay in a new process, a resume at
// a halted node in a new process.
func TestZBudgetLoopSwitch(t *testing.T) {
	ctx := context.Background()
	for _, sub := range fSubjects() {
		cs := agenttest.NewCountingStore(agent.NewMemStore())
		p := &fProc{h: &fHarness{mem: agent.NewMemStore(), crash: map[int]int{}, acked: map[string]bool{}}}
		p.h.drive = -1
		flow, err := sub.build(p, 0)
		if err != nil {
			t.Fatal(err)
		}
		j, _ := agent.NewJournal(cs)
		if out, err := flow.Run(ctx, j, "r", sub.in); err != nil || out != sub.want {
			t.Fatalf("%s: %q %v", sub.name, out, err)
		}
		c := cs.Reset()
		t.Logf("%s fresh: Load %d Get %d Inserted %d", sub.name, c.Load, c.Get, c.Inserted)
		j2, _ := agent.NewJournal(cs)
		if out, err := flow.Run(ctx, j2, "r", sub.in); err != nil || out != sub.want {
			t.Fatalf("%s replay: %q %v", sub.name, out, err)
		}
		c = cs.Reset()
		t.Logf("%s replay new journal: Load %d Get %d Inserted %d", sub.name, c.Load, c.Get, c.Inserted)
		if c.Load > 1 || c.Inserted != 0 {
			t.Errorf("%s replay: Load %d Inserted %d", sub.name, c.Load, c.Inserted)
		}
	}
}
