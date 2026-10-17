package agent

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"reflect"
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
	e, err := newNotifyEngine(j, n, trail, all, "test")
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
	e := testEngine(t, memJournal(), sink, trail, clk, "p1", WithOverdueNotifier(&fakeSink{}), WithNotifyFailingAfter(9), WithNotifyErrors(func(err error) { reported = append(reported, err) }))
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
				t.Fatalf("failing.failures = %d, want 3 rounds", r.Failures)
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

// failNTrail fails the next n appends of an acked record, as a store that is down at the worst moment.
type failNTrail struct {
	NotifyTrail
	mu sync.Mutex
	n  int
}

func (f *failNTrail) Append(ctx context.Context, trail string, seq int, rec NotifyRecord) error {
	if rec.Kind == NotifyAcked {
		f.mu.Lock()
		if f.n > 0 {
			f.n--
			f.mu.Unlock()
			return errors.New("store down")
		}
		f.mu.Unlock()
	}
	return f.NotifyTrail.Append(ctx, trail, seq, rec)
}

func (f *failNTrail) set(n int) { f.mu.Lock(); f.n = n; f.mu.Unlock() }

// A store error on the acked append is retried in place, under the round's claim, so one delivery
// is one notification.
func TestNotifyRound_AckIsRetriedInPlace(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	ft := &failNTrail{NotifyTrail: NewMemNotifyTrail()}
	ft.set(2) // fewer than the in-place retries
	sink := &fakeSink{}
	e := testEngine(t, memJournal(), sink, ft, clk, "p1")
	if res, err := e.round(ctx, haltEvent()); err != nil || !res.done {
		t.Fatalf("round = %+v, %v", res, err)
	}
	recs, _ := ft.Load(ctx, "notify:r")
	if sink.count() != 1 || !eqRecKinds(recKinds(recs), []NotifyRecordKind{NotifyObserved, NotifyClaimed, NotifyAcked}) {
		t.Fatalf("calls=%d trail=%v; want one delivery, one claim, one ack", sink.count(), recKinds(recs))
	}
}

// If the store stays down past the in-place retries, the next round of the same process records the
// acknowledgement it owes and does not send again. Only a restart, which forgets that it delivered,
// repeats the notification: the documented duplicate window.
func TestNotifyRound_AnUnrecordedAckIsNotSentAgainByTheSameProcess(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	ft := &failNTrail{NotifyTrail: NewMemNotifyTrail()}
	ft.set(1000)
	sink := &fakeSink{}
	e := testEngine(t, memJournal(), sink, ft, clk, "p1")
	res, err := e.round(ctx, haltEvent())
	if err == nil || res.done || sink.count() != 1 {
		t.Fatalf("round = %+v, %v, calls %d", res, err, sink.count())
	}
	ft.set(0)
	clk.advance(time.Minute)
	if res, err := e.round(ctx, haltEvent()); err != nil || !res.done {
		t.Fatalf("second round = %+v, %v", res, err)
	}
	if sink.count() != 1 {
		t.Fatalf("the same process sent %d times, want 1", sink.count())
	}
	recs, _ := ft.Load(ctx, "notify:r")
	if got := recKinds(recs); got[len(got)-1] != NotifyAcked || recs[len(recs)-1].Attempts != 1 {
		t.Fatalf("trail = %v, last %+v", got, recs[len(recs)-1])
	}

	// A restart forgets: it delivers again, with the same ID.
	ft2 := &failNTrail{NotifyTrail: NewMemNotifyTrail()}
	ft2.set(1000)
	sink2 := &fakeSink{}
	e1 := testEngine(t, memJournal(), sink2, ft2, clk, "p1")
	_, _ = e1.round(ctx, haltEvent())
	ft2.set(0)
	clk.advance(4*time.Minute + 10*time.Second) // the claim and its skew margin have lapsed; the event is not yet overdue
	e2 := testEngine(t, memJournal(), sink2, ft2, clk, "p1")
	if res, err := e2.round(ctx, haltEvent()); err != nil || !res.done {
		t.Fatalf("after restart = %+v, %v", res, err)
	}
	if sink2.count() != 2 || sink2.calls[0].ID != sink2.calls[1].ID {
		t.Fatalf("after a restart: %d calls; want the one duplicate with one ID", sink2.count())
	}
}

// Two deliverers in one process that share a configured holder name (a drive's notifier and a
// sweep, say) are still two holders: a live claim of one stops the other.
func TestNotifyRound_TwoDeliverersWithOneNameAreTwoHolders(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	trail := NewMemNotifyTrail()
	block, entered := make(chan struct{}), make(chan struct{})
	slow := notifierFunc(func(context.Context, Notification) error {
		close(entered)
		<-block
		return nil
	})
	fast := &fakeSink{}
	a := testEngine(t, memJournal(), slow, trail, clk, "same")
	b := testEngine(t, memJournal(), fast, trail, clk, "same")
	done := make(chan struct{})
	go func() { defer close(done); _, _ = a.round(ctx, haltEvent()) }()
	<-entered
	res, err := b.round(ctx, haltEvent())
	if err != nil || res.done || fast.count() != 0 {
		t.Fatalf("the second deliverer delivered under the first one's live claim: %+v %v calls=%d", res, err, fast.count())
	}
	close(block)
	<-done
	recs, _ := trail.Load(ctx, "notify:r")
	holders := map[string]bool{}
	for _, r := range recs {
		if r.Kind == NotifyClaimed {
			holders[r.Holder] = true
		}
	}
	if len(holders) != 1 || !strings.HasPrefix(recs[1].Holder, "same/") {
		t.Fatalf("claims: %+v", recs)
	}
	c := testEngine(t, memJournal(), fast, trail, clk, "same")
	if c.holder == a.holder || c.holder == b.holder || a.holder == b.holder {
		t.Fatalf("holders collide: %q %q %q", a.holder, b.holder, c.holder)
	}
}

type notifierFunc func(context.Context, Notification) error

func (f notifierFunc) Notify(ctx context.Context, n Notification) error { return f(ctx, n) }

// The failing signal comes from the trail: rounds that claimed and never acknowledged, whoever ran
// them and however they ended, so a process that dies in every round still raises it.
func TestNotifyRound_FailingIsCountedFromTheTrailAcrossRestarts(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	trail := NewMemNotifyTrail()
	ev := haltEvent()
	failingCount := func() (n int, failures int) {
		recs, _ := trail.Load(ctx, "notify:r")
		for _, r := range recs {
			if r.Kind == NotifyFailing && r.Key == ev.key {
				n++
				failures = r.Failures
			}
		}
		return
	}
	// Default: ceil(8/3) = 3 rounds. Each round is a new process (empty memory); the sink is down.
	for i := range 2 {
		e := testEngine(t, memJournal(), &fakeSink{fail: 1000}, trail, clk, "p")
		_, _ = e.round(ctx, ev)
		if n, _ := failingCount(); n != 0 {
			t.Fatalf("failing raised after only %d rounds", i+1)
		}
		clk.advance(20 * time.Minute)
	}
	e := testEngine(t, memJournal(), &fakeSink{fail: 1000}, trail, clk, "p")
	_, _ = e.round(ctx, ev)
	if n, f := failingCount(); n != 1 || f != 3 {
		t.Fatalf("after 3 rounds: %d failing records with failures=%d; want 1 with 3", n, f)
	}
	clk.advance(20 * time.Minute)
	_, _ = testEngine(t, memJournal(), &fakeSink{fail: 1000}, trail, clk, "p").round(ctx, ev)
	if n, _ := failingCount(); n != 1 {
		t.Fatalf("%d failing records, want it raised once", n)
	}
}

