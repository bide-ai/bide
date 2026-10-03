package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// store/sqlite leases runs, so ResolveHaltRef checks for a live driver with its lease, as on
// MemStore and Postgres: it refuses while another handle on the file holds the root run's lease,
// and resolves without WithMinHaltAge once the lease is free.
func TestResolveHaltRef_UsesTheLease(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "resolve.db")
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	j := agenttest.MustJournal(a)
	defer a.Close()
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if won, _, err := agent.ClaimAttempt(ctx, j, "r1", "attempt:tool:c1", agent.Record{Kind: agent.StepAttempt, ToolUseID: "c1", AttemptedAt: 1}); err != nil || !won {
		t.Fatal(won, err)
	}
	if ok, err := b.AcquireLease(ctx, "r1", "worker-b", time.Minute); err != nil || !ok {
		t.Fatal(ok, err)
	}
	ref := agent.HaltRef{RunID: "r1", Op: agent.OpRef{Kind: agent.OpTool, ID: "c1"}, Cause: agent.HaltCrashed}
	err = agent.ResolveHaltRef(ctx, j, ref, agent.Outcome{Result: "charged"})
	if _, ok := errors.AsType[*agent.HaltInFlight](err); !ok {
		t.Fatalf("resolving while another handle leases the run = %v; want *HaltInFlight", err)
	}
	if err := b.ReleaseLease(ctx, "r1", "worker-b"); err != nil {
		t.Fatal(err)
	}
	if err := agent.ResolveHaltRef(ctx, j, ref, agent.Outcome{Result: "charged"}); err != nil {
		t.Fatalf("resolving once the lease is free = %v; want nil, with no WithMinHaltAge", err)
	}
	if rec, ok, err := j.Get(ctx, "r1", agent.ToolResultStep("c1")); err != nil || !ok || string(rec.Result) != `"charged"` {
		t.Fatalf("the recorded result = %s, %v, %v; want \"charged\"", rec.Result, ok, err)
	}
}
