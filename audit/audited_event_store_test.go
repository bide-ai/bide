package audit_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/audit/eventstoretest"
)

// trailAnchor is an Anchor that fails while down is set, blocks (ignoring its context, as a hung
// network call does) while hang is set, and records every head it accepts in arrival order.
type trailAnchor struct {
	mu       sync.Mutex
	down     bool
	hang     chan struct{} // when non-nil, Publish waits for it to close and ignores ctx
	attempts int
	heads    []audit.SignedTreeHead
	delay    time.Duration
}

func (f *trailAnchor) Publish(_ context.Context, runID string, sth audit.SignedTreeHead) error {
	f.mu.Lock()
	f.attempts++
	down, hang, delay := f.down, f.hang, f.delay
	f.mu.Unlock()
	if hang != nil {
		<-hang
	}
	time.Sleep(delay)
	if down {
		return errors.New("anchor unreachable")
	}
	if sth.RunID != runID {
		return errors.New("wrong run")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heads = append(f.heads, sth)
	return nil
}

func (f *trailAnchor) set(down bool) { f.mu.Lock(); f.down = down; f.mu.Unlock() }

func (f *trailAnchor) tried() int { f.mu.Lock(); defer f.mu.Unlock(); return f.attempts }

func (f *trailAnchor) sizes(trail string) []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []int
	for _, h := range f.heads {
		if h.RunID == trail {
			out = append(out, h.Size)
		}
	}
	return out
}

type errLog struct {
	mu   sync.Mutex
	errs []error
}

func (e *errLog) on(_ string, err error) { e.mu.Lock(); e.errs = append(e.errs, err); e.mu.Unlock() }

func (e *errLog) n() int { e.mu.Lock(); defer e.mu.Unlock(); return len(e.errs) }

type rig struct {
	s    *audit.AuditedEventStore
	mem  *audit.MemEventStore
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
	errs *errLog
}

func newRig(t *testing.T, anchor audit.Anchor) *rig {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	mem := audit.NewMemEventStore()
	el := &errLog{}
	s, err := audit.NewAuditedEventStore(mem, audit.Ed25519Signer{Priv: priv}, anchor)
	if err != nil {
		t.Fatal(err)
	}
	return &rig{s: s.OnError(el.on), mem: mem, pub: pub, priv: priv, errs: el}
}