// Rounds that crashed (a claim with nothing after it) count too: a process that dies in every
// round never reaches the end of one.
func TestNotifyRound_CrashedRoundsCountTowardFailing(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	trail := NewMemNotifyTrail()
	ev := haltEvent()
	now := clk.now()
	_ = trail.Append(ctx, "notify:r", 0, NotifyRecord{Kind: NotifyObserved, Key: ev.key, RunID: "r", At: now, EventKind: string(ev.kind), DeadlineAt: now.Add(time.Hour)})
	for i := 1; i <= 3; i++ {
		_ = trail.Append(ctx, "notify:r", i, NotifyRecord{Kind: NotifyClaimed, Key: ev.key, RunID: "r", At: now, Holder: "dead", Until: now.Add(time.Minute)})
	}
	clk.advance(5 * time.Minute)
	sink := &fakeSink{}
	e := testEngine(t, memJournal(), sink, trail, clk, "p")
	if res, err := e.round(ctx, ev); err != nil || !res.done {
		t.Fatalf("round = %+v, %v", res, err)
	}
	recs, _ := trail.Load(ctx, "notify:r")
	got := recKinds(recs)
	want := []NotifyRecordKind{NotifyObserved, NotifyClaimed, NotifyClaimed, NotifyClaimed, NotifyFailing, NotifyClaimed, NotifyAcked}
	if !eqRecKinds(got, want) {
		t.Fatalf("trail = %v; want failing raised before the fourth round delivers", got)
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
	if err != nil || res.done || sink.count() != 0 || !res.next.Equal(now.Add(4*time.Minute)) {
		t.Fatalf("a live claim must be respected until it lapses, plus the skew margin: %+v %v calls=%d", res, err, sink.count())
	}
	clk.advance(4 * time.Minute) // Until (2 minutes) plus the 2 minute skew margin
	if res, err := e.round(ctx, ev); err != nil || !res.done || sink.count() != 1 {
		t.Fatalf("takeover = %+v, %v, calls=%d", res, err, sink.count())
	}
	recs, _ := trail.Load(ctx, "notify:r")
	if got := recKinds(recs); !eqRecKinds(got, []NotifyRecordKind{NotifyObserved, NotifyClaimed, NotifyClaimed, NotifyAcked}) || !strings.HasPrefix(recs[2].Holder, "p2/") {
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
	count := func(kind NotifyRecordKind, key string) int {
		recs, _ := trail.Load(ctx, "notify:r")
		return countKind(recs, kind, key)
	}
	notice := OverdueKey(ev.key)

	// Past the deadline: one overdue record, and the notice goes out in the round that delivers the
	// event, to the overdue sink, which is down. Every later round tries it again.
	for i := range 3 {
		clk.advance(20 * time.Minute)
		_, _ = e.round(ctx, ev)
		if overdue.count() != i+1 {
			t.Fatalf("round %d: %d overdue attempts, want one per round", i, overdue.count())
		}
	}
	if count(NotifyOverdue, ev.key) != 1 || count(NotifyObserved, notice) != 1 || count(NotifyClaimed, notice) != 0 {
		recs, _ := trail.Load(ctx, "notify:r")
		t.Fatalf("overdue records %d, notice observed %d, notice claims %d: %v", count(NotifyOverdue, ev.key), count(NotifyObserved, notice), count(NotifyClaimed, notice), recKinds(recs))
	}
	if got := overdue.calls[0]; got.Kind != NotificationOverdue || got.Key != notice || got.Reason != "deadline" {
		t.Fatalf("overdue notice = %+v", got)
	}
	// The overdue sink returns: the notice is acknowledged once, and not sent again.
	overdue.mu.Lock()
	overdue.fail = 0
	overdue.mu.Unlock()
	clk.advance(20 * time.Minute)
	_, _ = e.round(ctx, ev)
	n := overdue.count()
	if count(NotifyAcked, notice) != 1 {
		t.Fatal("the notice was not acknowledged")
	}
	clk.advance(20 * time.Minute)
	_, _ = e.round(ctx, ev)
	if overdue.count() != n {
		t.Fatal("an acknowledged notice was sent again")
	}
	// The original keeps retrying and closes when its sink returns.
	sink.mu.Lock()
	sink.fail = 0
	sink.mu.Unlock()
	clk.advance(20 * time.Minute)
	if res, err := e.round(ctx, ev); err != nil || !res.done {
		t.Fatalf("original = %+v, %v", res, err)
	}
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

// One engine runs one round at a time for a key, so a drive's goroutine and a sweep that share an
// engine cannot both deliver under its own live claim.
func TestNotifyRound_OneRoundAtATimePerKey(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	block, entered := make(chan struct{}), make(chan struct{}, 2)
	var calls atomic.Int32
	sink := notifierFunc(func(context.Context, Notification) error {
		calls.Add(1)
		entered <- struct{}{}
		<-block
		return nil
	})
	e := testEngine(t, memJournal(), sink, NewMemNotifyTrail(), clk, "p")
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = e.round(ctx, haltEvent()) }()
	}
	<-entered
	time.Sleep(20 * time.Millisecond) // the second round, if it were not held, would be in Notify by now
	close(block)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("%d deliveries under one engine, want 1", calls.Load())
	}
}

var notifyT0 = time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)

func nrec(k NotifyRecordKind, key string, min int) NotifyRecord {
	return NotifyRecord{Kind: k, Key: key, RunID: "r", At: notifyT0.Add(time.Duration(min) * time.Minute)}
}

func observed(key string, min int) NotifyRecord {
	r := nrec(NotifyObserved, key, min)
	r.EventKind = "approval_pending"
	r.EventAt = r.At.Add(-time.Second)
	r.DeadlineAt = r.At.Add(5 * time.Minute)
	return r
}

func claimed(key string, min int, until int) NotifyRecord {
	r := nrec(NotifyClaimed, key, min)
	r.Holder = "p"
	r.Until = notifyT0.Add(time.Duration(until) * time.Minute)
	return r
}

