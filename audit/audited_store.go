package audit

import (
	"context"
	"fmt"
	"iter"
	"sync"
	"time"

	"github.com/bide-ai/bide/agent"
)

// AuditedStore wraps a store so every journal write is continuously anchored: whenever a run's
// journal grows, it commits an RFC 6962 Merkle root over the run, signs a Signed Tree Head, and
// publishes it to an Anchor (an external transparency log). The caller drives the agent over a
// Journal on it exactly as on the store it wraps (agent.NewJournal(audit.NewAuditedStore(store,
// signer, anchor))) and gets a stream of signed, out-of-band commitments for free.
//
// Anchoring is a SIDE CHANNEL: a publish failure NEVER fails the write. Failing an Insert because
// an STH could not be published would be dangerous: the write already succeeded, and the caller
// could wrongly retry a non-idempotent step. Publish errors go to the optional OnError hook
// instead; the journal remains the source of truth.
type AuditedStore struct {
	inner  agent.Store
	j      *agent.Journal // reads runs back to anchor them
	signer Signer
	anchor Anchor
	now    func() int64
	onErr  func(runID string, err error)

	mu       sync.Mutex
	lastSize map[string]int         // per-run journal length already anchored
	runLocks map[string]*sync.Mutex // serializes anchoring within a run (see anchorIfGrown)
}

// NewAuditedStore wraps inner so each journal growth is signed by signer (ed25519, ML-DSA-65, or
// hybrid) and published to anchor. Timestamps default to time.Now().UnixNano(); override with
// WithClock for tests.
//
// It refuses a nil store or anchor, and a signer without a usable key, with an error wrapping
// agent.ErrConfig. Such a signer could sign nothing, and the store may not fail a write once the
// inner store has stored it, so the signer is refused here, before any write, rather than when the
// first write is anchored.
func NewAuditedStore(inner agent.Store, signer Signer, anchor Anchor) (*AuditedStore, error) {
	if inner == nil || anchor == nil {
		return nil, fmt.Errorf("audit: NewAuditedStore: a nil store or anchor: %w", agent.ErrConfig)
	}
	if err := checkSigner(signer); err != nil {
		return nil, fmt.Errorf("audit: NewAuditedStore: %w", err)
	}
	j, err := agent.NewJournal(inner)
	if err != nil {
		return nil, fmt.Errorf("audit: NewAuditedStore: %w", err)
	}
	return &AuditedStore{
		inner:    inner,
		j:        j,
		signer:   signer,
		anchor:   anchor,
		now:      func() int64 { return time.Now().UnixNano() },
		lastSize: map[string]int{},
		runLocks: map[string]*sync.Mutex{},
	}, nil
}

// WithClock sets a deterministic timestamp source (for tests). Returns the store for chaining.
func (a *AuditedStore) WithClock(now func() int64) *AuditedStore { a.now = now; return a }

// OnError sets a hook called when anchoring a write fails (Publish, or committing the root).
// Without it, anchor errors are swallowed: the write still succeeds. Returns the store.
func (a *AuditedStore) OnError(fn func(runID string, err error)) *AuditedStore {
	a.onErr = fn
	return a
}

// journalHeader is the name of a run's journal header, its first entry (see agent.StepHeader).
const journalHeader = "@journal"

// Insert stores the entry in the inner store, then anchors the run's tree head if the journal grew
// past the last anchored head. It anchors after every insert but the journal header's (a journal
// writes the header just before the run's first record, whose insert anchors both, so a run's
// anchored heads never stop at its header): also after an insert that found the entry stored
// (another writer's, or this writer's retry of a write whose first attempt committed but
// reported an error, A3), and after one that failed, which may have committed all the same. The
// anchoring never changes what Insert returns.
func (a *AuditedStore) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	e, inserted, err := a.inner.Insert(ctx, runID, name, data)
	if name != journalHeader {
		if aerr := a.anchorIfGrown(ctx, runID); aerr != nil && a.onErr != nil {
			a.onErr(runID, aerr)
		}
	}
	return e, inserted, err
}

// Reanchor anchors runID's journal now, if it holds records no published head covers. A failed
// publish is covered by the run's next write, but a run's last write has none: OnError is the
// signal to call Reanchor for that run (once the anchor is reachable again), out of band. It
// returns the anchoring error, and publishes nothing when the anchored head is current.
func (a *AuditedStore) Reanchor(ctx context.Context, runID string) error {
	return a.anchorIfGrown(ctx, runID)
}

// Get reads through to the inner store.
func (a *AuditedStore) Get(ctx context.Context, runID, name string) (agent.Entry, bool, error) {
	return a.inner.Get(ctx, runID, name)
}

// Load reads through to the inner store.
func (a *AuditedStore) Load(ctx context.Context, runID string, after int64) iter.Seq2[agent.Entry, error] {
	return a.inner.Load(ctx, runID, after)
}

// Unwrap returns the wrapped store, so agent.Capability finds its optional capabilities
// (agent.Leaser, agent.Lister) through the wrapper: an AuditedStore has exactly the capabilities of
// the store it wraps. Leasing and listing touch no journal, so they bypass anchoring. It passes run
// IDs and names through unchanged, as Unwrap requires.
func (a *AuditedStore) Unwrap() agent.Store { return a.inner }

// anchorIfGrown publishes a fresh STH iff runID's journal is longer than the last anchored size,
// so each write that grows the journal anchors once.
//
// Anchoring is serialized per run: the journal is read and its head published under the run's
// lock, so a run's anchored heads only ever grow. Without it, two writes landing at once (parallel
// tool calls) could publish their heads in the wrong order, and the anchor log would show the
// run's committed size going down, which reads to a monitor as the journal being rolled back.
// The anchored size advances only when a publish succeeds, so a failed publish is retried by the
// run's next write. Different runs anchor independently.
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

	recs, err := a.j.History(ctx, runID)
	if err != nil {
		return err
	}
	a.mu.Lock()
	grown := len(recs) > a.lastSize[runID]
	a.mu.Unlock()
	if !grown || len(recs) == 1 && recs[0].Kind == agent.StepHeader {
		return nil // nothing new, or the header alone (its run's first record failed)
	}
	th, err := journalHead(runID, recs, a.now())
	if err != nil {
		return err
	}
	sth, err := SignTreeHead(th, a.signer)
	if err != nil {
		return err
	}
	if err := a.anchor.Publish(ctx, runID, sth); err != nil {
		return err
	}
	a.mu.Lock()
	a.lastSize[runID] = len(recs)
	a.mu.Unlock()
	return nil
}

var _ agent.Store = (*AuditedStore)(nil)
