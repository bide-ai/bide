package audit

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/bide-ai/bide/agent"
)

const (
	// DefaultAnchorTimeout is how long AuditedEventStore waits for one head to be built, signed and
	// published before it gives up on it (see AuditedEventStore.WithTimeout).
	DefaultAnchorTimeout = 3 * time.Second
	// DefaultAnchorCooldown is how long, after a failed publish, AuditedEventStore skips publishing
	// before it probes the anchor again (see AuditedEventStore.WithCooldown).
	DefaultAnchorCooldown = 30 * time.Second

	// pendingDrain is how many unanchored trails one AnchorTrails call retries besides the ones it
	// is given, so the recovery pass of one run is not charged for a whole backlog.
	pendingDrain = 16
	// anchorBudgetTimeouts bounds one AnchorTrails call to this many timeouts in all.
	anchorBudgetTimeouts = 10
)

// AuditedEventStore wraps an EventStore so every append is continuously anchored, as AuditedStore
// does for a journal: whenever a trail grows it commits an RFC 6962 Merkle root over the trail's
// leaves, signs a Signed Tree Head, and publishes it to an Anchor. The head is the one
// SignNotifyTrail signs (kind events, RunID the trail's name), with the same Signer and the same
// Anchor interface as the journal's, so an HSM or KMS signer and an external transparency log work
// unchanged. Wrap the store a notifier's trail lives in (NewNotifyTrail(NewAuditedEventStore(...)))
// and the notification trail is signed and anchored as it is written; bide audit notifications then
// verifies it against the heads. Any other event trail can be wrapped the same way.
//
// Anchoring is a SIDE CHANNEL, as for AuditedStore: a publish failure NEVER fails the append. The
// leaf is stored before anything is signed, and failing the append because a head could not be
// published would have a caller retry a write that already succeeded. Failures go to the OnError
// hook; the trail remains the source of truth, and Reanchor, ReanchorPending and AnchorTrails
// publish a head that a failure left out.
//
// An anchor that is slow or down must not slow the writer, so anchoring is bounded twice. One
// attempt (reading the trail, signing, publishing) is abandoned after the timeout (default
// DefaultAnchorTimeout), even if the Anchor or the Signer ignores its context, so a hung anchor
// holds a write for at most that long. And after a failed or timed-out publish the store skips
// publishing for the cooldown (default DefaultAnchorCooldown) and then lets a single write probe
// the anchor, so an outage costs one timeout per cooldown, not one per write. Skipped trails are
// remembered (Unanchored) and published once the anchor answers.
//
// Anchoring is serialized per trail: a trail's head is built, signed and published while the
// trail's slot is held, from the trail as it is then stored, and only if it is longer than the
// last head this store published. So the heads one AuditedEventStore publishes for a trail only
// grow, which a monitor reads as the trail never being rolled back. Two processes anchoring one
// trail (the notifier runs in several) can still deliver a smaller head after a larger one; both
// commit to prefixes of one trail, so a monitor should compare heads by size, not arrival. A store
// that restarts publishes the current head of a trail again the first time it anchors it.
//
// It keeps a few bytes per trail it has anchored, for the life of the store. It is safe for
// concurrent use; configure it (WithClock, OnError, WithTimeout, WithCooldown) before the first
// write.
type AuditedEventStore struct {
	inner    EventStore
	signer   Signer
	anchor   Anchor
	now      func() int64
	onErr    func(trail string, err error)
	timeout  time.Duration
	cooldown time.Duration

	mu       sync.Mutex
	trails   map[string]*anchoredTrail
	failedAt time.Time // when a publish last failed; zero while the anchor is believed healthy
	probing  bool      // a write is probing the anchor after the cooldown
}

// anchoredTrail is the per-trail state of an AuditedEventStore.
type anchoredTrail struct {
	slot  chan struct{} // capacity 1; held while a head is built, signed and published
	size  int           // the size of the last head published; read and written holding slot
	dirty bool          // a write or an attempt left the trail possibly unanchored; guarded by mu
	epoch uint64        // counts the times dirty was set, so a publish clears only what it covered
}