func TestFoldNotify_Lifecycle(t *testing.T) {
	st := FoldNotify([]NotifyRecord{observed("k", 0), claimed("k", 1, 3), nrec(NotifyAcked, "k", 2)})
	e := st.Events["k"]
	if e == nil || e.Open() || e.Acked == nil || e.Rounds != 1 || e.Kind != "approval_pending" || !e.EventAt.Equal(notifyT0.Add(-time.Second)) {
		t.Fatalf("event = %+v", e)
	}
	if !reflect.DeepEqual(st.Keys, []string{"k"}) {
		t.Fatalf("keys = %v", st.Keys)
	}
	open := FoldNotify([]NotifyRecord{observed("k", 0)}).Events["k"]
	if !open.Open() || open.Claim != nil {
		t.Fatalf("a fresh event must be open and unclaimed: %+v", open)
	}
}

func TestFoldNotify_IgnoresRecordsThatDoNotApply(t *testing.T) {
	cases := map[string][]NotifyRecord{
		"claim before observation":     {claimed("k", 0, 1)},
		"second observation":           {observed("k", 0), observed("k", 5)},
		"claim after ack":              {observed("k", 0), nrec(NotifyAcked, "k", 1), claimed("k", 2, 4)},
		"ack after clear":              {observed("k", 0), nrec(NotifyCleared, "k", 1), nrec(NotifyAcked, "k", 2)},
		"clear after ack":              {observed("k", 0), nrec(NotifyAcked, "k", 1), nrec(NotifyCleared, "k", 2)},
		"overdue after ack":            {observed("k", 0), nrec(NotifyAcked, "k", 1), nrec(NotifyOverdue, "k", 9)},
		"failing after clear":          {observed("k", 0), nrec(NotifyCleared, "k", 1), nrec(NotifyFailing, "k", 2)},
		"recovered without an episode": {nrec(NotifyRecovered, "", 1)},
		"unknown kind":                 {observed("k", 0), {Kind: "bogus", Key: "k", At: notifyT0}},
	}
	for name, recs := range cases {
		st := FoldNotify(recs)
		switch name {
		case "claim before observation":
			if len(st.Events) != 0 {
				t.Errorf("%s: an event appeared from a claim alone", name)
			}
		case "second observation":
			if !st.Events["k"].ObservedAt.Equal(notifyT0) || len(st.Keys) != 1 {
				t.Errorf("%s: the second observation replaced the first: %+v", name, st.Events["k"])
			}
		case "claim after ack":
			if e := st.Events["k"]; e.Rounds != 0 || e.Claim != nil || e.Acked == nil {
				t.Errorf("%s: %+v", name, e)
			}
		case "ack after clear":
			if e := st.Events["k"]; e.Acked != nil || e.Cleared == nil {
				t.Errorf("%s: the later terminal record won: %+v", name, e)
			}
		case "clear after ack":
			if e := st.Events["k"]; e.Cleared != nil || e.Acked == nil {
				t.Errorf("%s: the later terminal record won: %+v", name, e)
			}
		case "overdue after ack":
			if st.Events["k"].Overdue != nil {
				t.Errorf("%s: overdue recorded on a closed event", name)
			}
		case "failing after clear":
			if st.Events["k"].Failing != nil {
				t.Errorf("%s: failing recorded on a closed event", name)
			}
		case "recovered without an episode":
			if st.Episodes != 0 {
				t.Errorf("%s: Episodes = %d", name, st.Episodes)
			}
		}
	}
}

func TestFoldNotify_FailingAndOverdueAreRaisedOnce(t *testing.T) {
	f1, f2 := nrec(NotifyFailing, "k", 1), nrec(NotifyFailing, "k", 2)
	f1.Failures, f2.Failures = 8, 9
	o1, o2 := nrec(NotifyOverdue, "k", 6), nrec(NotifyOverdue, "k", 7)
	e := FoldNotify([]NotifyRecord{observed("k", 0), f1, f2, o1, o2}).Events["k"]
	if e.Failing.Failures != 8 || !e.Overdue.At.Equal(notifyT0.Add(6*time.Minute)) {
		t.Fatalf("the first failing and overdue records must stand: %+v %+v", e.Failing, e.Overdue)
	}
}

func TestNotifyEvent_ClaimLiveAndOverdueBoundaries(t *testing.T) {
	e := FoldNotify([]NotifyRecord{observed("k", 0), claimed("k", 1, 3)}).Events["k"]
	if !e.ClaimLive(notifyT0.Add(3*time.Minute - time.Nanosecond)) {
		t.Error("a claim is live until its Until")
	}
	if e.ClaimLive(notifyT0.Add(3 * time.Minute)) {
		t.Error("a claim lapses at Until")
	}
	// observed at minute 0, deadline minute 5
	if e.OverdueDue(notifyT0.Add(5 * time.Minute)) {
		t.Error("not overdue at the deadline itself")
	}
	if !e.OverdueDue(notifyT0.Add(5*time.Minute + time.Nanosecond)) {
		t.Error("overdue just past the deadline")
	}
	e2 := FoldNotify([]NotifyRecord{observed("k", 0), nrec(NotifyOverdue, "k", 6)}).Events["k"]
	if e2.OverdueDue(notifyT0.Add(time.Hour)) {
		t.Error("overdue is raised once")
	}
	e3 := FoldNotify([]NotifyRecord{observed("k", 0), nrec(NotifyAcked, "k", 1)}).Events["k"]
	if e3.OverdueDue(notifyT0.Add(time.Hour)) || e3.ClaimLive(notifyT0) {
		t.Error("a closed event is neither overdue nor claimed")
	}
	if got := FoldNotify([]NotifyRecord{observed("k", 0), claimed("k", 1, 3), claimed("k", 4, 6)}).Events["k"]; got.Rounds != 2 || !got.Claim.Until.Equal(notifyT0.Add(6*time.Minute)) {
		t.Errorf("the latest claim must win: %+v", got)
	}
}

func TestFoldNotify_RecoveryEpisodes(t *testing.T) {
	since := func(min int) NotifyRecord { return nrec(NotifyFailingSince, "", min) }
	st := FoldNotify([]NotifyRecord{since(0), since(5)})
	if st.FailingSince == nil || !st.FailingSince.At.Equal(notifyT0) {
		t.Fatalf("the first failing_since opens the episode: %+v", st.FailingSince)
	}
	d := 15 * time.Minute
	if st.RecoveryFailingDue(notifyT0.Add(d), d) {
		t.Error("not due at exactly D")
	}
	if !st.RecoveryFailingDue(notifyT0.Add(d+time.Nanosecond), d) {
		t.Error("due once past D")
	}
	st = FoldNotify([]NotifyRecord{since(0), nrec(NotifyRecovered, "", 3), since(10)})
	if st.Episodes != 1 || st.FailingSince == nil || !st.FailingSince.At.Equal(notifyT0.Add(10*time.Minute)) {
		t.Fatalf("a second episode after recovery: episodes=%d since=%+v", st.Episodes, st.FailingSince)
	}
	if (&NotifyState{}).RecoveryFailingDue(notifyT0, 0) {
		t.Error("no open episode is never due")
	}
}

