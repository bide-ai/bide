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
// Publishing is off the write's path: Append returns when the inner store has stored the leaf, and
// a background publisher builds, signs and publishes the head a moment later (Flush waits for it).
// So a slow or hung anchor, signer or clock cannot lengthen a write, and a notifier's delivery
// rounds keep the timing the claim arithmetic of the design note assumes. The publisher is bounded
// in turn: one attempt (reading the trail, signing, publishing) is abandoned after the timeout
// (default DefaultAnchorTimeout), even if the Anchor or the Signer ignores its context, and after a
// failed or timed-out publish the store pauses publishing for the cooldown (default
// DefaultAnchorCooldown) and then lets a single attempt probe the anchor, so an outage costs one
// timeout per cooldown. Trails not published are remembered (Unanchored) and published by the next
// append to any trail after the cooldown, or by Reanchor, ReanchorPending or AnchorTrails.
// The window a head leaves open, from an append to its head, is therefore the publisher's delay
// when the anchor is healthy, and the outage plus the time to the next sweep when it is not. A
// process that dies in that window leaves its last records unattested until another process's
// sweep or recovery pass signs the trail again, and bide audit notifications reports them.
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

	minInterval time.Duration // WithMinInterval; 0 anchors every growth
	wall        func() time.Time
	after       func(time.Duration, func()) (stop func() bool)

	mu       sync.Mutex
	trails   map[string]*anchoredTrail
	failedAt time.Time     // when a publish last failed; zero while the anchor is believed healthy
	probing  bool          // a write is probing the anchor after the cooldown
	running  bool          // the background publisher is running
	idle     chan struct{} // closed when the running publisher exits
}

// anchoredTrail is the per-trail state of an AuditedEventStore.
type anchoredTrail struct {
	slot     chan struct{} // capacity 1; held while a head is built, signed and published
	size     int           // the size of the last head published; read and written holding slot
	dirty    bool          // a write or an attempt left the trail possibly unanchored; guarded by mu
	epoch    uint64        // counts the times dirty was set, so a publish clears only what it covered
	appended uint64        // counts the appends that grew the trail, so the publisher knows what it has not seen
	lastPub  time.Time     // when a head of the trail was last published; guarded by mu
	hold     func() bool   // stops the timer that publishes growth WithMinInterval held back; nil when none is armed; guarded by mu
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
		wall:   time.Now,
		after:  func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop },
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

// WithMinInterval limits how often a trail's head is published: after one head, growth of the same
// trail is held back until d has passed, and then published as one head that covers all of it.
// Without it (the default, or d not positive) every append that the background publisher sees
// publishes a head, which for an anchor that keeps each head for years is one retained object per
// append at worst.
//
// What stays immediate: a trail's first head, Reanchor, ReanchorPending, AnchorTrails (the
// notifier's sweep and recovery pass) and Flush. What is bounded: while this process lives, a record
// is covered by a published head at most d (plus one publish) after it was appended, because held-back
// growth is published by a timer, not only by a later append. A process that dies loses the timer:
// records appended in the last d stay unanchored until another process anchors the trail, so d is also
// how much a crash can leave uncovered. The cooldown after a failed publish still applies.
// It returns the store; configure it before the first write.
func (a *AuditedEventStore) WithMinInterval(d time.Duration) *AuditedEventStore {
	a.minInterval = max(d, 0)
	return a
}

// holdBack reports whether trail's growth must wait for the minimum interval, and arms the timer that
// publishes it when it does.
func (a *AuditedEventStore) holdBack(trail string) bool {
	t := a.trailOf(trail)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.minInterval <= 0 || t.lastPub.IsZero() {
		return false
	}
	wait := a.minInterval - a.wall().Sub(t.lastPub)
	if wait <= 0 {
		return false
	}
	if t.hold == nil {
		t.hold = a.after(wait, func() { a.fire(trail, t) })
	}
	return true
}

// fire publishes the growth held back for trail: the timer's work. It respects the cooldown.
func (a *AuditedEventStore) fire(trail string, t *anchoredTrail) {
	a.mu.Lock()
	t.hold = nil
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), a.timeout)
	defer cancel()
	if err := a.anchorTrailOpts(ctx, trail, false, true); err != nil {
		a.report(trail, err)
	}
}