var _ EventStore = (*AuditedEventStore)(nil)

// NewAuditedEventStore wraps inner so each growth of a trail is signed by signer (ed25519, ECDSA,
// ML-DSA-65, hybrid or an HSM or KMS signer) and published to anchor.
//
// It refuses a nil store or anchor, and a signer without a usable key, with an error wrapping
// agent.ErrConfig: the append may not fail once the inner store has stored the leaf, so a signer
// that could sign nothing is refused here, before any write.
func NewAuditedEventStore(inner EventStore, signer Signer, anchor Anchor) (*AuditedEventStore, error) {
	if inner == nil || anchor == nil {
		return nil, fmt.Errorf("audit: NewAuditedEventStore: a nil store or anchor: %w", agent.ErrConfig)
	}
	if err := checkSigner(signer); err != nil {
		return nil, fmt.Errorf("audit: NewAuditedEventStore: %w", err)
	}
	return &AuditedEventStore{
		inner: inner, signer: signer, anchor: anchor,
		now:     func() int64 { return time.Now().UnixNano() },
		timeout: DefaultAnchorTimeout, cooldown: DefaultAnchorCooldown,
		trails: map[string]*anchoredTrail{},
	}, nil
}

// WithClock sets a deterministic timestamp source for the heads (for tests). It returns the store.
func (a *AuditedEventStore) WithClock(now func() int64) *AuditedEventStore { a.now = now; return a }

// OnError sets a hook called when anchoring a write fails: a head could not be built, signed or
// published, an attempt timed out, or the write's context was done. Without it the errors are
// dropped; the write has succeeded all the same. The hook may be called from several goroutines.
// It returns the store.
func (a *AuditedEventStore) OnError(fn func(trail string, err error)) *AuditedEventStore {
	a.onErr = fn
	return a
}

// WithTimeout sets how long one anchoring attempt may take before it is abandoned (default
// DefaultAnchorTimeout). The attempt includes reading the trail, signing, and publishing, and a
// write waits for it at most this long, whatever the Anchor does with its context. A d that is not
// positive leaves the default. It returns the store.
func (a *AuditedEventStore) WithTimeout(d time.Duration) *AuditedEventStore {
	if d > 0 {
		a.timeout = d
	}
	return a
}

// WithCooldown sets how long, after a failed publish, writes skip publishing before one probes the
// anchor again (default DefaultAnchorCooldown). Zero turns the pause off: every write tries. A
// negative d leaves the default. It returns the store.
func (a *AuditedEventStore) WithCooldown(d time.Duration) *AuditedEventStore {
	if d >= 0 {
		a.cooldown = d
	}
	return a
}

// Unwrap returns the wrapped store, so a decorator above this one, and NotifyTrail, find it and
// the stores beneath it. Reads and writes pass run IDs and positions through unchanged.
func (a *AuditedEventStore) Unwrap() EventStore { return a.inner }

// Load reads through to the inner store.
func (a *AuditedEventStore) Load(ctx context.Context, trail string) ([][]byte, error) {
	return a.inner.Load(ctx, trail)
}

// Append stores leaf in the inner store, then publishes a head over the trail if it is longer than
// the last head published. The anchoring never changes what Append returns. A position taken by a
// different leaf (ErrSeqTaken) stored nothing of this caller's and publishes nothing. An append
// that failed otherwise may have committed all the same, so the trail is remembered as possibly
// unanchored (Unanchored) without touching the anchor. Under a context that is done nothing can be
// read or published: the write is reported to OnError and remembered, the signal to call
// ReanchorPending (a notifier's sweep and recovery pass do) once the anchor is reachable.
func (a *AuditedEventStore) Append(ctx context.Context, trail string, seq int, leaf []byte) error {
	err := a.inner.Append(ctx, trail, seq, leaf)
	switch {
	case errors.Is(err, ErrSeqTaken):
		return err
	case err != nil:
		a.markDirty(trail)
		return err
	}
	if cerr := ctx.Err(); cerr != nil {
		a.markDirty(trail)
		a.report(trail, fmt.Errorf("audit: trail %s: position %d not anchored: context done: %w", trail, seq, cerr))
		return nil
	}
	if aerr := a.anchorTrail(ctx, trail, false); aerr != nil {
		a.report(trail, aerr)
	}
	return nil
}