func TestFoldNotify_IsPure(t *testing.T) {
	recs := []NotifyRecord{observed("a", 0), observed("b", 1), claimed("a", 2, 4), nrec(NotifyAcked, "b", 3)}
	before := append([]NotifyRecord(nil), recs...)
	a, b := FoldNotify(recs), FoldNotify(recs)
	if !reflect.DeepEqual(a, b) || !reflect.DeepEqual(recs, before) {
		t.Fatal("fold is not deterministic or mutated its input")
	}
	if !reflect.DeepEqual(a.Keys, []string{"a", "b"}) {
		t.Fatalf("keys = %v", a.Keys)
	}
}

func TestNotifyKeys(t *testing.T) {
	if HaltedKey("a:b", OpTool, "c", 1) == HaltedKey("a", OpTool, "b:c", 1) {
		t.Error("ids with ':' must not collide")
	}
	if ApprovalKey("r", "c", 0) == ApprovalKey("r", "c", 1) {
		t.Error("a reminder is a different event")
	}
	if got := HaltedKey("run 1", OpStep, "pay", 2); got != "halted:run%201:step:pay:2" {
		t.Errorf("halted key = %q", got)
	}
	if RecoveryKey("r", 0) == RecoveryKey("r", 1) {
		t.Error("episodes are different events")
	}
	id := NotifyID("k")
	if len(id) != 64 || id != NotifyID("k") || id == NotifyID("k2") {
		t.Errorf("NotifyID = %q", id)
	}
	if NotifyTrailName("r") != "notify:r" || !strings.HasPrefix(NotifyDigestTrail, "notify:") {
		t.Error("trail names")
	}
}

func TestNotifyRecord_JSONOmitsUnsetAndNormalizesZones(t *testing.T) {
	r := NotifyRecord{Kind: NotifyObserved, Key: "k", At: notifyT0.In(time.FixedZone("x", 7200))}
	b, _ := json.Marshal(r.Normalized())
	for _, absent := range []string{"event_at", "deadline_at", "until", "first_sent_at", "holder", "auth"} {
		if strings.Contains(string(b), `"`+absent+`"`) {
			t.Errorf("unset field %s is in %s", absent, b)
		}
	}
	if !strings.Contains(string(b), `"at":"2026-10-13T09:00:00Z"`) {
		t.Errorf("time not normalized to UTC: %s", b)
	}
	if !SameNotifyRecord(r, NotifyRecord{Kind: NotifyObserved, Key: "k", At: notifyT0}) {
		t.Error("one instant in two zones must be the same record")
	}
	if SameNotifyRecord(r, NotifyRecord{Kind: NotifyObserved, Key: "k2", At: notifyT0}) {
		t.Error("different keys must differ")
	}
}

// An event observed with no deadline (a resolved event) is never overdue, however late it is.
func TestFoldNotify_AnEventWithNoDeadlineIsNeverOverdue(t *testing.T) {
	at := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	st := FoldNotify([]NotifyRecord{
		{Kind: NotifyObserved, Key: "resolved:k", RunID: "r", At: at, EventKind: string(NotificationResolved), EventAt: at, Reason: "decided"},
	})
	e := st.Events["resolved:k"]
	if e.Reason != "decided" {
		t.Fatalf("reason = %q, want the observed record's", e.Reason)
	}
	if e.OverdueDue(at.Add(1000 * time.Hour)) {
		t.Fatal("an event with no deadline was overdue")
	}
}

// two processes deliver the same escalation step at once. The trail's append race and the
// claim must give one observation and one delivery.
func TestNotifyRace_TwoProcessesOneEscalationStep(t *testing.T) {
	for i := 0; i < 50; i++ {
		clk := routeClock()
		def, esc := &fakeSink{fail: 1000}, &fakeSink{}
		j, trail := memJournal(), NewMemNotifyTrail()
		opts := []NotifyOption{WithNotifyReceiver("boss", esc),
			WithNotifyEscalation(NotificationApprovalPending, EscalationStep{Receiver: "boss", Deadline: time.Minute})}
		a := testEngine(t, j, def, trail, clk, "p1", opts...)
		b := testEngine(t, j, def, trail, clk, "p2", opts...)
		ev := approvalEvent("alice")
		_ = a.roundRouted(context.Background(), ev) // observed, never acked
		clk.advance(10 * time.Minute)
		var wg sync.WaitGroup
		for _, e := range []*notifyEngine{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				base := ev
				base.receiver = ""
				_, _ = e.round(context.Background(), e.escalationOf(base, 1))
			}()
		}
		wg.Wait()
		n := 0
		for _, r := range trailOf(t, trail, "r", EscalationKey(ev.key, 1)) {
			if r.Kind == NotifyObserved {
				n++
			}
		}
		if n != 1 || esc.count() > 1 {
			t.Fatalf("iteration %d: %d observations of the step, %d deliveries", i, n, esc.count())
		}
	}
}

type inflightSink struct {
	fakeSink
	in, release chan struct{}
	once        sync.Once
}

func (g *inflightSink) Notify(ctx context.Context, n Notification) error {
	if n.Kind != NotificationResolved {
		g.once.Do(func() { close(g.in); <-g.release })
	}
	return g.fakeSink.Notify(ctx, n)
}

// an event is resolved while its delivery is in flight and a second process scans. The
// trail must stay a valid history (nothing follows a clearing or an acknowledgement) and the
// receiver must end with exactly one all-clear.
func TestNotifyRace_AllClearWhileDeliveryInFlight(t *testing.T) {
	ctx := context.Background()
	clk := resolvedClock()
	j, trail := memJournal(), NewMemNotifyTrail()
	sink := &inflightSink{in: make(chan struct{}), release: make(chan struct{})}
	a := testEngine(t, j, sink, trail, clk, "p1")
	b := testEngine(t, j, sink, trail, clk, "p2")
	ev := haltEvent()
	done := make(chan error, 1)
	go func() { _, err := a.round(ctx, ev); done <- err }()
	<-sink.in
	putResult(t, j, "r", "c1")
	clk.advance(time.Minute)
	_, _ = b.round(ctx, notifyEvent{key: scanKey("r"), scan: true, runID: "r"})
	_ = b.flush(ctx)
	close(sink.release)
	if err := <-done; err != nil {
		t.Logf("delivery round: %v", err)
	}
	clk.advance(time.Minute)
	for _, e := range []*notifyEngine{a, b} {
		_, _ = e.round(ctx, notifyEvent{key: scanKey("r"), scan: true, runID: "r"})
		_ = e.flush(ctx)
	}
	var kinds []NotifyRecordKind
	for _, r := range trailOf(t, trail, "r", ev.key) {
		kinds = append(kinds, r.Kind)
	}
	closed := 0
	for i, k := range kinds {
		if k == NotifyAcked || k == NotifyCleared {
			closed++
			if i != len(kinds)-1 {
				t.Errorf("records follow a %s: %v", k, kinds)
			}
		}
	}
	if closed > 1 {
		t.Errorf("event both acked and cleared: %v", kinds)
	}
	if n := len(resolvedCalls(&sink.fakeSink)); n != 1 {
		t.Errorf("%d all-clears delivered, want 1 (trail %v)", n, kinds)
	}
}

