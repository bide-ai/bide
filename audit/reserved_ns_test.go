package audit_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// The audit package's journal writers refuse a run under @cap/ (a cap ledger is the engine's) and
// write nothing.
func TestRecordWritersRefuseCapRuns(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	j, err := agent.NewJournal(store)
	if err != nil {
		t.Fatal(err)
	}
	run := "@cap/ns/refunds/usd/d2026-10-22/0123abcd"
	if _, err := audit.RecordPolicy(ctx, j, run, []byte("p"), "d"); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("RecordPolicy = %v, want ErrConfig", err)
	}
	if _, err := audit.RecordConvergence(ctx, j, run, []byte(`{}`), "d"); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("RecordConvergence = %v, want ErrConfig", err)
	}
	for id, err := range store.Runs(ctx, agent.RunFilter{}) {
		t.Errorf("the store holds run %q (%v) after refused writes", id, err)
	}
}
