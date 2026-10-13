package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// clock is a settable clock; sleeping on it advances it.
type notifyClock struct {
	mu     sync.Mutex
	t      time.Time
	sleeps []time.Duration
}

func (c *notifyClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *notifyClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}
func (c *notifyClock) sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	c.sleeps = append(c.sleeps, d)
	c.t = c.t.Add(d)
	c.mu.Unlock()
	return nil
}

// fakeSink is a Notifier that fails the first fail calls, then succeeds, recording what it saw.
type fakeSink struct {
	mu    sync.Mutex
	fail  int
	err   error
	calls []Notification
	ack   *NotifyAck
}

func (f *fakeSink) Notify(ctx context.Context, n Notification) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, n)
	if len(f.calls) <= f.fail {
		if f.err != nil {
			return f.err
		}
		return errors.New("sink down")
	}
	if f.ack != nil {
		ReportNotifyAck(ctx, *f.ack)
	}
	return nil
}

func (f *fakeSink) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

func testEngine(t *testing.T, j *Journal, n Notifier, trail NotifyTrail, clk *notifyClock, holder string, opts ...NotifyOption) *notifyEngine {
	t.Helper()
	all := append([]NotifyOption{
		WithNotifyHolder(holder),
		notifyOption(func(c *notifyConfig) error {
			c.now, c.sleep, c.jitter = clk.now, clk.sleep, func(d time.Duration) time.Duration { return d }
			return nil
		}),
	}, opts...)
	e, err := newNotifyEngine(j, n, trail, all)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func haltEvent() notifyEvent {
	return notifyEvent{key: HaltedKey("r", OpTool, "c1", 100), kind: NotificationOutcomeUnknown, runID: "r",
		halt: &NotifyHalt{Op: OpRef{Kind: OpTool, ID: "c1", ToolName: "pay"}, Cause: HaltCrashed}}
}

func recKinds(recs []NotifyRecord) []NotifyRecordKind {
	var out []NotifyRecordKind
	for _, r := range recs {
		out = append(out, r.Kind)
	}
	return out
}

func eqRecKinds(a, b []NotifyRecordKind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestNotifyRound_DeliversOnceAndRecordsIt(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	trail, sink := NewMemNotifyTrail(), &fakeSink{ack: &NotifyAck{Status: 200, Auth: &NotifyAuth{Signature: "hmac-sha256", SecretSlot: "current"}}}
	e := testEngine(t, memJournal(), sink, trail, clk, "p1")
	ev := haltEvent()

	res, err := e.round(ctx, ev)
	if err != nil || !res.done {
		t.Fatalf("round = %+v, %v", res, err)
	}
	if sink.count() != 1 {
		t.Fatalf("notified %d times, want 1", sink.count())
	}
	n := sink.calls[0]
	if n.ID != NotifyID(ev.key) || n.Key != ev.key || n.Kind != NotificationOutcomeUnknown || n.Attempt != 1 || n.Halt.Op.ID != "c1" || !n.Deadline.Equal(clk.now().Add(5*time.Minute)) {
		t.Fatalf("notification = %+v", n)
	}
	recs, _ := trail.Load(ctx, "notify:r")
	if got := recKinds(recs); !eqRecKinds(got, []NotifyRecordKind{NotifyObserved, NotifyClaimed, NotifyAcked}) {
		t.Fatalf("trail = %v", got)
	}
	acked := recs[2]
	if acked.Attempts != 1 || acked.Status != 200 || acked.Auth == nil || acked.Auth.SecretSlot != "current" || acked.FirstSentAt.IsZero() {
		t.Fatalf("acked = %+v", acked)
	}

	// Another pass, and another process, find it done and send nothing.
	if res, err := e.round(ctx, ev); err != nil || !res.done {
		t.Fatalf("second round = %+v, %v", res, err)
	}
	e2 := testEngine(t, memJournal(), sink, trail, clk, "p2")
	if res, err := e2.round(ctx, ev); err != nil || !res.done {
		t.Fatalf("other process = %+v, %v", res, err)
	}
	if sink.count() != 1 {
		t.Fatalf("a closed event was sent again: %d calls", sink.count())
	}
}

func TestNotifyRound_RetriesWithinARoundWithBackoff(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	trail, sink := NewMemNotifyTrail(), &fakeSink{fail: 2}
	e := testEngine(t, memJournal(), sink, trail, clk, "p1")
	if res, err := e.round(ctx, haltEvent()); err != nil || !res.done {
		t.Fatalf("round = %+v, %v", res, err)
	}
	if sink.count() != 3 || sink.calls[2].Attempt != 3 {
		t.Fatalf("calls = %d, last attempt %d; want 3 and 3", sink.count(), sink.calls[len(sink.calls)-1].Attempt)
	}
	if len(clk.sleeps) != 2 || clk.sleeps[0] != time.Second || clk.sleeps[1] != 2*time.Second {
		t.Fatalf("backoff within a round = %v, want 1s then 2s", clk.sleeps)
	}
	recs, _ := trail.Load(ctx, "notify:r")
	if recs[len(recs)-1].Attempts != 3 {
		t.Fatalf("acked.attempts = %d, want 3", recs[len(recs)-1].Attempts)
	}
}

func TestNotifyRound_PermanentErrorEndsTheRoundNeverTheEvent(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	trail := NewMemNotifyTrail()
	sink := &fakeSink{fail: 1000, err: ErrNotifyPermanent}
	var reported []error
	e := testEngine(t, memJournal(), sink, trail, clk, "p1", WithOverdueNotifier(&fakeSink{}), WithNotifyFailingAfter(3), WithNotifyErrors(func(err error) { reported = append(reported, err) }))
	ev := haltEvent()

	res, err := e.round(ctx, ev)
	if err == nil || res.done || sink.count() != 1 {
		t.Fatalf("a permanent error must end the round after one call: res=%+v err=%v calls=%d", res, err, sink.count())
	}
	if !res.next.After(clk.now()) {
		t.Fatalf("next = %v, want a future retry", res.next)
	}
	// Rounds keep coming, however many: the third failure writes failing once, and the event never closes.
	for range 6 {
		clk.advance(20 * time.Minute) // past every backoff and the claim
		if res, _ := e.round(ctx, ev); res.done {
			t.Fatal("an event closed without an acknowledgement")
		}
	}
	recs, _ := trail.Load(ctx, "notify:r")
	failing := 0
	for _, r := range recs {
		if r.Kind == NotifyFailing && r.Key == ev.key {
			failing++
			if r.Failures != 3 {
				t.Fatalf("failing.failures = %d, want 3", r.Failures)
			}
		}
		if r.Kind == NotifyAcked && r.Key == ev.key {
			t.Fatal("acked without a delivery")
		}
	}
	if failing != 1 {
		t.Fatalf("%d failing records, want 1", failing)
	}
	own := 0
	for _, err := range reported {
		if strings.Contains(err.Error(), ev.key) && strings.Contains(err.Error(), "is failing") {
			own++
		}
	}
	if own != 1 {
		t.Fatalf("failing reported %d times for the event, want 1: %v", own, reported)
	}
	// The sink comes back: the same event is delivered, with the same ID, and closes.
	sink.mu.Lock()
	sink.fail = 0
	sink.mu.Unlock()
	clk.advance(20 * time.Minute)
	if res, err := e.round(ctx, ev); err != nil || !res.done {
		t.Fatalf("after recovery: %+v, %v", res, err)
	}
	if last := sink.calls[len(sink.calls)-1]; last.ID != NotifyID(ev.key) || last.Attempt <= 3 {
		t.Fatalf("last = %+v", last)
	}
	_ = e.flush(ctx)
}

// failAckTrail fails every append of an acked record, as a store that is down at the worst moment.
type failAckTrail struct {
	NotifyTrail
	fail atomic.Bool
}

func (f *failAckTrail) Append(ctx context.Context, trail string, seq int, rec NotifyRecord) error {
	if rec.Kind == NotifyAcked && f.fail.Load() {
		return errors.New("store down")
	}
	return f.NotifyTrail.Append(ctx, trail, seq, rec)
}

// The documented duplicate window: delivered, but the acknowledgement could not be recorded. The
// next round sends it again with the same ID, and then closes it.
func TestNotifyRound_DeliveredButNotRecordedIsSentAgain(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	ft := &failAckTrail{NotifyTrail: NewMemNotifyTrail()}
	ft.fail.Store(true)
	sink := &fakeSink{}
	e := testEngine(t, memJournal(), sink, ft, clk, "p1")
	res, err := e.round(ctx, haltEvent())
	if err == nil || res.done || sink.count() != 1 {
		t.Fatalf("round = %+v, %v, calls %d", res, err, sink.count())
	}
	ft.fail.Store(false)
	clk.advance(time.Minute)
	if res, err := e.round(ctx, haltEvent()); err != nil || !res.done {
		t.Fatalf("second round = %+v, %v", res, err)
	}
	if sink.count() != 2 || sink.calls[0].ID != sink.calls[1].ID {
		t.Fatalf("calls = %d with ids %q %q: want exactly one duplicate with one ID", sink.count(), sink.calls[0].ID, sink.calls[len(sink.calls)-1].ID)
	}
}

func TestNotifyRound_AnotherHoldersLiveClaimIsRespectedThenTakenOver(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	ev := haltEvent()
	now := clk.now()
	_ = trail.Append(ctx, "notify:r", 0, NotifyRecord{Kind: NotifyObserved, Key: ev.key, RunID: "r", At: now, EventKind: string(ev.kind), DeadlineAt: now.Add(5 * time.Minute)})
	_ = trail.Append(ctx, "notify:r", 1, NotifyRecord{Kind: NotifyClaimed, Key: ev.key, RunID: "r", At: now, Holder: "crashed", Until: now.Add(2 * time.Minute)})

	e := testEngine(t, memJournal(), sink, trail, clk, "p2")
	res, err := e.round(ctx, ev)
	if err != nil || res.done || sink.count() != 0 || !res.next.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("a live claim must be respected until it lapses: %+v %v calls=%d", res, err, sink.count())
	}
	clk.advance(2 * time.Minute) // the claim lapses exactly at Until
	if res, err := e.round(ctx, ev); err != nil || !res.done || sink.count() != 1 {
		t.Fatalf("takeover = %+v, %v, calls=%d", res, err, sink.count())
	}
	recs, _ := trail.Load(ctx, "notify:r")
	if got := recKinds(recs); !eqRecKinds(got, []NotifyRecordKind{NotifyObserved, NotifyClaimed, NotifyClaimed, NotifyAcked}) || recs[2].Holder != "p2" {
		t.Fatalf("trail = %v", got)
	}
}

// Many processes see the same event at once. The append at one position is the only arbiter, and
// exactly one of them delivers.
func TestNotifyRound_ConcurrentProcessesDeliverOnce(t *testing.T) {
	ctx := context.Background()
	for iter := range 40 {
		clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
		trail, sink := NewMemNotifyTrail(), &fakeSink{}
		const procs = 6
		var wg sync.WaitGroup
		start := make(chan struct{})
		for p := range procs {
			e := testEngine(t, memJournal(), sink, trail, clk, "p"+string(rune('a'+p)))
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, _ = e.round(ctx, haltEvent())
			}()
		}
		close(start)
		wg.Wait()
		if sink.count() != 1 {
			t.Fatalf("iteration %d: %d processes delivered, want exactly 1", iter, sink.count())
		}
		recs, _ := trail.Load(ctx, "notify:r")
		got := recKinds(recs)
		if !eqRecKinds(got, []NotifyRecordKind{NotifyObserved, NotifyClaimed, NotifyAcked}) {
			t.Fatalf("iteration %d: trail = %v", iter, got)
		}
	}
}