// appendNotify appends n records to trail from position from, through the real adapter.
func appendNotify(t *testing.T, tr agent.NotifyTrail, trail string, from, n int) {
	t.Helper()
	for i := from; i < from+n; i++ {
		if err := tr.Append(context.Background(), trail, i, agent.NotifyRecord{Kind: agent.NotifyClaimed, Key: fmt.Sprintf("k%d", i), RunID: "r1", At: t0.Add(time.Duration(i) * time.Second), Holder: "p1"}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
}

// flush waits for the store's background publisher.
func (r *rig) flush(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAuditedEventStore_RefusesBadConfiguration(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	good := audit.Ed25519Signer{Priv: priv}
	for name, c := range map[string]func() (*audit.AuditedEventStore, error){
		"nil store": func() (*audit.AuditedEventStore, error) {
			return audit.NewAuditedEventStore(nil, good, audit.NewMemAnchorLog())
		},
		"nil anchor": func() (*audit.AuditedEventStore, error) {
			return audit.NewAuditedEventStore(audit.NewMemEventStore(), good, nil)
		},
		"no key": func() (*audit.AuditedEventStore, error) {
			return audit.NewAuditedEventStore(audit.NewMemEventStore(), audit.Ed25519Signer{}, audit.NewMemAnchorLog())
		},
		"nil signer": func() (*audit.AuditedEventStore, error) {
			return audit.NewAuditedEventStore(audit.NewMemEventStore(), nil, audit.NewMemAnchorLog())
		},
	} {
		if s, err := c(); !errors.Is(err, agent.ErrConfig) || s != nil {
			t.Errorf("%s: %v, %v, want ErrConfig", name, s, err)
		}
	}
}

func TestAuditedEventStore_KeepsTheEventStoreContract(t *testing.T) {
	r := newRig(t, audit.NewMemAnchorLog())
	eventstoretest.Run(t, func(*testing.T) audit.EventStore {
		s, err := audit.NewAuditedEventStore(r.mem, audit.Ed25519Signer{Priv: r.priv}, audit.NewMemAnchorLog())
		if err != nil {
			t.Fatal(err)
		}
		return s // every handle reaches the same trails, as the suite requires
	})
}

func TestAuditedEventStore_UnwrapReturnsTheInnerStore(t *testing.T) {
	r := newRig(t, audit.NewMemAnchorLog())
	if r.s.Unwrap() != audit.EventStore(r.mem) {
		t.Fatal("Unwrap is not the wrapped store")
	}
}

// Every append that grows a trail publishes a head over exactly the trail's leaves, in the format
// SignNotifyTrail produces and the verifier reads: kind events, RunID the trail's name.
func TestAuditedEventStore_AnchorsEachAppend(t *testing.T) {
	ctx := context.Background()
	log := audit.NewMemAnchorLog()
	r := newRig(t, log)
	r.s.WithClock(func() int64 { return t0.UnixNano() })
	tr := audit.NewNotifyTrail(r.s)

	for i := range 3 {
		appendNotify(t, tr, trailName, i, 1)
		r.flush(t) // one head per append when the publisher keeps up
	}
	appendNotify(t, tr, "notify:r2", 0, 1)
	r.flush(t)

	var sizes []int
	for _, e := range log.Entries() {
		if e.STH.RunID == trailName {
			sizes = append(sizes, e.STH.Size)
		}
		if e.STH.Kind != audit.TreeEvents || e.RunID != e.STH.RunID {
			t.Fatalf("entry %+v is not an event-trail head of its run", e)
		}
		if err := e.STH.Verify(audit.Ed25519Verifier{Pub: r.pub}); err != nil {
			t.Fatalf("head of %s size %d does not verify: %v", e.RunID, e.STH.Size, err)
		}
	}
	if !slices.Equal(sizes, []int{1, 2, 3}) {
		t.Fatalf("heads of %s have sizes %v, want 1 2 3", trailName, sizes)
	}
	if log.Len() != 4 || r.errs.n() != 0 {
		t.Fatalf("%d entries (want 4: three for r1, one for r2), %d errors", log.Len(), r.errs.n())
	}

	// The newest head is the very head SignNotifyTrail signs over the same leaves.
	want, err := audit.SignNotifyTrail(ctx, r.mem, trailName, audit.Ed25519Signer{Priv: r.priv}, t0)
	if err != nil {
		t.Fatal(err)
	}
	var got audit.SignedTreeHead
	for _, e := range log.Entries() {
		if e.STH.RunID == trailName {
			got = e.STH
		}
	}
	if got.Size != want.Size || got.Kind != want.Kind || got.RunID != want.RunID || !bytes.Equal(got.Root, want.Root) {
		t.Fatalf("auto-anchored head %+v differs from SignNotifyTrail's %+v", got.TreeHead, want.TreeHead)
	}
	if rest := r.s.Unanchored(); len(rest) != 0 {
		t.Fatalf("Unanchored = %v after every append anchored", rest)
	}
}

// An anchor error never fails the write; it is reported, the trail is remembered as unanchored,
// and a later Reanchor publishes the head once the anchor is back, and then publishes nothing.
func TestAuditedEventStore_AnchorFailureNeverFailsAnAppendAndReanchorRecovers(t *testing.T) {
	ctx := context.Background()
	fa := &trailAnchor{down: true}
	r := newRig(t, fa)
	r.s.WithCooldown(0)
	tr := audit.NewNotifyTrail(r.s)

	appendNotify(t, tr, trailName, 0, 3) // fatal on any append error
	r.flush(t)
	if got, err := tr.Load(ctx, trailName); err != nil || len(got) != 3 {
		t.Fatalf("trail = %d records, %v", len(got), err)
	}
	if r.errs.n() == 0 || len(fa.sizes(trailName)) != 0 {
		t.Fatalf("%d anchor errors (want some), heads %v", r.errs.n(), fa.sizes(trailName))
	}
	if !slices.Equal(r.s.Unanchored(), []string{trailName}) {
		t.Fatalf("Unanchored = %v", r.s.Unanchored())
	}
	if err := r.s.Reanchor(ctx, trailName); err == nil {
		t.Fatal("Reanchor reported no error while the anchor is down")
	}

	fa.set(false)
	if err := r.s.Reanchor(ctx, trailName); err != nil {
		t.Fatal(err)
	}
	if got := fa.sizes(trailName); !slices.Equal(got, []int{3}) {
		t.Fatalf("heads after the outage = %v, want one of size 3", got)
	}
	before := fa.tried()
	if err := r.s.Reanchor(ctx, trailName); err != nil || fa.tried() != before {
		t.Fatalf("a current head was published again: %v, %d attempts", err, fa.tried()-before)
	}
	if len(r.s.Unanchored()) != 0 {
		t.Fatalf("Unanchored = %v after Reanchor", r.s.Unanchored())
	}
}

// ReanchorPending re-signs every trail an outage left behind, including ones no run touches again.
func TestAuditedEventStore_ReanchorPendingCoversEveryFailedTrail(t *testing.T) {
	fa := &trailAnchor{down: true}
	r := newRig(t, fa)
	r.s.WithCooldown(0)
	tr := audit.NewNotifyTrail(r.s)
	appendNotify(t, tr, "notify:a", 0, 2)
	appendNotify(t, tr, "notify:b", 0, 1)
	r.flush(t)
	fa.set(false)

	if got := r.s.Unanchored(); !slices.Equal(got, []string{"notify:a", "notify:b"}) {
		t.Fatalf("Unanchored = %v", got)
	}
	if err := r.s.ReanchorPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fa.sizes("notify:a"), []int{2}) || !slices.Equal(fa.sizes("notify:b"), []int{1}) || len(r.s.Unanchored()) != 0 {
		t.Fatalf("a %v, b %v, still unanchored %v", fa.sizes("notify:a"), fa.sizes("notify:b"), r.s.Unanchored())
	}
}

// During an outage only one write per cooldown pays for a publish attempt: the rest are skipped, so
// an anchor that is down does not add a timeout to every append. Afterwards one probe finds the
// anchor back and the trail is brought up to date.
func TestAuditedEventStore_CooldownBoundsAttemptsDuringAnOutage(t *testing.T) {
	fa := &trailAnchor{down: true}
	r := newRig(t, fa)
	r.s.WithCooldown(80 * time.Millisecond)
	tr := audit.NewNotifyTrail(r.s)

	appendNotify(t, tr, trailName, 0, 10)
	r.flush(t)
	if fa.tried() != 1 || r.errs.n() != 1 {
		t.Fatalf("%d publish attempts and %d errors in an outage of 10 appends, want 1 and 1", fa.tried(), r.errs.n())
	}
	if !slices.Equal(r.s.Unanchored(), []string{trailName}) {
		t.Fatalf("Unanchored = %v", r.s.Unanchored())
	}

	fa.set(false)
	time.Sleep(120 * time.Millisecond)
	appendNotify(t, tr, trailName, 10, 1)
	r.flush(t)
	if got := fa.sizes(trailName); !slices.Equal(got, []int{11}) {
		t.Fatalf("heads after the cooldown = %v, want one head of size 11", got)
	}
	if len(r.s.Unanchored()) != 0 {
		t.Fatalf("Unanchored = %v", r.s.Unanchored())
	}
}

// A Publish that never returns and ignores its context holds no write at all, and the publisher
// itself is released at the timeout, so a Flush returns and later writes are not waited on.
func TestAuditedEventStore_HungAnchorDoesNotStallWrites(t *testing.T) {
	hang := make(chan struct{})
	fa := &trailAnchor{hang: hang}
	r := newRig(t, fa)
	r.s.WithTimeout(100 * time.Millisecond).WithCooldown(time.Hour)
	tr := audit.NewNotifyTrail(r.s)

	start := time.Now()
	appendNotify(t, tr, trailName, 0, 21)
	if d := time.Since(start); d > 50*time.Millisecond*21 {
		t.Fatalf("21 writes took %v against a hung anchor", d)
	}
	r.flush(t)
	if r.errs.n() != 1 || !errors.Is(r.errs.errs[0], context.DeadlineExceeded) || fa.tried() != 1 {
		t.Fatalf("errors = %v after %d attempts, want one deadline error after one attempt", r.errs.errs, fa.tried())
	}

	// The anchor comes back: the abandoned publish finishes, and a Reanchor catches the trail up.
	close(hang)
	deadline := time.Now().Add(5 * time.Second)
	for len(fa.sizes(trailName)) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := r.s.Reanchor(context.Background(), trailName); err != nil {
		t.Fatal(err)
	}
	sizes := fa.sizes(trailName)
	if len(sizes) == 0 || sizes[len(sizes)-1] != 21 || !slices.IsSorted(sizes) {
		t.Fatalf("heads = %v, want growing sizes ending at 21", sizes)
	}
}

// The head is published in the background, so it does not depend on the writer's context: a write
// whose context is done is anchored all the same.
func TestAuditedEventStore_AnchoringDoesNotDependOnTheWritersContext(t *testing.T) {
	fa := &trailAnchor{}
	r := newRig(t, fa)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := audit.NewNotifyTrail(r.s).Append(ctx, trailName, 0, agent.NotifyRecord{Kind: agent.NotifyClaimed, Key: "k", RunID: "r1", At: t0, Holder: "p"}); err != nil {
		t.Fatal(err)
	}
	r.flush(t)
	if r.errs.n() != 0 || !slices.Equal(fa.sizes(trailName), []int{1}) {
		t.Fatalf("errors %v, heads %v", r.errs.errs, fa.sizes(trailName))
	}
}

// A slow anchor does not slow a write: the writer returns at once and the publisher catches up
// with one head for the burst.
func TestAuditedEventStore_ASlowAnchorDoesNotSlowWrites(t *testing.T) {
	fa := &trailAnchor{delay: 300 * time.Millisecond}
	r := newRig(t, fa)
	tr := audit.NewNotifyTrail(r.s)
	start := time.Now()
	appendNotify(t, tr, trailName, 0, 10)
	if d := time.Since(start); d > 250*time.Millisecond {
		t.Fatalf("10 writes took %v against an anchor that takes 300ms a head", d)
	}
	r.flush(t)
	sizes := fa.sizes(trailName)
	if len(sizes) == 0 || sizes[len(sizes)-1] != 10 || len(sizes) > 3 {
		t.Fatalf("heads %v: want the burst covered by a few heads ending at 10", sizes)
	}
}

// Writers racing for positions, each anchoring its own append, publish heads that only grow: the
// anchor never sees a trail's committed size go down.
func TestAuditedEventStore_HeadsOnlyGrowUnderConcurrentAppends(t *testing.T) {
	fa := &trailAnchor{delay: time.Millisecond}
	r := newRig(t, fa)
	tr := audit.NewNotifyTrail(r.s)
	const writers, perWriter = 8, 6
	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			for i := range perWriter {
				for {
					recs, err := tr.Load(context.Background(), trailName)
					if err != nil {
						t.Error(err)
						return
					}
					err = tr.Append(context.Background(), trailName, len(recs), agent.NotifyRecord{Kind: agent.NotifyClaimed, Key: fmt.Sprintf("w%d-%d", w, i), RunID: "r1", At: t0, Holder: "p"})
					if err == nil {
						break
					}
					if !errors.Is(err, agent.ErrTrailConflict) {
						t.Error(err)
						return
					}
				}
			}
		})
	}
	wg.Wait()
	r.flush(t)

	sizes := fa.sizes(trailName)
	if len(sizes) == 0 || sizes[len(sizes)-1] != writers*perWriter {
		t.Fatalf("heads %v do not end at the trail's size %d", sizes, writers*perWriter)
	}
	for i := 1; i < len(sizes); i++ {
		if sizes[i] <= sizes[i-1] {
			t.Fatalf("anchored sizes went %d then %d in arrival order: %v", sizes[i-1], sizes[i], sizes)
		}
	}
	if r.errs.n() != 0 {
		t.Fatalf("%d errors: %v", r.errs.n(), r.errs.errs)
	}
}