// Reanchor publishes a head over trail now if it holds records no head this store published
// covers. It tries the anchor even during the cooldown, once, within the timeout, and returns the
// error that stopped it; it publishes nothing when the last head is current as far as this store
// knows (see the type's note on restarts).
func (a *AuditedEventStore) Reanchor(ctx context.Context, trail string) error {
	return a.anchorTrail(ctx, trail, true)
}

// ReanchorPending reanchors every trail that a failure, a skipped attempt or a done context left
// possibly unanchored, one after the other, and stops at the first error. Each attempt is bounded
// by the timeout; the whole call is not, so give ctx a deadline.
func (a *AuditedEventStore) ReanchorPending(ctx context.Context) error {
	for _, trail := range a.Unanchored() {
		if err := a.anchorTrail(ctx, trail, true); err != nil {
			return err
		}
	}
	return nil
}

// Unanchored returns, sorted, the trails that may hold records no published head covers: those
// whose last attempt failed, timed out or was skipped. A trail whose attempt was never made (a
// process that died) is not here; the notifier's sweep and recovery pass anchor those (AnchorTrails).
func (a *AuditedEventStore) Unanchored() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for name, t := range a.trails {
		if t.dirty {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// AnchorTrails is the best-effort catch-up the notifier calls on its sweep and recovery pass
// (agent.NotifyTrailAnchorer): it anchors the named trails, and up to a few more that earlier
// attempts left unanchored, unless the anchor is in its cooldown, when it only remembers them. It
// stops at the first error, which it returns, and spends at most ten timeouts in all, remembering
// the trails it did not reach. Unlike Reanchor it respects the cooldown, so a sweep over many
// trails does not wait out a timeout for each while the anchor is down.
func (a *AuditedEventStore) AnchorTrails(ctx context.Context, trails []string) error {
	names := slices.Compact(slices.Sorted(slices.Values(trails)))
	pending := a.Unanchored()
	pending = slices.DeleteFunc(pending, func(n string) bool { _, dup := slices.BinarySearch(names, n); return dup })
	names = append(names, pending[:min(len(pending), pendingDrain)]...)

	if a.coolingDown() {
		for _, n := range names {
			a.markDirty(n)
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, anchorBudgetTimeouts*a.timeout)
	defer cancel()
	for i, n := range names {
		if ctx.Err() != nil {
			for _, rest := range names[i:] {
				a.markDirty(rest)
			}
			return nil
		}
		if err := a.anchorTrail(ctx, n, false); err != nil {
			return err
		}
	}
	return nil
}

func (a *AuditedEventStore) report(trail string, err error) {
	if a.onErr != nil {
		a.onErr(trail, err)
	}
}

func (a *AuditedEventStore) trailOf(name string) *anchoredTrail {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.trails[name]
	if !ok {
		t = &anchoredTrail{slot: make(chan struct{}, 1)}
		a.trails[name] = t
	}
	return t
}

// markDirty remembers that trail may hold records no head covers.
func (a *AuditedEventStore) markDirty(name string) {
	t := a.trailOf(name)
	a.mu.Lock()
	t.dirty = true
	t.epoch++
	a.mu.Unlock()
}

// epochOf is the trail's dirty epoch now: what a publish that reads the trail next covers.
func (a *AuditedEventStore) epochOf(t *anchoredTrail) uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return t.epoch
}

// clean forgets that the trail is unanchored if nothing was remembered since epoch.
func (a *AuditedEventStore) clean(t *anchoredTrail, epoch uint64) {
	a.mu.Lock()
	if t.epoch == epoch {
		t.dirty = false
	}
	a.mu.Unlock()
}

// coolingDown reports whether publishing is paused: a publish failed less than a cooldown ago, or
// a write is already probing the anchor.
func (a *AuditedEventStore) coolingDown() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cooldown > 0 && !a.failedAt.IsZero() && (a.probing || time.Since(a.failedAt) < a.cooldown)
}

// admit says whether a publish may go ahead. While the anchor is believed healthy it always may;
// after a failure it may not until the cooldown has passed, and then only one at a time (the probe).
// A forced publish always may.
func (a *AuditedEventStore) admit(force bool) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if force || a.cooldown <= 0 || a.failedAt.IsZero() {
		return true
	}
	if a.probing || time.Since(a.failedAt) < a.cooldown {
		return false
	}
	a.probing = true
	return true
}