// When the pending approval's gate expired, the all-clear's reason is "expired", and an
// approval_expired event is sent once, keyed by the run and the call.
func TestResolved_ExpiredGate(t *testing.T) {
	ctx := context.Background()
	gate := notifyEvent{key: ApprovalKey("r", "c1", 0), kind: NotificationApprovalPending, runID: "r", opRun: "r",
		approval: &NotifyApproval{ToolUseID: "c1", ToolName: "refund"}}
	for name, expire := range map[string]func(t *testing.T, j *Journal){
		"single": func(t *testing.T, j *Journal) {
			writeRaw(t, j, "r", approvalStep("c1"), Record{Kind: StepApproval, ToolUseID: "c1", Result: json.RawMessage(expiredDecision)})
		},
		"m-of-n": func(t *testing.T, j *Journal) {
			b, _ := json.Marshal(ApprovalTally{Need: 2, Approvers: abc, Expired: true})
			writeRaw(t, j, "r", ApprovalTallyStep("c1"), Record{Kind: StepValue, Result: b})
		},
	} {
		t.Run(name, func(t *testing.T) {
			j := memJournal()
			trail, sink := NewMemNotifyTrail(), &fakeSink{}
			e := testEngine(t, j, sink, trail, resolvedClock(), "p1")
			if res, err := e.round(ctx, gate); err != nil || !res.done {
				t.Fatal(res, err)
			}
			expire(t, j)
			scanAndFlush(t, e, "r")
			scanAndFlush(t, e, "r")
			got := resolvedCalls(sink)
			if len(got) != 1 || got[0].Reason != "expired" {
				t.Fatalf("all-clears = %+v; want one, reason expired", got)
			}
			var expired []Notification
			for _, c := range sink.calls {
				if c.Kind == NotificationApprovalExpired {
					expired = append(expired, c)
				}
			}
			if len(expired) != 1 || expired[0].Key != ExpiredKey("r", "c1") || expired[0].Approval == nil || expired[0].Approval.ToolUseID != "c1" {
				t.Fatalf("approval_expired deliveries = %+v; want one for c1", expired)
			}
		})
	}
}

// TestNotifyBudget_RoundCoversBothAcks checks roundBudget counts the overdue notice's
// acknowledgement as well as the event's (Model 14 finding N3): a round that starts under a claim
// with more than roundBudget left ends inside it, and a fresh claim always has that much.
func TestNotifyBudget_RoundCoversBothAcks(t *testing.T) {
	for _, ttl := range []time.Duration{30 * time.Second, 2 * time.Minute, 16 * time.Minute, time.Hour} {
		e := &notifyEngine{cfg: defaultNotifyConfig()}
		e.cfg.claimTTL = ttl
		worst := time.Duration(e.cfg.roundAttempts+1)*e.callTimeout() +
			e.cfg.attemptBackoff*time.Duration(1<<(e.cfg.roundAttempts-1)-1) +
			ttl/8 + ttl/8 // the event's acknowledgement, then the notice's
		if got := e.roundBudget(); got < worst {
			t.Errorf("ttl %v: roundBudget %v is less than a round's worst case %v", ttl, got, worst)
		}
		if got := e.roundBudget(); got >= ttl {
			t.Errorf("ttl %v: roundBudget %v does not fit inside a fresh claim", ttl, got)
		}
	}
}

// TestNotifyBudget_NoticeClaimCoversItsCall checks a peer's notice claim outlasts the notice's call
// and its acknowledgement (Model 14 finding N2), so another peer cannot claim and resend it while
// the first call is still running, and never outlasts the event's own claim.
func TestNotifyBudget_NoticeClaimCoversItsCall(t *testing.T) {
	for _, ttl := range []time.Duration{30 * time.Second, 2 * time.Minute, 16 * time.Minute, time.Hour} {
		e := &notifyEngine{cfg: defaultNotifyConfig()}
		e.cfg.claimTTL = ttl
		got := e.noticeClaimTTL()
		if need := e.callTimeout() + ttl/8; got < need && got != ttl {
			t.Errorf("ttl %v: notice claim %v is shorter than its call and ack %v", ttl, got, need)
		}
		if got > ttl {
			t.Errorf("ttl %v: notice claim %v outlasts the event's claim", ttl, got)
		}
	}
}

// A saga's finished rollback records when the run aborted, by the drive's clock, beside why.
func TestAbort_RecordsWhenTheRunAborted(t *testing.T) {
	ctx := context.Background()
	l := newLedger()
	store := memJournal()
	a := mustNew(&scriptModel{turns: [][]Emit{toolTurn("a1", "A", `{}`), toolTurn("a2", "boom", `{}`)}}, store, WithTools(l.write("A"), failTool("boom")))
	at := time.Date(2026, 10, 13, 15, 4, 5, 678_000_000, time.UTC)
	if _, err := a.Run(ctx, "root", UserText("go"), WithSaga(), WithClock(func() time.Time { return at })); err == nil {
		t.Fatal("setup: the saga did not abort")
	}
	e, ok, err := store.getEntry(ctx, "root", runAbortedStep)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	rec, err := decodeStored("root", runAbortedStep, e.Data)
	if err != nil {
		t.Fatal(err)
	}
	got, has := abortedAt(rec)
	if !has || !got.Equal(at) {
		t.Fatalf("aborted_at = %v, %v; want %v", got, has, at)
	}
	st, err := Status(ctx, store, "root")
	if err != nil || st.State != RunAborted || st.Terminal == "" {
		t.Fatalf("Status = %+v, %v: the reason must still read as the terminal text", st, err)
	}
}

