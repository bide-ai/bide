package audit

import (
	"context"
	"fmt"
	"iter"
	"maps"
	"slices"
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

	minInterval time.Duration    // WithMinInterval; 0 anchors every growth
	wall        func() time.Time // the clock the interval is measured on
	after       func(time.Duration, func()) (stop func() bool)
	lastPub     map[string]time.Time   // when a run's head was last published
	pending     map[string]func() bool // runs whose growth is held back, and the timer that publishes it
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
		wall:     time.Now,
		after:    func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop },
		lastPub:  map[string]time.Time{},
		pending:  map[string]func() bool{},
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

// WithMinInterval limits how often a run's head is published: after one head, growth of the same run
// is held back until d has passed, and then published as one head that covers all of it. Without it
// (the default, or d not positive) every journal write publishes a head, which for an anchor that
// keeps each head for years (an S3 Object Lock bucket, say) is one retained object per write.
//
// What stays immediate: a run's first head, the write of its end marker (run:complete or
// run:aborted), Reanchor, and Flush. What is bounded: while this process lives, a record is covered
// by a published head at most d (plus one publish) after it was written, because held-back growth is
// published by a timer, not only by a later write. A process that dies loses the timer: records
// written in the last d, and not yet anchored, stay unanchored until another process anchors the run
// (Reanchor), so d is also how much a crash can leave uncovered. Heads of one run still only grow.
// Call Flush before the process exits. It returns the store; configure it before the first write.
func (a *AuditedStore) WithMinInterval(d time.Duration) *AuditedStore {
	a.minInterval = max(d, 0)
	return a
}

// Flush publishes now every run whose growth WithMinInterval held back, stopping their timers, and
// returns the first error (it still tries the others). Call it before the process exits, so the last
// records are covered. It publishes nothing for a run whose head is current.
func (a *AuditedStore) Flush(ctx context.Context) error {
	a.mu.Lock()
	runs := make([]string, 0, len(a.pending))
	for r := range a.pending {
		runs = append(runs, r)
	}
	a.mu.Unlock()
	slices.Sort(runs)
	var first error
	for _, r := range runs {
		if err := a.anchorIfGrown(ctx, r, true); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Pending returns, sorted, the runs whose growth WithMinInterval is holding back.
func (a *AuditedStore) Pending() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Sorted(maps.Keys(a.pending))
}

// holdBack reports whether runID's growth must wait for the minimum interval, and arms the timer
// that publishes it when it does.
func (a *AuditedStore) holdBack(runID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.minInterval <= 0 {
		return false
	}
	last, ok := a.lastPub[runID]
	if !ok {
		return false
	}
	wait := a.minInterval - a.wall().Sub(last)
	if wait <= 0 {
		return false
	}
	if _, armed := a.pending[runID]; !armed {
		a.pending[runID] = a.after(wait, func() { a.fire(runID) })
	}
	return true
}

// fire publishes the growth held back for runID: the timer's work.
func (a *AuditedStore) fire(runID string) {
	a.mu.Lock()
	delete(a.pending, runID) // disarmed: a failed publish must not leave the run looking armed
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), heldBackTimeout)
	defer cancel()
	if err := a.anchorIfGrown(ctx, runID, true); err != nil && a.onErr != nil {
		a.onErr(runID, err)
	}
}

// heldBackTimeout bounds one publish of held-back growth, which no caller's context covers.
const heldBackTimeout = 30 * time.Second

// published records that runID's head was just published and disarms its timer.
func (a *AuditedStore) published(runID string) {
	a.mu.Lock()
	a.lastPub[runID] = a.wall()
	stop := a.pending[runID]
	delete(a.pending, runID)
	a.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// journalHeader is the name of a run's journal header, its first entry (see agent.StepHeader).
const journalHeader = "@journal"

// The journal entries that end a run (agent.runCompleteStep and runAbortedStep): the write of either
// is anchored at once, whatever WithMinInterval says.
const (
	runCompleteName = "run:complete"
	runAbortedName  = "run:aborted"
)

// Insert stores the entry in the inner store, then anchors the run's tree head if the journal grew
// past the last anchored head. It anchors after every insert but the journal header's (a journal
// writes the header just before the run's first record, whose insert anchors both, so a run's
// anchored heads never stop at its header): also after an insert that found the entry stored
// (another writer's, or this writer's retry of a write whose first attempt committed but
// reported an error, A3), and after one that failed, which may have committed all the same. The
// anchoring never changes what Insert returns.
//
// Under a context that is done it cannot anchor (the read and the publish would fail with the
// context), and anchoring without the caller's context could block with no deadline. If the call
// stored the entry, or failed (it may have committed all the same), it reports an error wrapping
// the context's to OnError, the signal to Reanchor the run: a run's last write, run:complete
// included, may be written by a drive whose context is cancelled meanwhile. An insert that stored
// nothing (the entry was already there) has nothing to anchor and reports nothing.
func (a *AuditedStore) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	e, inserted, err := a.inner.Insert(ctx, runID, name, data)
	if name == journalHeader {
		return e, inserted, err
	}
	if cerr := ctx.Err(); cerr != nil {
		if (inserted || err != nil) && a.onErr != nil {
			a.onErr(runID, fmt.Errorf("audit: run %s: %q not anchored: context done: %w", runID, name, cerr))
		}
		return e, inserted, err
	}
	if aerr := a.anchorIfGrown(ctx, runID, name == runCompleteName || name == runAbortedName); aerr != nil && a.onErr != nil {
		a.onErr(runID, aerr)
	}
	return e, inserted, err
}

// protocol:identity begin GateAnchorStep

// Reanchor anchors runID's journal now, if it holds records no published head covers. A failed
// publish is covered by the run's next write, but a run's last write has none, and a write under
// a context that is done is not anchored at all (see Insert): OnError is the signal to call
// Reanchor for that run (once the anchor is reachable again), out of band. It
// returns the anchoring error, and publishes nothing when the anchored head is current as far as
// this AuditedStore knows: it remembers the heads it published, so another AuditedStore over the
// same store (another process, or after a restart) publishes the current head again, which a
// monitor reads as the same size, not a change.
func (a *AuditedStore) Reanchor(ctx context.Context, runID string) error {
	return a.anchorIfGrown(ctx, runID, true)
}

// protocol:identity end

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

// protocol:identity begin Anchor

// anchorIfGrown publishes a fresh STH iff runID's journal is longer than the last anchored size,
// so each write that grows the journal anchors once.
//
// Anchoring is serialized per run: the journal is read and its head published under the run's
// lock, so a run's anchored heads only ever grow. Without it, two writes landing at once (parallel
// tool calls) could publish their heads in the wrong order, and the anchor log would show the
// run's committed size going down, which reads to a monitor as the journal being rolled back.
// The anchored size advances only when a publish succeeds, so a failed publish is retried by the
// run's next write. Different runs anchor independently.
//
// With WithMinInterval, growth within the interval of the run's last head is held back (a timer
// publishes it) unless force is set: the run's end, Reanchor and Flush force.
func (a *AuditedStore) anchorIfGrown(ctx context.Context, runID string, force bool) error {
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
	if !force && a.holdBack(runID) {
		return nil
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
	a.published(runID)
	return nil
}

// protocol:identity end

var _ agent.Store = (*AuditedStore)(nil)