// A lost race (a different leaf at a stored position) stored nothing of the caller's, so it
// publishes nothing.
func TestAuditedEventStore_ALostRaceAnchorsNothing(t *testing.T) {
	fa := &trailAnchor{}
	r := newRig(t, fa)
	appendNotify(t, audit.NewNotifyTrail(r.s), trailName, 0, 1)
	r.flush(t)
	before := fa.tried()
	if before != 1 {
		t.Fatalf("%d attempts for the first append", before)
	}
	if err := r.s.Append(context.Background(), trailName, 0, []byte("other")); !errors.Is(err, audit.ErrSeqTaken) {
		t.Fatalf("different leaf = %v, want ErrSeqTaken", err)
	}
	r.flush(t)
	if fa.tried() != before || r.errs.n() != 0 {
		t.Fatalf("a refused append published a head (%d attempts, %d errors)", fa.tried()-before, r.errs.n())
	}
}

// wrapped is an EventStore decorator that hides the audited store beneath it behind Unwrap.
type wrapped struct{ audit.EventStore }

func (w wrapped) Unwrap() audit.EventStore { return w.EventStore }

// The notification trail finds the audited store beneath any decorator, and a trail over a store
// that anchors nothing has nothing to re-sign.
func TestNotifyTrail_AnchorTrailsFindsTheAuditedStoreThroughUnwrap(t *testing.T) {
	ctx := context.Background()
	fa := &trailAnchor{down: true}
	r := newRig(t, fa)
	r.s.WithCooldown(0)
	tr := audit.NewNotifyTrail(wrapped{wrapped{r.s}})
	appendNotify(t, tr, trailName, 0, 2)
	r.flush(t)
	fa.set(false)

	var anchorer agent.NotifyTrailAnchorer = tr
	if err := anchorer.AnchorTrails(ctx, []string{trailName}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fa.sizes(trailName), []int{2}) {
		t.Fatalf("heads = %v", fa.sizes(trailName))
	}

	plain := audit.NewNotifyTrail(audit.NewMemEventStore())
	if err := plain.AnchorTrails(ctx, []string{trailName}); err != nil {
		t.Fatalf("a trail with no anchoring store beneath = %v, want nil", err)
	}
}