// aborted_at is informational: a run:aborted with the time, without it (an earlier version's, the
// reason as a string), or with another time behaves the same for Status, a drive and a recovery pass.
func TestAbort_TheTimeIsReadByNoDrive(t *testing.T) {
	ctx := context.Background()
	variants := map[string]json.RawMessage{
		"with a time":            mustJSONValue(abortValue{Reason: "refund failed", AbortedAt: "2026-10-13T09:00:00.000Z"}),
		"another time":           mustJSONValue(abortValue{Reason: "refund failed", AbortedAt: "1999-01-01T00:00:00.000Z"}),
		"a time that is no time": mustJSONValue(abortValue{Reason: "refund failed", AbortedAt: "yesterday"}),
		"no time":                mustJSONValue(abortValue{Reason: "refund failed"}),
		"an earlier version's":   mustJSON("refund failed"),
	}
	type outcome struct {
		state    RunState
		terminal string
		runErr   string
		recover  int
	}
	var first *outcome
	var firstName string
	for name, value := range variants {
		j := memJournal()
		start, _ := json.Marshal(RunStart{Input: UserText("go"), Saga: true})
		if _, _, err := j.putEntry(ctx, "r", runStartStep, Record{Kind: StepValue, Result: start}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := j.putEntry(ctx, "r", runAbortedStep, Record{Kind: StepValue, Result: value}); err != nil {
			t.Fatal(err)
		}
		st, err := Status(ctx, j, "r")
		if err != nil {
			t.Fatal(name, err)
		}
		a := mustNew(NewScriptedModel(), j)
		_, runErr := a.Resume(ctx, "r")
		n, rerr := Recover(ctx, j, func(context.Context, string, RunStart) error { return nil })
		if rerr != nil {
			t.Fatal(name, rerr)
		}
		o := &outcome{st.State, st.Terminal, fmtErr(runErr), n}
		if first == nil {
			first, firstName = o, name
		} else if *o != *first {
			t.Fatalf("%q behaves as %+v but %q as %+v: the time was read", name, *o, firstName, *first)
		}
	}
	if first.state != RunAborted || first.terminal != "refund failed" || first.recover != 0 {
		t.Fatalf("outcome = %+v", *first)
	}
}

func fmtErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// The sweep dates an abort by its aborted_at, whatever the run's start says; for a run an earlier
// version aborted, by the newest time the journal records (an attempt marker, else the start).
func TestNotifyPending_AbortedRunsAreDatedByTheAbortThenByTheNewestRecord(t *testing.T) {
	ctx := context.Background()
	clk := sweepClock()
	now := clk.now()
	j := memJournal()
	abort := func(run string, v json.RawMessage) {
		t.Helper()
		if _, _, err := j.putEntry(ctx, run, runAbortedStep, Record{Kind: StepValue, Result: v}); err != nil {
			t.Fatal(err)
		}
	}
	stamp := func(d time.Duration) string { return now.Add(-d).UTC().Format(time.RFC3339Nano) }
	// aborted_at says recent although the run started three days ago: caught up.
	putStart(t, j, "long-recent", now.Add(-72*time.Hour))
	abort("long-recent", mustJSONValue(abortValue{Reason: "x", AbortedAt: stamp(time.Minute)}))
	// aborted_at says old although the run started a minute ago (clocks differ): not caught up.
	putStart(t, j, "start-recent-abort-old", now.Add(-time.Minute))
	abort("start-recent-abort-old", mustJSONValue(abortValue{Reason: "x", AbortedAt: stamp(48 * time.Hour)}))
	// An earlier version's abort, busy until two minutes ago: caught up by its newest attempt marker.
	putStart(t, j, "legacy-busy", now.Add(-72*time.Hour))
	putMarker(t, j, "legacy-busy", "c1", now.Add(-2*time.Minute).UnixMilli())
	abort("legacy-busy", mustJSON("x"))
	// An earlier version's abort whose newest time is old: not caught up.
	putStart(t, j, "legacy-old", now.Add(-72*time.Hour))
	putMarker(t, j, "legacy-old", "c1", now.Add(-48*time.Hour).UnixMilli())
	abort("legacy-old", mustJSON("x"))
	// An earlier version's abort of a run that records no time at all: unknown, not caught up.
	abort("legacy-untimed", mustJSON("x"))

	sink := &fakeSink{}
	if err := sweep(t, j, sink, NewMemNotifyTrail(), clk); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, c := range sink.calls {
		got[c.RunID] = true
	}
	if len(got) != 2 || !got["long-recent"] || !got["legacy-busy"] {
		t.Fatalf("caught up %v; want long-recent and legacy-busy only", got)
	}
}

func TestNotifyClock_DatesTheSweepAndRefusesNil(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	// The run aborted at 2020-01-01 by the drive's clock.
	if _, _, err := j.putEntry(ctx, "r", runAbortedStep, Record{Kind: StepValue, Result: mustJSONValue(abortValue{Reason: "x", AbortedAt: "2020-01-01T00:00:00.000Z"})}); err != nil {
		t.Fatal(err)
	}
	wall := &fakeSink{}
	if err := NotifyPending(ctx, j, wall, NewMemNotifyTrail()); err != nil || wall.count() != 0 {
		t.Fatalf("wall time: %v, %d notifications; a 2020 abort is long past", err, wall.count())
	}
	same := &fakeSink{}
	clk := func() time.Time { return time.Date(2020, 1, 1, 0, 5, 0, 0, time.UTC) }
	if err := NotifyPending(ctx, j, same, NewMemNotifyTrail(), WithNotifyClock(clk)); err != nil || same.count() != 1 {
		t.Fatalf("the drive's clock: %v, %d notifications; want the abort caught up", err, same.count())
	}
	if err := NotifyPending(ctx, j, same, NewMemNotifyTrail(), WithNotifyClock(nil)); !errors.Is(err, ErrConfig) {
		t.Fatalf("nil clock: %v", err)
	}
}

// Every notifier option with a range refuses a value outside it, at New, with ErrConfig: a notifier
// that would deliver on a zero timeout or retry forever never runs half-configured.
func TestNotifierOptions_RefuseOutOfRangeValues(t *testing.T) {
	for name, opt := range map[string]NotifyOption{
		"deadline zero":            WithNotifyDeadline(0),
		"claim ttl negative":       WithNotifyClaimTTL(-time.Second),
		"retry base zero":          WithNotifyRetry(0, time.Second),
		"retry max below base":     WithNotifyRetry(time.Minute, time.Second),
		"failing after zero":       WithNotifyFailingAfter(0),
		"recovery failing after 0": WithNotifyRecoveryFailingAfter(0),
		"recovery failing neg":     WithNotifyRecoveryFailingAfter(-time.Minute),
		"halt age negative":        WithNotifyHaltAge(-time.Second),
		"aborted within zero":      WithNotifyAbortedWithin(0),
		"holder empty":             WithNotifyHolder(""),
		"overdue notifier nil":     WithOverdueNotifier(nil),
	} {
		_, err := New(&scriptModel{}, memJournal(), WithNotifier(&fakeSink{}, NewMemNotifyTrail(), opt))
		if !errors.Is(err, ErrConfig) {
			t.Errorf("%s: New = %v; want ErrConfig", name, err)
		}
	}
	for name, opt := range map[string]NotifyOption{
		"retry equal bounds": WithNotifyRetry(time.Second, time.Second),
		"failing after one":  WithNotifyFailingAfter(1),
		"recovery 1m":        WithNotifyRecoveryFailingAfter(time.Minute),
		"holder":             WithNotifyHolder("worker-1"),
		"overdue notifier":   WithOverdueNotifier(&fakeSink{}),
	} {
		if _, err := New(&scriptModel{}, memJournal(), WithNotifier(&fakeSink{}, NewMemNotifyTrail(), opt)); err != nil {
			t.Errorf("%s: New = %v", name, err)
		}
	}
}

// WithNotifier itself refuses a nil sink or trail, and an agent with no notifier has nothing to flush.
func TestNotifier_NilPartsAndFlushWithoutNotifier(t *testing.T) {
	if _, err := New(&scriptModel{}, memJournal(), WithNotifier(nil, NewMemNotifyTrail())); !errors.Is(err, ErrConfig) {
		t.Errorf("nil sink = %v; want ErrConfig", err)
	}
	if _, err := New(&scriptModel{}, memJournal(), WithNotifier(&fakeSink{}, nil)); !errors.Is(err, ErrConfig) {
		t.Errorf("nil trail = %v; want ErrConfig", err)
	}
	a := mustNew(&scriptModel{}, memJournal())
	if err := a.FlushNotifications(context.Background()); err != nil {
		t.Errorf("FlushNotifications without a notifier = %v", err)
	}
}

// NotifyPending refuses a nil journal, sink or trail, and a store that cannot list runs, rather
// than sweeping nothing.
func TestNotifyPending_Refusals(t *testing.T) {
	ctx := context.Background()
	sink, trail := &fakeSink{}, NewMemNotifyTrail()
	if err := NotifyPending(ctx, nil, sink, trail); !errors.Is(err, ErrConfig) {
		t.Errorf("nil journal = %v; want ErrConfig", err)
	}
	if err := NotifyPending(ctx, memJournal(), nil, trail); !errors.Is(err, ErrConfig) {
		t.Errorf("nil sink = %v; want ErrConfig", err)
	}
	if err := NotifyPending(ctx, memJournal(), sink, nil); !errors.Is(err, ErrConfig) {
		t.Errorf("nil trail = %v; want ErrConfig", err)
	}
	if err := NotifyPending(ctx, mustJournal(plainStore{NewMemStore()}), sink, trail); !errors.Is(err, ErrConfig) {
		t.Errorf("a store with no Lister = %v; want ErrConfig", err)
	}
}

// covRunFault is a MemStore whose Load fails for one run, and whose Runs yields an error after its
// runs when runsErr is set.
type covRunFault struct {
	*MemStore
	badRun  string
	runsErr bool
}

func (s covRunFault) Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error] {
	if runID == s.badRun {
		return func(yield func(Entry, error) bool) { yield(Entry{}, errCovBlip) }
	}
	return s.MemStore.Load(ctx, runID, after)
}

