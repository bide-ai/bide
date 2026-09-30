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
	"github.com/bide-ai/bide/audit"
)

func newAudited(t *testing.T, inner agent.Durable) *audit.AuditedStore {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return audit.NewAuditedStore(inner, priv, audit.NewMemAnchorLog())
}

// Wrapping a leasing store in AuditedStore keeps its lease: a run another holder leases on the
// inner store is not driven through the wrapper.
func TestAuditedStore_LeaseHonorsInnerLease(t *testing.T) {
	ctx := context.Background()
	inner := agent.NewMemStore()
	if got, err := inner.AcquireLease(ctx, "r", "other", time.Minute); err != nil || !got {
		t.Fatalf("AcquireLease = %v, %v", got, err)
	}
	driven, err := agent.Lease(ctx, newAudited(t, inner), "r", func(context.Context) error {
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
	inner := agent.NewMemStore()
	store := newAudited(t, inner)
	if _, err := store.Do(ctx, "r1", "s", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue}, nil
	}); err != nil {
		t.Fatal(err)
	}
	var resumed []string
	n, err := agent.Recover(ctx, store, func(_ context.Context, runID string) error {
		resumed = append(resumed, runID)
		return nil
	})
	if err != nil || n != 1 || !slices.Equal(resumed, []string{"r1"}) {
		t.Fatalf("Recover through AuditedStore = (%d, %v), resumed %v; want (1, nil), [r1]", n, err, resumed)
	}
}

// bareStore implements Durable and nothing else.
type bareStore struct{ agent.Durable }

// The wrapper adds no capability its inner store lacks: over a store with no Lister, Recover
// still reports ErrConfig, and over a store with no Leaser, Lease drives unconditionally.
func TestAuditedStore_AddsNoCapabilities(t *testing.T) {
	ctx := context.Background()
	store := newAudited(t, bareStore{agent.NewMemStore()})
	if _, err := agent.Recover(ctx, store, func(context.Context, string) error { return nil }); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("Recover over a non-Lister = %v, want ErrConfig", err)
	}
	driven, err := agent.Lease(ctx, store, "r", func(context.Context) error { return nil })
	if err != nil || !driven {
		t.Fatalf("Lease over a non-Leaser = (%v, %v), want (true, nil)", driven, err)
	}
}