// settle records the outcome of a publish that admit allowed.
func (a *AuditedEventStore) settle(ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.probing = false
	if ok {
		a.failedAt = time.Time{}
	} else {
		a.failedAt = time.Now()
	}
}

// release ends a probe without a verdict on the anchor.
func (a *AuditedEventStore) release() {
	a.mu.Lock()
	a.probing = false
	a.mu.Unlock()
}

// anchorTrail publishes a head over trail if it grew past the last one. It holds the trail's slot
// for the whole of building, signing and publishing, on a goroutine it abandons at the timeout, so
// a head that hangs holds up neither this call nor (after the cooldown starts) any other, while
// the trail's slot stays held until it ends, which keeps the heads of a trail in order.
func (a *AuditedEventStore) anchorTrail(ctx context.Context, trail string, force bool) error {
	if !force && a.coolingDown() {
		a.markDirty(trail) // no waiting on a trail's slot, nor a read, for an anchor that is down
		return nil
	}
	t := a.trailOf(trail)
	tctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	select {
	case t.slot <- struct{}{}:
	case <-tctx.Done():
		a.markDirty(trail)
		return fmt.Errorf("audit: trail %s: an earlier head is still being published: %w", trail, tctx.Err())
	}
	res := make(chan error, 1)
	go func() {
		defer func() { <-t.slot }()
		res <- a.publishGrown(tctx, trail, t, force)
	}()
	select {
	case err := <-res:
		return err
	case <-tctx.Done():
	}
	select {
	case err := <-res: // finished as the deadline passed
		return err
	default:
	}
	a.markDirty(trail)
	if ctx.Err() == nil { // the timeout, not the caller giving up: the anchor is not answering
		a.settle(false)
	}
	return fmt.Errorf("audit: trail %s: publishing its head did not finish in %v: %w", trail, a.timeout, tctx.Err())
}

// publishGrown runs holding the trail's slot.
func (a *AuditedEventStore) publishGrown(ctx context.Context, trail string, t *anchoredTrail, force bool) error {
	epoch := a.epochOf(t)
	log, err := LoadEventLog(ctx, a.inner, trail)
	if err != nil {
		a.markDirty(trail)
		return err
	}
	size := len(log.leaves)
	if size <= t.size {
		a.clean(t, epoch)
		return nil
	}
	if !a.admit(force) {
		a.markDirty(trail)
		return nil
	}
	sth, err := SignTreeHead(log.TreeHead(trail, a.now()), a.signer)
	if err == nil {
		err = a.anchor.Publish(ctx, trail, sth)
	}
	if err != nil {
		if ctx.Err() != nil {
			a.release() // the caller gave up or timed out: that says nothing about the anchor
		} else {
			a.settle(false)
		}
		a.markDirty(trail)
		return fmt.Errorf("audit: trail %s: publish head of size %d: %w", trail, size, err)
	}
	a.settle(true)
	t.size = size
	a.clean(t, epoch)
	return nil
}
