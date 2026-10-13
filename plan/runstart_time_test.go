package plan

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// A flow run's run:start records when it started (agent.RunStart.StartedAt), and a later drive
// neither rewrites it nor reads it: a run whose run:start holds none, as an earlier version wrote
// it, resumes and finishes the same way.
func TestRunStart_FlowRecordsStartTime(t *testing.T) {
	ctx := context.Background()
	before := time.Now().Truncate(time.Millisecond)
	store := agent.NewMemStore()
	j := agenttest.MustJournal(store)
	var fired int
	flow := effectFlow(t, &fired, Idempotent())
	if _, err := flow.Run(ctx, j, "r", 5); err == nil {
		t.Fatal("first drive: want the node's error")
	}
	st, ok, err := agent.RecordedStart(ctx, j, "r")
	if err != nil || !ok {
		t.Fatalf("RecordedStart: %v, %v", ok, err)
	}
	at, ok := st.Started()
	if !ok || at.Before(before) || at.After(time.Now()) || at.Location() != time.UTC {
		t.Fatalf("StartedAt %q (%v, %v), want the time of the first drive, UTC", st.StartedAt, at, ok)
	}

	// The same journal with its start time removed.
	edited := agent.NewMemStore()
	for e, err := range store.Load(ctx, "r", -1) {
		if err != nil {
			t.Fatal(err)
		}
		data := e.Data
		if e.Name == "run:start" {
			data = regexp.MustCompile(`,"started_at":"[^"]*"`).ReplaceAll(data, nil)
		}
		if _, _, err := edited.Insert(ctx, "r", e.Name, data); err != nil {
			t.Fatal(err)
		}
	}
	ej := agenttest.MustJournal(edited)
	firedBefore := fired
	if out, err := flow.Run(ctx, ej, "r", 5); err != nil || out != "charged 5" || fired != firedBefore+1 {
		t.Fatalf("resume without a start time = %q, %v (charge ran %d more times)", out, err, fired-firedBefore)
	}
	if st, _, _ := agent.RecordedStart(ctx, ej, "r"); st.StartedAt != "" {
		t.Fatalf("a later drive wrote a start time: %q", st.StartedAt)
	}
	if out, err := flow.Run(ctx, j, "r", 5); err != nil || out != "charged 5" {
		t.Fatalf("resume = %q, %v", out, err)
	}
	if again, _, _ := agent.RecordedStart(ctx, j, "r"); again.StartedAt != st.StartedAt {
		t.Fatalf("a later drive rewrote the start time: %q, was %q", again.StartedAt, st.StartedAt)
	}
}