func TestNotifyRound_OverdueIsRaisedOnceAndTheOriginalKeepsRetrying(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	trail, sink, overdue := NewMemNotifyTrail(), &fakeSink{fail: 1000}, &fakeSink{fail: 1000, err: ErrNotifyPermanent}
	e := testEngine(t, memJournal(), sink, trail, clk, "p1", WithOverdueNotifier(overdue))
	ev := haltEvent()
	_, _ = e.round(ctx, ev)
	if overdue.count() != 0 {
		t.Fatal("overdue before the deadline")
	}
	clk.advance(6 * time.Minute) // past the 5 minute deadline
	_, _ = e.round(ctx, ev)
	_, _ = e.round(ctx, ev)
	recs, _ := trail.Load(ctx, "notify:r")
	n := 0
	for _, r := range recs {
		if r.Kind == NotifyOverdue && r.Key == ev.key {
			n++
			if r.Latency < 5*time.Minute {
				t.Fatalf("overdue.latency = %v", r.Latency)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d overdue records, want 1", n)
	}
	// The overdue notice is its own event, delivered to the overdue sink by the worker.
	waitFor(t, func() bool { return overdue.count() == 1 })
	if got := overdue.calls[0]; got.Kind != NotificationOverdue || got.Key != OverdueKey(ev.key) || got.Reason != "deadline" {
		t.Fatalf("overdue notice = %+v", got)
	}
	// An overdue notice that itself goes unacknowledged is retried, not escalated again.
	waitFor(t, func() bool { return e.w.settled() })
	_ = e.flush(ctx)
	noticeEv := notifyEvent{key: OverdueKey(ev.key), kind: NotificationOverdue, runID: "r", origKey: ev.key, reason: "deadline"}
	for range 3 {
		clk.advance(20 * time.Minute) // past the notice's backoff, its claim and what would be its deadline
		_, _ = e.round(ctx, noticeEv)
	}
	recs, _ = trail.Load(ctx, "notify:r")
	for _, r := range recs {
		if r.Kind == NotifyOverdue && r.Key == OverdueKey(ev.key) {
			t.Fatal("an overdue notice was itself marked overdue")
		}
	}
	// The original keeps retrying and closes when the sink returns.
	sink.mu.Lock()
	sink.fail = 0
	sink.mu.Unlock()
	clk.advance(20 * time.Minute)
	if res, err := e.round(ctx, ev); err != nil || !res.done {
		t.Fatalf("original = %+v, %v", res, err)
	}
	_ = e.flush(ctx)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for range 400 {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}
