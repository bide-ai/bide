package audit_test

import (
	"context"
	"crypto/ed25519"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// slowAnchor is a remote anchor with latency: the publish of tree size hold stays in flight until
// a larger tree head has been published (or a short timeout passes), then lands.
type slowAnchor struct {
	inner    *audit.MemAnchorLog
	hold     int
	inFlight chan struct{} // closed once the held publish has started
	release  chan struct{} // closed once a larger tree head has been published
	once     sync.Once
}

func (a *slowAnchor) Publish(ctx context.Context, runID string, sth audit.SignedTreeHead) error {
	switch {
	case sth.Size == a.hold:
		close(a.inFlight)
		select {
		case <-a.release:
		case <-time.After(300 * time.Millisecond):
		}
	case sth.Size > a.hold:
		defer a.once.Do(func() { close(a.release) })
	}
	return a.inner.Publish(ctx, runID, sth)
}

// A run's anchored tree heads must never shrink: an anchor log in which a run's committed size goes
// down reads, to any monitor, as the journal being rolled back, which is exactly what anchoring
// exists to expose. Two steps of one run land concurrently (parallel tool calls), and the anchor
// is slow to accept the first.
func TestAuditedStore_AnchoredHeadsNeverShrink(t *testing.T) {
	ctx := context.Background()
	_, priv, _ := ed25519.GenerateKey(nil)
	anchor := &slowAnchor{inner: audit.NewMemAnchorLog(), hold: 1, inFlight: make(chan struct{}), release: make(chan struct{})}
	store := audit.NewAuditedStore(agent.NewMemStore(), priv, anchor)
	step := func(name string) {
		if _, err := store.Do(ctx, "r1", name, func(context.Context) (agent.Record, error) {
			return agent.Record{Kind: agent.StepValue}, nil
		}); err != nil {
			t.Error(err)
		}
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); step("a") }() // its size-1 head is slow to anchor
	select {
	case <-anchor.inFlight:
	case <-time.After(2 * time.Second):
		t.Fatal("the first step never started anchoring")
	}
	step("b") // lands while the first head is still in flight
	wg.Wait()

	var sizes []int
	for _, e := range anchor.inner.Entries() {
		if e.RunID == "r1" {
			sizes = append(sizes, e.STH.Size)
		}
	}
	for i := 1; i < len(sizes); i++ {
		if sizes[i] < sizes[i-1] {
			t.Fatalf("run r1's anchored tree sizes, in anchor order, are %v: a later head is smaller than an earlier one", sizes)
		}
	}
	if len(sizes) == 0 || sizes[len(sizes)-1] != 2 {
		t.Fatalf("run r1's anchored tree sizes are %v, want the last to cover both records (2)", sizes)
	}
}

// flakyAnchor fails its first publish, then accepts.
type flakyAnchor struct {
	inner *audit.MemAnchorLog
	calls int
}

func (a *flakyAnchor) Publish(ctx context.Context, runID string, sth audit.SignedTreeHead) error {
	a.calls++
	if a.calls == 1 {
		return context.DeadlineExceeded // the remote anchor timed out
	}
	return a.inner.Publish(ctx, runID, sth)
}

// A head whose publish failed is not treated as anchored: the run's next step retries it, even a
// memoized replay that does not grow the journal, so every record ends up covered by an anchored
// head.
func TestAuditedStore_RetriesAFailedPublish(t *testing.T) {
	ctx := context.Background()
	_, priv, _ := ed25519.GenerateKey(nil)
	anchor := &flakyAnchor{inner: audit.NewMemAnchorLog()}
	var failures int
	store := audit.NewAuditedStore(agent.NewMemStore(), priv, anchor).OnError(func(string, error) { failures++ })
	step := func() {
		if _, err := store.Do(ctx, "r1", "a", func(context.Context) (agent.Record, error) {
			return agent.Record{Kind: agent.StepValue}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	step() // records "a"; anchoring fails
	if failures != 1 || anchor.inner.Len() != 0 {
		t.Fatalf("after the failed publish: %d failures, %d anchored heads; want 1 and 0", failures, anchor.inner.Len())
	}
	step() // a memoized replay of "a": nothing new to write, but the head is still unanchored
	if n := anchor.inner.Len(); n != 1 {
		t.Fatalf("after the replay, %d anchored heads, want 1 (the failed head retried)", n)
	}
}