// ---- through the notifier ----

type recSink struct {
	mu sync.Mutex
	n  int
}

func (s *recSink) Notify(context.Context, agent.Notification) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return nil
}

func (s *recSink) count() int { s.mu.Lock(); defer s.mu.Unlock(); return s.n }

// notifiedGate drives run to an approval gate with a notifier writing through trail.
func notifiedGate(t *testing.T, store agent.Store, run string, trail agent.NotifyTrail, sink agent.Notifier, opts ...agent.NotifyOption) {
	t.Helper()
	j, err := agent.NewJournal(store)
	if err != nil {
		t.Fatal(err)
	}
	tool := agent.MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithApproval(agent.SingleApproval()))
	a, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{}`)), j,
		agent.WithTools(tool), agent.WithNotifier(sink, trail, append([]agent.NotifyOption{agent.WithNotifyHolder("p1")}, opts...)...))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), run, agent.UserText("go")); !errors.As(err, new(*agent.ApprovalPending)) {
		t.Fatalf("Run = %v", err)
	}
	if err := a.FlushNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// A delivery is acknowledged and recorded although every anchor publish fails, and the notifier
// still reports nothing as undelivered: an anchor error is a side channel.
func TestNotifier_AnAnchorOutageNeverFailsADelivery(t *testing.T) {
	fa := &trailAnchor{down: true}
	r := newRig(t, fa)
	sink := &recSink{}
	var notifyErrs errLog
	notifiedGate(t, agent.NewMemStore(), "told", audit.NewNotifyTrail(r.s), sink, agent.WithNotifyErrors(func(err error) { notifyErrs.on("", err) }))

	if sink.count() != 1 {
		t.Fatalf("the sink was called %d times, want 1", sink.count())
	}
	recs, err := audit.NewNotifyTrail(r.mem).Load(context.Background(), "notify:told")
	if err != nil {
		t.Fatal(err)
	}
	var kinds []agent.NotifyRecordKind
	for _, rec := range recs {
		kinds = append(kinds, rec.Kind)
	}
	if !slices.Contains(kinds, agent.NotifyAcked) {
		t.Fatalf("trail %v holds no acknowledgement", kinds)
	}
	if r.errs.n() == 0 {
		t.Fatal("the outage was not reported to OnError")
	}
	for _, err := range notifyErrs.errs {
		if !strings.Contains(err.Error(), "anchor trails") {
			t.Fatalf("the notifier reported %v: only anchoring may fail in an anchor outage", err)
		}
	}
}

// NotifyPending re-signs a trail that grew past its last head, which is how the window after an
// outage closes without a new event.
func TestNotifyPending_ResignsAGrownTrailAfterAnOutage(t *testing.T) {
	ctx := context.Background()
	fa := &trailAnchor{down: true}
	r := newRig(t, fa)
	r.s.WithCooldown(0)
	store := agent.NewMemStore()
	trail := audit.NewNotifyTrail(r.s)
	notifiedGate(t, store, "told", trail, &recSink{})
	r.flush(t)
	if len(fa.sizes("notify:told")) != 0 {
		t.Fatal("a head was published while the anchor was down")
	}

	fa.set(false)
	j, err := agent.NewJournal(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.NotifyPending(ctx, j, &recSink{}, trail, agent.WithNotifyHolder("sweeper")); err != nil {
		t.Fatal(err)
	}
	sizes := fa.sizes("notify:told")
	log, _ := r.mem.Load(ctx, "notify:told")
	if len(sizes) != 1 || sizes[0] != len(log) || len(log) == 0 {
		t.Fatalf("heads after the sweep = %v, trail has %d records", sizes, len(log))
	}
	// The trails that belong to no run are signed again too: today's open segment and the index.
	for _, name := range []string{agent.OpenSegment(time.Now()), agent.OpenIndex(time.Now().UTC().Year())} {
		stored, _ := r.mem.Load(ctx, name)
		got := fa.sizes(name)
		if len(stored) == 0 || len(got) == 0 || got[len(got)-1] != len(stored) {
			t.Fatalf("%s: %d records stored, heads %v after the sweep", name, len(stored), got)
		}
	}
}

// A sweep over a hung anchor still returns: every anchoring it does is bounded by the timeout.
func TestNotifyPending_ABoundedSweepOverAHungAnchor(t *testing.T) {
	hang := make(chan struct{})
	defer close(hang)
	fa := &trailAnchor{}
	r := newRig(t, fa)
	r.s.WithTimeout(100 * time.Millisecond).WithCooldown(0)
	store := agent.NewMemStore()
	trail := audit.NewNotifyTrail(r.s)
	notifiedGate(t, store, "told", trail, &recSink{})
	j, err := agent.NewJournal(store)
	if err != nil {
		t.Fatal(err)
	}

	fa.mu.Lock()
	fa.hang = hang
	fa.mu.Unlock()
	// Grow the trail so the sweep has a head to sign; that append already pays one timeout.
	if err := trail.Append(context.Background(), "notify:told", mustLen(t, r.mem, "notify:told"), agent.NotifyRecord{Kind: agent.NotifyFailingSince, At: t0, Reason: "resume_error"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- agent.NotifyPending(context.Background(), j, &recSink{}, trail, agent.WithNotifyHolder("sweeper"))
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("NotifyPending did not return against a hung anchor")
	}
}

func mustLen(t *testing.T, es audit.EventStore, trail string) int {
	t.Helper()
	l, err := es.Load(context.Background(), trail)
	if err != nil {
		t.Fatal(err)
	}
	return len(l)
}

// AnchorTrails (the notifier's sweep and recovery pass) respects the cooldown, so a sweep over many
// trails does not wait out a timeout for each while the anchor is down; Reanchor does not.
func TestAuditedEventStore_AnchorTrailsRespectsTheCooldownAndReanchorDoesNot(t *testing.T) {
	ctx := context.Background()
	fa := &trailAnchor{down: true}
	r := newRig(t, fa)
	r.s.WithCooldown(time.Hour)
	appendNotify(t, audit.NewNotifyTrail(r.s), trailName, 0, 2)
	r.flush(t)
	fa.set(false)

	before := fa.tried()
	if err := r.s.AnchorTrails(ctx, []string{trailName}); err != nil || fa.tried() != before {
		t.Fatalf("AnchorTrails in the cooldown: %v, %d attempts", err, fa.tried()-before)
	}
	if !slices.Equal(r.s.Unanchored(), []string{trailName}) {
		t.Fatalf("Unanchored = %v", r.s.Unanchored())
	}
	if err := r.s.Reanchor(ctx, trailName); err != nil || !slices.Equal(fa.sizes(trailName), []int{2}) {
		t.Fatalf("Reanchor: %v, heads %v", err, fa.sizes(trailName))
	}
	// The anchor answered: the cooldown is over for everyone.
	appendNotify(t, audit.NewNotifyTrail(r.s), "notify:r2", 0, 1)
	if err := r.s.AnchorTrails(ctx, []string{"notify:r2"}); err != nil || !slices.Equal(fa.sizes("notify:r2"), []int{1}) {
		t.Fatalf("after recovery: %v, heads %v", err, fa.sizes("notify:r2"))
	}
}

// failingAppendStore commits an append and then reports an error when failNext is set, as a store
// does when its reply is lost.
type failingAppendStore struct {
	audit.EventStore
	mu       sync.Mutex
	failNext bool
}

func (f *failingAppendStore) Append(ctx context.Context, trail string, seq int, leaf []byte) error {
	err := f.EventStore.Append(ctx, trail, seq, leaf)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil && f.failNext {
		f.failNext = false
		return errors.New("reply lost")
	}
	return err
}

// An append that errors may have committed, so its trail is remembered as unanchored without
// touching the anchor, and the next Reanchor signs it.
func TestAuditedEventStore_AnAppendThatErroredLeavesItsTrailUnanchored(t *testing.T) {
	ctx := context.Background()
	_, priv, _ := ed25519.GenerateKey(nil)
	fa := &trailAnchor{}
	inner := &failingAppendStore{EventStore: audit.NewMemEventStore(), failNext: true}
	s, err := audit.NewAuditedEventStore(inner, audit.Ed25519Signer{Priv: priv}, fa)
	if err != nil {
		t.Fatal(err)
	}
	leafErr := s.Append(ctx, trailName, 0, []byte("x"))
	if leafErr == nil {
		t.Fatal("the lossy append reported no error")
	}
	if !slices.Equal(s.Unanchored(), []string{trailName}) || fa.tried() != 0 {
		t.Fatalf("Unanchored = %v after %d attempts: an errored append must be remembered without touching the anchor", s.Unanchored(), fa.tried())
	}
}

// While a head is being published, a write that errors marks the trail unanchored: the publish in
// flight did not cover it, so it must not clear that mark.
func TestAuditedEventStore_APublishDoesNotClearAMarkMadeDuringIt(t *testing.T) {
	ctx := context.Background()
	_, priv, _ := ed25519.GenerateKey(nil)
	gate := make(chan struct{})
	fa := &trailAnchor{hang: gate}
	inner := &failingAppendStore{EventStore: audit.NewMemEventStore()}
	s, err := audit.NewAuditedEventStore(inner, audit.Ed25519Signer{Priv: priv}, fa)
	if err != nil {
		t.Fatal(err)
	}
	s.WithTimeout(10 * time.Second)
	tr := audit.NewNotifyTrail(s)
	appendNotify(t, tr, trailName, 0, 1) // the publisher is now blocked inside Publish
	deadline := time.Now().Add(5 * time.Second)
	for fa.tried() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	inner.mu.Lock()
	inner.failNext = true
	inner.mu.Unlock()
	if err := s.Append(ctx, trailName, 1, []byte("lost-reply")); err == nil {
		t.Fatal("expected the lossy append to report an error")
	}
	close(gate)
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.Unanchored(), []string{trailName}) {
		t.Fatalf("Unanchored = %v: the publish in flight cleared a mark it did not cover", s.Unanchored())
	}
}

// After a cooldown, exactly one publisher probes a recovering anchor, however many trails reach
// the anchor at once.
func TestAuditedEventStore_OnlyOneProbeAfterTheCooldown(t *testing.T) {
	ctx := context.Background()
	fa := &trailAnchor{down: true, delay: 150 * time.Millisecond}
	r := newRig(t, fa)
	r.s.WithCooldown(40 * time.Millisecond)
	tr := audit.NewNotifyTrail(r.s)
	const trails = 8
	for i := range trails {
		appendNotify(t, tr, fmt.Sprintf("notify:t%d", i), 0, 1)
		r.flush(t) // the first fails and opens the breaker; the rest are skipped
	}
	time.Sleep(80 * time.Millisecond) // the cooldown has passed
	before := fa.tried()
	var wg sync.WaitGroup
	for i := range trails {
		wg.Go(func() { _ = r.s.AnchorTrails(ctx, []string{fmt.Sprintf("notify:t%d", i)}) })
	}
	wg.Wait()
	if got := fa.tried() - before; got != 1 {
		t.Fatalf("%d publish attempts reached a failing anchor after the cooldown, want one probe", got)
	}
}

// The background publisher and the re-sign path (AnchorTrails, Reanchor, ReanchorPending) race
// over several trails against a slow, jittery anchor: whatever the interleaving, the heads that
// arrive for a trail only grow, and each trail ends covered.
func TestAuditedEventStore_HeadsOnlyGrowWithThePublisherAndTheResignPathRacing(t *testing.T) {
	ctx := context.Background()
	fa := &trailAnchor{delay: 2 * time.Millisecond}
	r := newRig(t, fa)
	r.s.WithCooldown(0)
	tr := audit.NewNotifyTrail(r.s)
	trails := []string{"notify:a", "notify:b", "notify:c"}
	const perTrail = 25
	stop := make(chan struct{})
	var resign sync.WaitGroup
	for k := range 4 {
		resign.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				switch (i + k) % 3 {
				case 0:
					_ = r.s.AnchorTrails(ctx, trails)
				case 1:
					_ = r.s.Reanchor(ctx, trails[i%len(trails)])
				default:
					_ = r.s.ReanchorPending(ctx)
				}
			}
		})
	}
	var writers sync.WaitGroup
	for _, name := range trails {
		writers.Go(func() { appendNotify(t, tr, name, 0, perTrail) })
	}
	writers.Wait()
	r.flush(t)
	close(stop)
	resign.Wait()
	for _, name := range trails {
		if err := r.s.Reanchor(ctx, name); err != nil {
			t.Fatal(err)
		}
		sizes := fa.sizes(name)
		if len(sizes) == 0 || sizes[len(sizes)-1] != perTrail {
			t.Fatalf("%s: heads %v do not end at %d", name, sizes, perTrail)
		}
		for i := 1; i < len(sizes); i++ {
			if sizes[i] <= sizes[i-1] {
				t.Fatalf("%s: arrival order went %d then %d: %v", name, sizes[i-1], sizes[i], sizes)
			}
		}
	}
}

// countingStore counts Loads.
type countingStore struct {
	audit.EventStore
	mu    sync.Mutex
	loads int
}

func (c *countingStore) Load(ctx context.Context, trail string) ([][]byte, error) {
	c.mu.Lock()
	c.loads++
	c.mu.Unlock()
	return c.EventStore.Load(ctx, trail)
}

func (c *countingStore) n() int { c.mu.Lock(); defer c.mu.Unlock(); return c.loads }

// While the anchor is in its cooldown nothing reads a trail for it: neither the publisher nor the
// re-sign path pays a Load per skipped trail, so a sweep over many trails stays cheap in an outage.
func TestAuditedEventStore_TheCooldownSkipsBeforeReadingTheTrail(t *testing.T) {
	ctx := context.Background()
	_, priv, _ := ed25519.GenerateKey(nil)
	fa := &trailAnchor{down: true}
	cs := &countingStore{EventStore: audit.NewMemEventStore()}
	s, err := audit.NewAuditedEventStore(cs, audit.Ed25519Signer{Priv: priv}, fa)
	if err != nil {
		t.Fatal(err)
	}
	s.WithCooldown(time.Hour)
	tr := audit.NewNotifyTrail(s)
	appendNotify(t, tr, "notify:a", 0, 1)
	if err := s.Flush(ctx); err != nil { // the one attempt that fails and opens the breaker
		t.Fatal(err)
	}
	appendNotify(t, tr, "notify:b", 0, 1)
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	before := cs.n()
	names := make([]string, 50)
	for i := range names {
		names[i] = fmt.Sprintf("notify:x%d", i)
	}
	if err := s.AnchorTrails(ctx, names); err != nil {
		t.Fatal(err)
	}
	if got := cs.n() - before; got != 0 {
		t.Fatalf("%d Loads during the cooldown, want none", got)
	}
}

// A caller that gives up on a slow Reanchor says nothing about the anchor: the breaker stays closed
// and the next trail is published.
func TestAuditedEventStore_ACallerGivingUpDoesNotTripTheBreaker(t *testing.T) {
	fa := &trailAnchor{delay: 400 * time.Millisecond}
	r := newRig(t, fa)
	r.s.WithCooldown(time.Hour)
	tr := audit.NewNotifyTrail(r.s)
	appendNotify(t, tr, "notify:a", 0, 1)
	r.flush(t)
	appendNotify(t, audit.NewNotifyTrail(r.mem), "notify:a", 1, 1) // behind the wrapper's back: only Reanchor will see it
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := r.s.Reanchor(ctx, "notify:a"); err == nil {
		t.Fatal("Reanchor returned no error although its context ended mid-publish")
	}
	appendNotify(t, tr, "notify:b", 0, 1)
	r.flush(t)
	if got := fa.sizes("notify:b"); !slices.Equal(got, []int{1}) {
		t.Fatalf("notify:b heads = %v: the breaker opened because a caller gave up", got)
	}
}

// A process that starts fresh (it remembers no head, and no failure) signs the global trails again
// on its first sweep: the digest's, the open index, today's segment and the newest few before it,
// and not the older sealed ones.
func TestNotifyPending_AFreshProcessSignsTheGlobalTrails(t *testing.T) {
	ctx := context.Background()
	mem := audit.NewMemEventStore()
	plain := audit.NewNotifyTrail(mem)
	now := time.Now().UTC()
	idx := agent.OpenIndex(now.Year())
	var segs []string
	for i := 5; i >= 0; i-- {
		seg := agent.OpenSegment(now.AddDate(0, 0, -i))
		segs = append(segs, seg)
		if err := plain.Append(ctx, seg, 0, agent.NotifyRecord{Kind: agent.NotifySeal, At: now, Next: "x"}); err != nil {
			t.Fatal(err)
		}
		if err := plain.Append(ctx, idx, 5-i, agent.NotifyRecord{Kind: agent.NotifySegment, At: now, Next: seg}); err != nil {
			t.Fatal(err)
		}
	}
	if err := plain.Append(ctx, agent.NotifyDigestTrail, 0, agent.NotifyRecord{Kind: agent.NotifyClaimed, Key: "d", At: now, Holder: "p"}); err != nil {
		t.Fatal(err)
	}

	_, priv, _ := ed25519.GenerateKey(nil)
	fa := &trailAnchor{}
	s, err := audit.NewAuditedEventStore(mem, audit.Ed25519Signer{Priv: priv}, fa)
	if err != nil {
		t.Fatal(err)
	}
	j, err := agent.NewJournal(agent.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.NotifyPending(ctx, j, &recSink{}, audit.NewNotifyTrail(s), agent.WithNotifyHolder("sweeper"), agent.WithNotifyErrors(func(err error) { t.Log("notifier:", err) })); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{agent.NotifyDigestTrail, idx, segs[3], segs[4], segs[5]} {
		if len(fa.sizes(name)) == 0 {
			t.Errorf("%s was not signed by the first sweep", name)
		}
	}
	for _, name := range segs[:3] {
		if len(fa.sizes(name)) != 0 {
			t.Errorf("%s, an old sealed segment, was signed again", name)
		}
	}
}
