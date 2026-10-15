package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/store/sqlite"
)

// The story the demo tells: the flow pauses before the refund, runs it once at 2 of 3, and the
// exported evidence verifies; a denial by two approvers stops it without a refund.
func TestFlowGateStory(t *testing.T) {
	ctx := context.Background()
	open := func(t *testing.T) *agent.Journal {
		store, err := sqlite.Open(filepath.Join(t.TempDir(), "flow.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		j, err := agent.NewJournal(store)
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	t.Run("approved", func(t *testing.T) {
		j, refunds := open(t), 0
		for _, step := range []func() error{
			func() error { return cmdRun(ctx, j, &refunds) },
			func() error { return cmdApprove(ctx, j, "ops", true) },
			func() error { return cmdRun(ctx, j, &refunds) },
			func() error { return cmdApprove(ctx, j, "finance", true) },
			func() error { return cmdRun(ctx, j, &refunds) },
			func() error { return cmdRun(ctx, j, &refunds) },
		} {
			if err := step(); err != nil {
				t.Fatal(err)
			}
		}
		if refunds != 1 {
			t.Fatalf("refunds = %d, want 1", refunds)
		}
		// refund.json's timeout was read: the gate recorded its deadline when it first waited.
		recs, err := j.History(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		dl, _, ok := agent.RecordedDeadline(recs, gateKey)
		if !ok || dl.TimeoutMS != (48*time.Hour).Milliseconds() {
			t.Fatalf("recorded deadline = %+v (ok=%v), want a 48h timeout", dl, ok)
		}
		dir := t.TempDir()
		if err := cmdEvidence(ctx, j, filepath.Join(dir, "e.json"), filepath.Join(dir, "k.json")); err != nil {
			t.Fatal(err)
		}
		if err := cmdVerify(filepath.Join(dir, "e.json")); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("denied", func(t *testing.T) {
		j, refunds := open(t), 0
		for _, step := range []func() error{
			func() error { return cmdRun(ctx, j, &refunds) },
			func() error { return cmdApprove(ctx, j, "ops", false) },
			func() error { return cmdApprove(ctx, j, "risk", false) },
			func() error { return cmdRun(ctx, j, &refunds) },
		} {
			if err := step(); err != nil {
				t.Fatal(err)
			}
		}
		if refunds != 0 {
			t.Fatalf("refunds = %d after a denial, want 0", refunds)
		}
	})
}
