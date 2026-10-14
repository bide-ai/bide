package plan

import (
	"context"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// A finished flow run's run:complete notes when it was written (Record.RecordedAt), beside the
// flow's output, and agent.Status reports it as EndedAt. A later drive returns the output and
// keeps the first time.
func TestRunComplete_FlowRecordsEndTime(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	var fired int
	flow := effectFlow(t, &fired, Idempotent())
	before := time.Now().Add(-time.Second)
	if out, err := flow.Run(ctx, j, "r", 5); err != nil || out != "charged 5" {
		t.Fatalf("Run = %q, %v", out, err)
	}
	after := time.Now().Add(time.Second)
	rec, ok, err := j.Get(ctx, "r", "run:complete")
	if err != nil || !ok {
		t.Fatalf("run:complete: %v, %v", ok, err)
	}
	at, err := agent.ParseTimestamp(rec.RecordedAt())
	if err != nil || at.Before(before) || at.After(after) {
		t.Fatalf("recorded_at %q (%v), want a canonical time within the run", rec.RecordedAt(), err)
	}
	if st, err := agent.Status(ctx, j, "r"); err != nil || st.State != agent.RunCompleted || st.EndedAt != rec.RecordedAt() {
		t.Fatalf("Status = %+v, %v, want completed at %q", st, err, rec.RecordedAt())
	}
	if out, err := flow.Run(ctx, j, "r", 5); err != nil || out != "charged 5" {
		t.Fatalf("second Run = %q, %v", out, err)
	}
	if again, _, _ := j.Get(ctx, "r", "run:complete"); again.RecordedAt() != rec.RecordedAt() {
		t.Fatalf("a later drive changed the end time to %q", again.RecordedAt())
	}
}
