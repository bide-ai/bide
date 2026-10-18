package awss3_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/anchor/awss3"
	"github.com/bide-ai/bide/anchor/awss3/awss3test"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/journaltest"
)

func TestAuditedStoreAnchorsToS3AndTheHeadMatchesTheJournal(t *testing.T) {
	k := newKeys(t, 1)
	f := awss3test.New(bucket)
	a := newAnchor(t, f, nil)
	inner := agent.NewMemStore()
	store, err := audit.NewAuditedStore(inner, k.signer, a)
	if err != nil {
		t.Fatal(err)
	}
	j := agenttest.MustJournal(store)
	for _, name := range []string{"a", "b", "c"} {
		if _, err := journaltest.Do(ctx, j, "run-1", name, func(context.Context) (agent.Record, error) {
			return agent.Record{Kind: agent.StepValue}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	head, err := newReader(t, f, k.v).Latest(ctx, audit.TreeJournal, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	// The anchored head is the head of the journal as stored: recompute it from the inner store.
	th, err := audit.NewTreeHead(ctx, agenttest.MustJournal(inner), "run-1", head.TimestampNanos)
	if err != nil || th.Size != head.Size || !bytes.Equal(th.Root, head.Root) {
		t.Fatalf("anchored head (size %d) does not match the journal (size %d): %v", head.Size, th.Size, err)
	}
	heads, err := newReader(t, f, k.v).Heads(ctx, audit.TreeJournal, "run-1")
	if err != nil || len(heads) < 3 {
		t.Fatalf("Heads = %d, %v; want one per growth", len(heads), err)
	}
	for i := 1; i < len(heads); i++ {
		if heads[i].Size <= heads[i-1].Size {
			t.Errorf("heads are not in increasing size: %v", sizes(heads))
		}
	}
}

func TestAuditedStoreSurvivesAnS3OutageAndReanchors(t *testing.T) {
	k := newKeys(t, 1)
	f := awss3test.New(bucket)
	a := newAnchor(t, f, nil)
	store, err := audit.NewAuditedStore(agent.NewMemStore(), k.signer, a)
	if err != nil {
		t.Fatal(err)
	}
	var failed []error
	store.OnError(func(_ string, err error) { failed = append(failed, err) })
	f.Hook = func(op string, _ int) error {
		if op == "PutObject" {
			return errors.New("network unreachable")
		}
		return nil
	}
	j := agenttest.MustJournal(store)
	if _, err := journaltest.Do(ctx, j, "run-1", "a", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue}, nil
	}); err != nil {
		t.Fatalf("an anchor outage failed the write: %v", err)
	}
	if len(failed) == 0 || !errors.Is(failed[0], awss3.ErrUnavailable) || len(f.Keys()) != 0 {
		t.Fatalf("OnError got %v, keys %v; want ErrUnavailable and nothing anchored", failed, f.Keys())
	}
	f.Hook = nil
	if err := store.Reanchor(ctx, "run-1"); err != nil {
		t.Fatalf("Reanchor: %v", err)
	}
	if _, err := newReader(t, f, k.v).Latest(ctx, audit.TreeJournal, "run-1"); err != nil {
		t.Fatalf("after Reanchor: %v", err)
	}
}

func TestAuditedEventStoreAnchorsATrailToS3(t *testing.T) {
	k := newKeys(t, 1)
	f := awss3test.New(bucket)
	a := newAnchor(t, f, nil)
	store, err := audit.NewAuditedEventStore(audit.NewMemEventStore(), k.signer, a)
	if err != nil {
		t.Fatal(err)
	}
	store.OnError(func(trail string, err error) { t.Errorf("anchoring %s: %v", trail, err) })
	trail := audit.NewNotifyTrail(store)
	for seq, key := range []string{"k1", "k2"} {
		rec := agent.NotifyRecord{Kind: agent.NotifyObserved, Key: key, At: time.Now()}
		if err := trail.Append(ctx, "notify:@open:2026-10-17", seq, rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	head, err := newReader(t, f, k.v).Latest(ctx, audit.TreeEvents, "notify:@open:2026-10-17")
	if err != nil || head.Size != 2 {
		t.Fatalf("Latest = size %d, %v; want the trail's head of size 2", head.Size, err)
	}
	// The same ID as a journal is a different tree.
	if _, err := newReader(t, f, k.v).Latest(ctx, audit.TreeJournal, "notify:@open:2026-10-17"); !errors.Is(err, awss3.ErrNotFound) {
		t.Errorf("a trail's head was found as a journal's: %v", err)
	}
}
