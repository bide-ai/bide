package audit

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"sync"
	"time"

	"github.com/bide-ai/bide/agent"
)

// AuditedStore wraps a Durable so every durable step is continuously anchored: whenever a
// run's journal grows, it commits an RFC 6962 Merkle root over the run, signs a Signed Tree
// Head, and publishes it to an Anchor (an external transparency log). The caller writes the
// agent loop exactly as before — audit.NewAuditedStore(store, priv, anchor) is a drop-in
// Durable — and gets a stream of signed, out-of-band commitments for free.
//
// Anchoring is a SIDE CHANNEL: a publish failure NEVER fails the durable step. Failing Do
// because an STH could not be published would be dangerous — the underlying write already
// succeeded, and the caller could wrongly retry a non-idempotent step. Publish errors go to
// the optional OnError hook instead; the journal remains the source of truth.
type AuditedStore struct {
	inner  agent.Durable
	priv   ed25519.PrivateKey
	anchor Anchor
	now    func() int64
	onErr  func(runID string, err error)

	mu       sync.Mutex
	lastSize map[string]int         // per-run journal length already anchored (dedup on replay)
	runLocks map[string]*sync.Mutex // serializes anchoring within a run (see anchorIfGrown)
}

// NewAuditedStore wraps inner so each journal growth is signed with priv and published to
// anchor. Timestamps default to time.Now().UnixNano(); override with WithClock for tests.
//
// It panics if priv is not ed25519.PrivateKeySize bytes. Such a key could sign nothing, and the
// store may not fail a step once the inner store has recorded it, so the key is refused here,
// before any step, rather than when the first step is anchored.
func NewAuditedStore(inner agent.Durable, priv ed25519.PrivateKey, anchor Anchor) *AuditedStore {
	if err := checkPrivateKey(priv); err != nil {
		panic(fmt.Sprintf("audit: NewAuditedStore: %v", err))
	}
	return &AuditedStore{
		inner:    inner,
		priv:     priv,
		anchor:   anchor,
		now:      func() int64 { return time.Now().UnixNano() },
		lastSize: map[string]int{},
		runLocks: map[string]*sync.Mutex{},
	}
}

// WithClock sets a deterministic timestamp source (for tests). Returns the store for chaining.
func (a *AuditedStore) WithClock(now func() int64) *AuditedStore { a.now = now; return a }

// OnError sets a hook called when anchoring a step fails (Publish, or committing the root).
// Without it, anchor errors are swallowed — the durable step still succeeds. Returns the store.
func (a *AuditedStore) OnError(fn func(runID string, err error)) *AuditedStore {
	a.onErr = fn
	return a
}

// Do runs the step on the inner store, then (if the journal grew) anchors the new tree head.
func (a *AuditedStore) Do(ctx context.Context, runID, name string, fn func(context.Context) (agent.Record, error)) (agent.Record, error) {
	rec, err := a.inner.Do(ctx, runID, name, fn)
	if err != nil {
		return rec, err // step failed / nothing recorded — nothing to anchor
	}
	if aerr := a.anchorIfGrown(ctx, runID); aerr != nil && a.onErr != nil {
		a.onErr(runID, aerr)
	}
	return rec, nil
}

// Unwrap returns the wrapped store, so agent.Capability finds its optional capabilities
// (agent.Leaser, agent.Lister) through the wrapper: an AuditedStore has exactly the capabilities of
// the store it wraps. Leasing and listing touch no journal, so they bypass anchoring.
func (a *AuditedStore) Unwrap() agent.Durable { return a.inner }

// History delegates unchanged.
func (a *AuditedStore) History(ctx context.Context, runID string) ([]agent.Record, error) {
	return a.inner.History(ctx, runID)
}

// anchorIfGrown publishes a fresh STH iff runID's journal is longer than the last anchored
// size — so a memoized replay (no growth) does not re-anchor, and each real step anchors once.
//
// Anchoring is serialized per run: the journal is read and its head published under the run's
// lock, so a run's anchored heads only ever grow. Without it, two steps landing at once (parallel
// tool calls) could publish their heads in the wrong order, and the anchor log would show the
// run's committed size going down, which reads to a monitor as the journal being rolled back.
// The anchored size advances only when a publish succeeds, so a failed publish is retried by the
// run's next step. Different runs anchor independently.
func (a *AuditedStore) anchorIfGrown(ctx context.Context, runID string) error {
	a.mu.Lock()
	rl, ok := a.runLocks[runID]
	if !ok {
		rl = &sync.Mutex{}
		a.runLocks[runID] = rl
	}
	a.mu.Unlock()
	rl.Lock()
	defer rl.Unlock()

	recs, err := a.inner.History(ctx, runID)
	if err != nil {
		return err
	}
	a.mu.Lock()
	grown := len(recs) > a.lastSize[runID]
	a.mu.Unlock()
	if !grown {
		return nil
	}
	th, err := journalHead(runID, recs, a.now())
	if err != nil {
		return err
	}
	if err := a.anchor.Publish(ctx, runID, SignTreeHead(th, a.priv)); err != nil {
		return err
	}
	a.mu.Lock()
	a.lastSize[runID] = len(recs)
	a.mu.Unlock()
	return nil
}

var _ agent.Durable = (*AuditedStore)(nil)