// published records that a head of the trail was just published and disarms its timer.
func (a *AuditedEventStore) published(t *anchoredTrail) {
	a.mu.Lock()
	t.lastPub = a.wall()
	stop := t.hold
	t.hold = nil
	a.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// Unwrap returns the wrapped store, so a decorator above this one, and NotifyTrail, find it and
// the stores beneath it. Reads and writes pass run IDs and positions through unchanged.
func (a *AuditedEventStore) Unwrap() EventStore { return a.inner }

// Load reads through to the inner store.
func (a *AuditedEventStore) Load(ctx context.Context, trail string) ([][]byte, error) {
	return a.inner.Load(ctx, trail)
}

// Append stores leaf in the inner store and returns its result, then has a background publisher
// publish a head over the trail if it is longer than the last head published. The head is built,
// signed and published off the append's path, so no anchor, signer or clock problem can slow or
// fail a write, or lengthen a notifier's delivery round: the claim arithmetic of the notifier
// (docs/design/notifier.md) counts the store's round trips as short, and this keeps it true. A
// burst of appends to a trail is covered by one head. Flush waits for the publisher.
//
// A position taken by a different leaf (ErrSeqTaken) stored nothing of this caller's and publishes
// nothing. An append that failed otherwise may have committed all the same, so the trail is
// remembered as possibly unanchored (Unanchored) and tried again with the next write, Reanchor,
// ReanchorPending or AnchorTrails.
func (a *AuditedEventStore) Append(ctx context.Context, trail string, seq int, leaf []byte) error {
	err := a.inner.Append(ctx, trail, seq, leaf)
	switch {
	case errors.Is(err, ErrSeqTaken):
		return err
	case err != nil:
		a.markDirty(trail)
		return err
	}
	t := a.trailOf(trail)
	a.mu.Lock()
	t.appended++
	a.mu.Unlock()
	a.markDirty(trail)
	a.kick()
	return nil
}

// kick starts the background publisher if it is not running.
func (a *AuditedEventStore) kick() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		return
	}
	a.running, a.idle = true, make(chan struct{})
	go a.publisher(a.idle)
}

// publisher publishes a head for every trail appended to and not yet seen, one at a time, then
// exits; the next append starts it again. A trail it fails to publish stays remembered
// (Unanchored) and is tried by the next kick, so a down anchor is not hammered by a loop.
func (a *AuditedEventStore) publisher(idle chan struct{}) {
	seen := map[string]uint64{} // the appends each trail had when the publisher last tried it
	for {
		name, appended, ok := a.nextAppended(seen)
		if !ok {
			return // nextAppended cleared running under the lock
		}
		seen[name] = appended
		if err := a.anchorTrail(context.Background(), name, false); err != nil {
			a.report(name, err)
		}
	}
}

// nextAppended returns a trail with appends the publisher has not seen, the smallest name first.
// When there is none it marks the publisher stopped, under the same lock a kick takes, so an append
// racing with the exit starts a new publisher.
func (a *AuditedEventStore) nextAppended(seen map[string]uint64) (name string, appended uint64, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for n, t := range a.trails {
		if t.dirty && t.appended != seen[n] && (!ok || n < name) {
			name, appended, ok = n, t.appended, true
		}
	}
	if !ok {
		a.running = false
		close(a.idle)
	}
	return name, appended, ok
}

// Flush waits until the background publisher has tried every trail appended to before the call (or
// ctx is done, whose error it returns). It does not retry what failed: use ReanchorPending. Call
// it before a process exits, so the last records are covered; whatever a process leaves behind is
// covered by the next sweep or recovery pass. Agent.FlushNotifications does not wait for this
// publisher: it signs the notify trails itself (AnchorTrails) and reports a failure to its own
// error handler, so a caller that needs the OnError reports of the appends it made calls Flush.
//
// With WithMinInterval it also publishes, now, the growth that was held back, and returns the first
// error of those (it still tries the others).
func (a *AuditedEventStore) Flush(ctx context.Context) error {
	for {
		a.mu.Lock()
		running, idle := a.running, a.idle
		a.mu.Unlock()
		if !running {
			break
		}
		select {
		case <-idle:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	var first error
	for _, name := range a.heldBack() {
		if err := a.anchorTrailOpts(ctx, name, false, true); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// heldBack returns, sorted, the trails whose growth WithMinInterval is holding back.
func (a *AuditedEventStore) heldBack() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for name, t := range a.trails {
		if t.hold != nil {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
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
	for _, n := range a.Unanchored() {
		if _, dup := slices.BinarySearch(names, n); !dup {
			names = append(names, n)
		}
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
		if err := a.anchorTrailOpts(ctx, n, false, true); err != nil {
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
	return a.anchorTrailOpts(ctx, trail, force, force)
}

// anchorTrailOpts is anchorTrail with the minimum interval separate from the cooldown: force skips
// the cooldown (and the interval), skipInterval only the interval.
func (a *AuditedEventStore) anchorTrailOpts(ctx context.Context, trail string, force, skipInterval bool) error {
	if !force && a.coolingDown() {
		a.markDirty(trail) // no waiting on a trail's slot, nor a read, for an anchor that is down
		return nil
	}
	if !skipInterval && a.holdBack(trail) {
		return nil // the trail stays dirty, and the timer publishes it
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
		// The slot is released before the answer is sent: a caller that has its answer may publish
		// the next head at once, and must not find the slot still held. A panic or an exit of the
		// goroutine releases it too.
		release := sync.OnceFunc(func() { <-t.slot })
		defer release()
		err := a.publishGrown(tctx, trail, t, force)
		release()
		res <- err
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
	a.published(t)
	return nil
}
