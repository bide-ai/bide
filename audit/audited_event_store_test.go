package audit_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"slices"
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

func TestAuditedEventStore_RefusesBadConfiguration(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	good := audit.Ed25519Signer{Priv: priv}
	for name, c := range map[string]func() (*audit.AuditedEventStore, error){
		"nil store":  func() (*audit.AuditedEventStore, error) { return audit.NewAuditedEventStore(nil, good, audit.NewMemAnchorLog()) },
		"nil anchor": func() (*audit.AuditedEventStore, error) { return audit.NewAuditedEventStore(audit.NewMemEventStore(), good, nil) },
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

	appendNotify(t, tr, trailName, 0, 3)
	appendNotify(t, tr, "notify:r2", 0, 1)

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
	if got, err := tr.Load(ctx, trailName); err != nil || len(got) != 3 {
		t.Fatalf("trail = %d records, %v", len(got), err)
	}
	if r.errs.n() != 3 || len(fa.sizes(trailName)) != 0 {
		t.Fatalf("%d anchor errors (want 3), heads %v", r.errs.n(), fa.sizes(trailName))
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
	fa.set(false)
	appendNotify(t, tr, "notify:c", 0, 1) // anchored at once

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
	if fa.tried() != 1 || r.errs.n() != 1 {
		t.Fatalf("%d publish attempts and %d errors in an outage of 10 appends, want 1 and 1", fa.tried(), r.errs.n())
	}
	if !slices.Equal(r.s.Unanchored(), []string{trailName}) {
		t.Fatalf("Unanchored = %v", r.s.Unanchored())
	}

	fa.set(false)
	time.Sleep(120 * time.Millisecond)
	appendNotify(t, tr, trailName, 10, 1)
	if got := fa.sizes(trailName); !slices.Equal(got, []int{11}) {
		t.Fatalf("heads after the cooldown = %v, want one head of size 11", got)
	}
	if len(r.s.Unanchored()) != 0 {
		t.Fatalf("Unanchored = %v", r.s.Unanchored())
	}
}

// A Publish that never returns and ignores its context cannot stall a write for longer than the
// timeout, and the writes after it do not wait for the anchor at all.
func TestAuditedEventStore_HungAnchorDoesNotStallWrites(t *testing.T) {
	hang := make(chan struct{})
	fa := &trailAnchor{hang: hang}
	r := newRig(t, fa)
	r.s.WithTimeout(100 * time.Millisecond).WithCooldown(time.Hour)
	tr := audit.NewNotifyTrail(r.s)

	start := time.Now()
	appendNotify(t, tr, trailName, 0, 1)
	if d := time.Since(start); d < 90*time.Millisecond || d > 3*time.Second {
		t.Fatalf("the first write took %v against a hung anchor, want about the 100ms timeout", d)
	}
	if r.errs.n() != 1 || !errors.Is(r.errs.errs[0], context.DeadlineExceeded) {
		t.Fatalf("errors = %v, want one deadline error", r.errs.errs)
	}
	start = time.Now()
	appendNotify(t, tr, trailName, 1, 20)
	if d := time.Since(start); d > time.Second || fa.tried() != 1 {
		t.Fatalf("20 more writes took %v with %d publish attempts: they waited on a hung anchor", d, fa.tried())
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
	if len(sizes) < 2 || sizes[len(sizes)-1] != 21 || !slices.IsSorted(sizes) {
		t.Fatalf("heads = %v, want growing sizes ending at 21", sizes)
	}
}

// A write under a context that is done cannot be anchored: it is reported, remembered, and
// anchored by the next Reanchor.
func TestAuditedEventStore_DoneContextIsReportedAndRetried(t *testing.T) {
	fa := &trailAnchor{}
	r := newRig(t, fa)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := audit.NewNotifyTrail(r.s).Append(ctx, trailName, 0, agent.NotifyRecord{Kind: agent.NotifyClaimed, Key: "k", RunID: "r1", At: t0, Holder: "p"}); err != nil {
		t.Fatal(err)
	}
	if r.errs.n() != 1 || !errors.Is(r.errs.errs[0], context.Canceled) || fa.tried() != 0 {
		t.Fatalf("errors %v, %d attempts", r.errs.errs, fa.tried())
	}
	if !slices.Equal(r.s.Unanchored(), []string{trailName}) {
		t.Fatalf("Unanchored = %v", r.s.Unanchored())
	}
	if err := r.s.ReanchorPending(context.Background()); err != nil || !slices.Equal(fa.sizes(trailName), []int{1}) {
		t.Fatalf("%v, heads %v", err, fa.sizes(trailName))
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
	before := fa.tried()
	if before != 1 {
		t.Fatalf("%d attempts for the first append", before)
	}
	if err := r.s.Append(context.Background(), trailName, 0, []byte("other")); !errors.Is(err, audit.ErrSeqTaken) {
		t.Fatalf("different leaf = %v, want ErrSeqTaken", err)
	}
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
	if notifyErrs.n() != 0 {
		t.Fatalf("the notifier reported %v: an anchor error must not surface as a delivery failure", notifyErrs.errs)
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
	go func() { done <- agent.NotifyPending(context.Background(), j, &recSink{}, trail, agent.WithNotifyHolder("sweeper")) }()
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
