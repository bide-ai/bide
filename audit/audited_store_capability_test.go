package audit_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"

	"github.com/bide-ai/bide/internal/journaltest"
)

func newAudited(t *testing.T, inner *agent.Journal) *audit.AuditedStore {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return mustAuditedStore(t, inner, priv, audit.NewMemAnchorLog())
}

// Wrapping a leasing store in AuditedStore keeps its lease: a run another holder leases on the
// inner store is not driven through the wrapper.
func TestAuditedStore_LeaseHonorsInnerLease(t *testing.T) {
	ctx := context.Background()
	inner := agent.NewMemStore()
	j := agenttest.MustJournal(inner)
	if got, err := inner.AcquireLease(ctx, "r", "other", time.Minute); err != nil || !got {
		t.Fatalf("AcquireLease = %v, %v", got, err)
	}
	driven, err := agent.Lease(ctx, agenttest.MustJournal(newAudited(t, j)), "r", func(context.Context) error {
		t.Error("drive ran while another holder held the lease")
		return nil
	})
	if err != nil || driven {
		t.Fatalf("Lease through AuditedStore = (%v, %v), want (false, nil)", driven, err)
	}
}

// Recover and RecoverLoop enumerate runs through the wrapper's inner Lister.
func TestAuditedStore_RecoverListsInnerRuns(t *testing.T) {
	ctx := context.Background()
	inner := agenttest.MemJournal()
	store := agenttest.MustJournal(newAudited(t, inner))
	if _, err := journaltest.Do(ctx, store, "r1", "run:start", func(context.Context) (agent.Record, error) { // a run recovery drives has a run:start
		return agent.Record{Kind: agent.StepValue, Result: []byte(`{"input":"x"}`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	var resumed []string
	n, err := agent.Recover(ctx, store, func(_ context.Context, runID string, _ agent.RunStart) error {
		resumed = append(resumed, runID)
		return nil
	})
	if err != nil || n != 1 || !slices.Equal(resumed, []string{"r1"}) {
		t.Fatalf("Recover through AuditedStore = (%d, %v), resumed %v; want (1, nil), [r1]", n, err, resumed)
	}
}

// bareStore implements agent.Store and nothing else: no Lister, no Leaser, no Unwrap.
type bareStore struct{ agent.Store }

// The wrapper adds no capability its inner store lacks: over a store with no Lister, Recover
// still reports ErrConfig, and over a store with no Leaser, Lease drives unconditionally.
func TestAuditedStore_AddsNoCapabilities(t *testing.T) {
	ctx := context.Background()
	store := agenttest.MustJournal(newAudited(t, agenttest.MustJournal(bareStore{agent.NewMemStore()})))
	if _, err := agent.Recover(ctx, store, func(context.Context, string, agent.RunStart) error { return nil }); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("Recover over a non-Lister = %v, want ErrConfig", err)
	}
	driven, err := agent.Lease(ctx, store, "r", func(context.Context) error { return nil })
	if err != nil || !driven {
		t.Fatalf("Lease over a non-Leaser = (%v, %v), want (true, nil)", driven, err)
	}
}

// RecoverLoop, too, enumerates through the wrapper and drives the inner store's runs.
func TestAuditedStore_RecoverLoopListsInnerRuns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := agenttest.MustJournal(newAudited(t, agenttest.MemJournal()))
	if _, err := journaltest.Do(ctx, store, "r1", "run:start", func(context.Context) (agent.Record, error) { // a run recovery drives has a run:start
		return agent.Record{Kind: agent.StepValue, Result: []byte(`{"input":"x"}`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	resumed := make(chan string, 16)
	done := make(chan error, 1)
	go func() {
		done <- agent.RecoverLoop(ctx, store, func(_ context.Context, runID string, _ agent.RunStart) error {
			resumed <- runID
			return nil
		}, agent.WithRecoverInterval(10*time.Millisecond))
	}()
	select {
	case id := <-resumed:
		if id != "r1" {
			t.Fatalf("RecoverLoop resumed %q, want r1", id)
		}
	case err := <-done:
		t.Fatalf("RecoverLoop through AuditedStore returned %v before resuming r1", err)
	case <-time.After(5 * time.Second):
		t.Fatal("RecoverLoop through AuditedStore never resumed r1")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("RecoverLoop returned %v after cancel, want context.Canceled", err)
	}
}