func (s covRunFault) Runs(ctx context.Context, f RunFilter) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		for id, err := range s.MemStore.Runs(ctx, f) {
			if !yield(id, err) {
				return
			}
		}
		if s.runsErr {
			yield("", errCovBlip)
		}
	}
}

// A sweep reports a run it cannot read and goes on: the other runs' gates are still notified, and
// the failure comes back joined (a listing failure as ErrStorage), so one bad run never hides the
// rest.
func TestNotifyPending_ContinuesPastAnUnreadableRun(t *testing.T) {
	ctx := context.Background()
	clk := sweepClock()
	s := covRunFault{MemStore: NewMemStore(), badRun: "a-bad", runsErr: true}
	j := mustJournal(s)
	putGate(t, j, "z-good", ApprovalWait{ToolUseID: "c1", ToolName: "refund", Single: true})
	// The bad run exists (its header is in the store), but its entries cannot be loaded.
	if _, _, err := s.MemStore.Insert(ctx, "a-bad", headerStep, headerEntry()); err != nil {
		t.Fatal(err)
	}
	sink := &fakeSink{}
	err := sweep(t, j, sink, NewMemNotifyTrail(), clk)
	if err == nil || !errors.Is(err, ErrStorage) {
		t.Fatalf("sweep = %v; want the joined failures including ErrStorage", err)
	}
	if sink.count() != 1 || sink.calls[0].RunID != "z-good" {
		t.Fatalf("the readable run was not notified: %+v", sink.calls)
	}
}

// A sweep whose context is cancelled stops and says so, rather than reporting success.
func TestNotifyPending_StopsWhenCancelled(t *testing.T) {
	j := memJournal()
	putGate(t, j, "r", ApprovalWait{ToolUseID: "c1", ToolName: "refund", Single: true})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sink := &fakeSink{}
	err := NotifyPending(ctx, j, sink, NewMemNotifyTrail(), fixedNotifyClock(sweepClock()), WithNotifyHolder("sweeper"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("sweep on a cancelled context = %v; want context.Canceled", err)
	}
	if sink.count() != 0 {
		t.Fatalf("a cancelled sweep notified %d times", sink.count())
	}
}

// covTrail is a MemNotifyTrail whose Load or Append can be made to fail, or to lose a race once.
type covTrail struct {
	*MemNotifyTrail
	mu          sync.Mutex
	loadErr     error
	appendErr   error
	conflicts   int // Appends that report ErrTrailConflict before one succeeds
	appendCalls int
}

func (t *covTrail) Load(ctx context.Context, name string) ([]NotifyRecord, error) {
	t.mu.Lock()
	err := t.loadErr
	t.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return t.MemNotifyTrail.Load(ctx, name)
}

func (t *covTrail) Append(ctx context.Context, name string, seq int, rec NotifyRecord) error {
	t.mu.Lock()
	t.appendCalls++
	err, conflict := t.appendErr, t.conflicts > 0
	if conflict {
		t.conflicts--
	}
	t.mu.Unlock()
	switch {
	case err != nil:
		return err
	case conflict:
		return ErrTrailConflict
	}
	return t.MemNotifyTrail.Append(ctx, name, seq, rec)
}

// errCollector is a WithNotifyErrors callback that keeps what it is given.
type errCollector struct {
	mu   sync.Mutex
	errs []error
}

func (c *errCollector) add(err error) { c.mu.Lock(); c.errs = append(c.errs, err); c.mu.Unlock() }
func (c *errCollector) n() int        { c.mu.Lock(); defer c.mu.Unlock(); return len(c.errs) }

// A recovery pass that cannot read or write the notification trail reports the fault to the error
// hook and goes on: it never fails the pass, and it never writes a half record.
func TestNotifier_RecoveryTrailFaultsAreReportedNotFatal(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		set  func(*covTrail)
		call func(*notifyEngine)
	}{
		"failed: load":  {func(t *covTrail) { t.loadErr = errCovBlip }, func(e *notifyEngine) { e.recoveryFailed(ctx, "r", errCovBlip) }},
		"failed: write": {func(t *covTrail) { t.appendErr = errCovBlip }, func(e *notifyEngine) { e.recoveryFailed(ctx, "r", errCovBlip) }},
		"ok: load":      {func(t *covTrail) { t.loadErr = errCovBlip }, func(e *notifyEngine) { e.recoveryOK(ctx, "r") }},
	} {
		t.Run(name, func(t *testing.T) {
			tr := &covTrail{MemNotifyTrail: NewMemNotifyTrail()}
			col := &errCollector{}
			e := testEngine(t, memJournal(), &fakeSink{}, tr, &notifyClock{t: time.Now()}, "p", WithNotifyErrors(col.add))
			tc.set(tr)
			tc.call(e)
			if col.n() != 1 || !errors.Is(col.errs[0], errCovBlip) {
				t.Fatalf("reported %v; want the one fault", col.errs)
			}
		})
	}
	t.Run("ok: write", func(t *testing.T) {
		tr := &covTrail{MemNotifyTrail: NewMemNotifyTrail()}
		col := &errCollector{}
		e := testEngine(t, memJournal(), &fakeSink{}, tr, &notifyClock{t: time.Now()}, "p", WithNotifyErrors(col.add))
		e.recoveryFailed(ctx, "r", errCovBlip) // opens the failure episode
		tr.mu.Lock()
		tr.appendErr = errCovBlip
		tr.mu.Unlock()
		e.recoveryOK(ctx, "r") // cannot record the recovery
		if col.n() != 1 || !errors.Is(col.errs[0], errCovBlip) {
			t.Fatalf("reported %v; want the one fault", col.errs)
		}
	})
}

// Losing the append race while opening or closing a failure episode retries on the fresh trail
// and records the episode once: two processes recovering the same run write one episode.
func TestNotifier_RecoveryEpisodeSurvivesALostAppendRace(t *testing.T) {
	ctx := context.Background()
	tr := &covTrail{MemNotifyTrail: NewMemNotifyTrail(), conflicts: 2}
	col := &errCollector{}
	e := testEngine(t, memJournal(), &fakeSink{}, tr, &notifyClock{t: time.Now()}, "p", WithNotifyErrors(col.add))
	e.recoveryFailed(ctx, "r", errCovBlip)
	recs, _ := tr.MemNotifyTrail.Load(ctx, "notify:r")
	if len(recs) != 1 || recs[0].Kind != NotifyFailingSince || tr.appendCalls != 3 {
		t.Fatalf("after two lost races: %v, %d appends; want one failing_since record after 3 tries", recKinds(recs), tr.appendCalls)
	}
	tr.mu.Lock()
	tr.conflicts, tr.appendCalls = 1, 0
	tr.mu.Unlock()
	e.recoveryOK(ctx, "r")
	recs, _ = tr.MemNotifyTrail.Load(ctx, "notify:r")
	if len(recs) != 2 || recs[1].Kind != NotifyRecovered || tr.appendCalls != 2 {
		t.Fatalf("after a lost race on recovery: %v, %d appends; want one recovered record after 2 tries", recKinds(recs), tr.appendCalls)
	}
	if col.n() != 0 {
		t.Fatalf("a lost race was reported as a fault: %v", col.errs)
	}
}

// What tells one halt of an operation from a later one is the halt's attempt time when it carries
// one, and otherwise the live attempt's generation (negated, less one): the first attempt of an
// untimed marker and each re-attempt after a not-started record are different events, for a tool
// call and for a Step alike. A marker that cannot be read is an error, not a guessed key.
func TestNotifier_HaltAttemptDistinguishesGenerations(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	j := memJournal()
	putMarker(t, j, "r", "c1", 0)
	if _, err := j.do(ctx, "r", stepAttemptStep("s1"), func(context.Context) (Record, error) {
		return Record{Kind: StepAttempt, ToolUseID: "s1"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	// c2: the first attempt is recorded as not started, so the live one is the first re-attempt.
	rec := Record{Kind: StepAttempt, ToolUseID: "c2"}
	won, marker, key, err := j.claimNext(ctx, "r", toolAttemptStep("c2"), rec)
	if err != nil || !won {
		t.Fatal(won, err)
	}
	if err := j.notStarted(ctx, "r", key, marker); err != nil {
		t.Fatal(err)
	}
	if won, _, _, err := j.claimNext(ctx, "r", toolAttemptStep("c2"), rec); err != nil || !won {
		t.Fatal(won, err)
	}
	e := testEngine(t, j, &fakeSink{}, NewMemNotifyTrail(), &notifyClock{t: time.Now()}, "p")
	for name, c := range map[string]struct {
		halt *OutcomeUnknown
		want int64
	}{
		"time on the halt":        {&OutcomeUnknown{RunRef: RunRef{RunID: "r"}, Op: OpRef{Kind: OpTool, ID: "x"}, AttemptedAt: at}, at.UnixMilli()},
		"untimed tool marker":     {&OutcomeUnknown{RunRef: RunRef{RunID: "r"}, Op: OpRef{Kind: OpTool, ID: "c1"}}, -1},
		"untimed step marker":     {&OutcomeUnknown{RunRef: RunRef{RunID: "r"}, Op: OpRef{Kind: OpStep, ID: "s1"}}, -1},
		"after a not-started one": {&OutcomeUnknown{RunRef: RunRef{RunID: "r"}, Op: OpRef{Kind: OpTool, ID: "c2"}}, -2},
	} {
		got, err := e.haltAttempt(ctx, c.halt)
		if err != nil || got != c.want {
			t.Errorf("%s: haltAttempt = %d, %v; want %d", name, got, err, c.want)
		}
	}
	bad := testEngine(t, mustJournal(covGetFail{NewMemStore(), "attempt:"}), &fakeSink{}, NewMemNotifyTrail(), &notifyClock{t: time.Now()}, "p")
	if _, err := bad.haltAttempt(ctx, &OutcomeUnknown{RunRef: RunRef{RunID: "r"}, Op: OpRef{Kind: OpTool, ID: "c1"}}); err == nil {
		t.Error("haltAttempt over an unreadable marker succeeded")
	}
}

// A round that cannot read or write the trail, or cannot tell whether its event still stands,
// returns the fault with the time to try again, and sends nothing: an alert is never delivered
// on a state the engine could not record or read.
func TestNotifyRound_FaultsSendNothingAndSayWhenToRetry(t *testing.T) {
	ctx := context.Background()
	clk := &notifyClock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	for name, c := range map[string]struct {
		trail func(*covTrail)
		store func(*MemStore) Store
	}{
		"trail unreadable":       {func(t *covTrail) { t.loadErr = errCovBlip }, nil},
		"observation unwritable": {func(t *covTrail) { t.appendErr = errCovBlip }, nil},
		"run state unreadable":   {nil, func(s *MemStore) Store { return loadFails{s} }},
	} {
		t.Run(name, func(t *testing.T) {
			tr := &covTrail{MemNotifyTrail: NewMemNotifyTrail()}
			if c.trail != nil {
				c.trail(tr)
			}
			var st Store = NewMemStore()
			if c.store != nil {
				st = c.store(NewMemStore())
			}
			sink := &fakeSink{}
			e := testEngine(t, mustJournal(st), sink, tr, clk, "p1")
			res, err := e.round(ctx, haltEvent())
			if err == nil || res.done || !res.next.After(clk.now()) {
				t.Fatalf("round = %+v, %v; want a fault, not done, and a retry time in the future", res, err)
			}
			if sink.count() != 0 {
				t.Fatalf("an alert was sent despite the fault: %d", sink.count())
			}
		})
	}
}
