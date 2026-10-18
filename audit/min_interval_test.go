package audit_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/journaltest"
)

// manualClock is a clock and a set of timers a test advances by hand.
type manualClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*manualTimer
}

type manualTimer struct {
	at      time.Time
	f       func()
	stopped bool
}

func newManualClock() *manualClock { return &manualClock{now: time.Unix(1_800_000_000, 0)} }

func (c *manualClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *manualClock) After(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &manualTimer{at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		was := !t.stopped
		t.stopped = true
		return was
	}
}

// Advance moves the clock and runs every timer that falls due, in order, as it passes.
func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var next *manualTimer
		for _, t := range c.timers {
			if !t.stopped && !t.at.After(target) && (next == nil || t.at.Before(next.at)) {
				next = t
			}
		}
		if next == nil {
			c.now = target
			c.mu.Unlock()
			return
		}
		next.stopped = true
		if next.at.After(c.now) {
			c.now = next.at
		}
		c.mu.Unlock()
		next.f()
	}
}

// timedAnchor records each head with the clock's time when it arrived.
type timedAnchor struct {
	clk   *manualClock
	mu    sync.Mutex
	heads []audit.SignedTreeHead
	at    []time.Time
	fail  error
}

func (a *timedAnchor) Publish(_ context.Context, runID string, sth audit.SignedTreeHead) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fail != nil {
		return a.fail
	}
	a.heads = append(a.heads, sth)
	a.at = append(a.at, a.clk.Now())
	return nil
}

func (a *timedAnchor) sizes() []int {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []int
	for _, h := range a.heads {
		out = append(out, h.Size)
	}
	return out
}

type intervalRig struct {
	clk    *manualClock
	anchor *timedAnchor
	store  *audit.AuditedStore
	j      *agent.Journal
	errs   []error
	steps  int
}

func newIntervalRig(t *testing.T, interval time.Duration) *intervalRig {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(nil)
	r := &intervalRig{clk: newManualClock()}
	r.anchor = &timedAnchor{clk: r.clk}
	r.store = mustAuditedStore(t, agenttest.MemJournal(), priv, r.anchor).WithMinInterval(interval)
	r.store.SetClock(r.clk.Now, r.clk.After)
	r.store.OnError(func(_ string, err error) { r.errs = append(r.errs, err) })
	r.j = agenttest.MustJournal(r.store)
	return r
}

func (r *intervalRig) write(t *testing.T, name string) {
	t.Helper()
	if _, err := journaltest.Do(context.Background(), r.j, "run-1", name, func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue}, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMinInterval_HoldsBackGrowthAndPublishesItOnTheTimer(t *testing.T) {
	r := newIntervalRig(t, 10*time.Second)
	r.write(t, "a") // the run's first head: at once
	first := r.anchor.sizes()
	if len(first) != 1 {
		t.Fatalf("heads after the first write = %v, want one", first)
	}
	for _, n := range []string{"b", "c", "d"} {
		r.clk.Advance(time.Second)
		r.write(t, n)
	}
	if got := r.anchor.sizes(); !slices.Equal(got, first) {
		t.Fatalf("heads inside the interval = %v, want only %v", got, first)
	}
	if p := r.store.Pending(); !slices.Equal(p, []string{"run-1"}) {
		t.Fatalf("Pending = %v, want the run", p)
	}
	r.clk.Advance(5 * time.Second) // 8 s since the head: the timer is not due
	if len(r.anchor.sizes()) != 1 {
		t.Fatal("the timer fired early")
	}
	r.clk.Advance(2 * time.Second) // 10 s: due
	got := r.anchor.sizes()
	if len(got) != 2 || got[1] <= got[0] {
		t.Fatalf("heads after the timer = %v, want one more, larger head covering the held-back writes", got)
	}
	if p := r.store.Pending(); len(p) != 0 {
		t.Fatalf("Pending after the timer = %v", p)
	}
	r.clk.Advance(time.Hour) // nothing left to publish
	if len(r.anchor.sizes()) != 2 {
		t.Fatal("a second timer published again")
	}
	if len(r.errs) != 0 {
		t.Fatal(r.errs)
	}
}

// The window: with writes at arbitrary times and a process that lives, every record is covered by a
// published head within the interval, because the timer publishes what no later write does.
func TestMinInterval_BoundsTheUncoveredWindow(t *testing.T) {
	const interval = 10 * time.Second
	r := newIntervalRig(t, interval)
	type write struct {
		size int
		at   time.Time
	}
	var writes []write
	gaps := []time.Duration{0, 1, 3, 12, 2, 2, 25, 1, 9, 9, 9, 40, 1}
	for i, g := range gaps {
		r.clk.Advance(g * time.Second)
		r.write(t, string(rune('a'+i)))
		recs, _ := r.j.History(context.Background(), "run-1")
		writes = append(writes, write{len(recs), r.clk.Now()})
	}
	r.clk.Advance(interval) // the last write's timer
	for _, w := range writes {
		covered := false
		for i, h := range r.anchor.heads {
			if h.Size >= w.size {
				if d := r.anchor.at[i].Sub(w.at); d > interval {
					t.Errorf("a record of size %d written at +%v was covered after %v, over the interval %v", w.size, w.at.Sub(r.clk.now), d, interval)
				}
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("a record of size %d was never covered", w.size)
		}
	}
	// Heads only grow, and there are fewer than one per write.
	got := r.anchor.sizes()
	if !slices.IsSorted(got) || len(got) >= len(writes) {
		t.Errorf("heads %v for %d writes: want growing, and fewer than one per write", got, len(writes))
	}
}

func TestMinInterval_RunEndReanchorAndFlushAreImmediate(t *testing.T) {
	for _, tc := range []struct {
		name string
		act  func(r *intervalRig, t *testing.T)
	}{
		{"run:complete", func(r *intervalRig, t *testing.T) { r.write(t, "run:complete") }},
		{"run:aborted", func(r *intervalRig, t *testing.T) { r.write(t, "run:aborted") }},
		{"Reanchor", func(r *intervalRig, t *testing.T) {
			r.write(t, "b")
			if err := r.store.Reanchor(context.Background(), "run-1"); err != nil {
				t.Fatal(err)
			}
		}},
		{"Flush", func(r *intervalRig, t *testing.T) {
			r.write(t, "b")
			if err := r.store.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newIntervalRig(t, time.Hour)
			r.write(t, "a")
			before := len(r.anchor.sizes())
			tc.act(r, t)
			recs, _ := r.j.History(context.Background(), "run-1")
			got := r.anchor.sizes()
			if len(got) <= before || got[len(got)-1] != len(recs) {
				t.Fatalf("heads %v after %s, want one covering all %d records at once", got, tc.name, len(recs))
			}
			if p := r.store.Pending(); len(p) != 0 {
				t.Errorf("Pending = %v after %s", p, tc.name)
			}
			r.clk.Advance(2 * time.Hour)
			if len(r.anchor.sizes()) != len(got) {
				t.Error("a disarmed timer published again")
			}
		})
	}
}

func TestMinInterval_ATimerFailureIsReportedAndTheNextWriteRecovers(t *testing.T) {
	r := newIntervalRig(t, 10*time.Second)
	r.write(t, "a")
	r.write(t, "b")
	r.anchor.mu.Lock()
	r.anchor.fail = errors.New("anchor down")
	r.anchor.mu.Unlock()
	r.clk.Advance(10 * time.Second)
	if len(r.errs) != 1 {
		t.Fatalf("errors = %v, want the timer's failure reported once", r.errs)
	}
	if p := r.store.Pending(); len(p) != 1 {
		t.Fatalf("Pending = %v after the timer failed: the run must be re-armed, so Flush and the next interval retry it", p)
	}
	r.anchor.mu.Lock()
	r.anchor.fail = nil
	r.anchor.mu.Unlock()
	r.clk.Advance(time.Second)
	r.write(t, "c") // the interval has long passed: this write publishes everything
	recs, _ := r.j.History(context.Background(), "run-1")
	if got := r.anchor.sizes(); got[len(got)-1] != len(recs) {
		t.Fatalf("heads %v, want the last to cover all %d records", got, len(recs))
	}
}

func TestMinInterval_OffByDefaultAndForNonPositive(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		r := newIntervalRig(t, d)
		for _, n := range []string{"a", "b", "c"} {
			r.write(t, n)
		}
		if got := r.anchor.sizes(); len(got) < 3 {
			t.Errorf("interval %v: heads %v, want one per write", d, got)
		}
	}
}

func TestMinInterval_RunsAreIndependent(t *testing.T) {
	r := newIntervalRig(t, time.Hour)
	r.write(t, "a")
	if _, err := journaltest.Do(context.Background(), r.j, "run-2", "a", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(r.anchor.sizes()) != 2 {
		t.Fatalf("heads %v: a second run's first head must not wait on the first run's interval", r.anchor.sizes())
	}
}

// --- the event store ---

type eventRig struct {
	clk    *manualClock
	anchor *timedAnchor
	store  *audit.AuditedEventStore
	trail  *audit.NotifyTrail
	seq    int
	errs   chan error
}

func newEventRig(t *testing.T, interval time.Duration) *eventRig {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(nil)
	r := &eventRig{clk: newManualClock(), errs: make(chan error, 16)}
	r.anchor = &timedAnchor{clk: r.clk}
	s, err := audit.NewAuditedEventStore(audit.NewMemEventStore(), edS(priv), r.anchor)
	if err != nil {
		t.Fatal(err)
	}
	r.store = s.WithMinInterval(interval).WithCooldown(0).OnError(func(_ string, err error) { r.errs <- err })
	r.store.SetClock(r.clk.Now, r.clk.After)
	r.trail = audit.NewNotifyTrail(r.store)
	return r
}

func (r *eventRig) appendN(t *testing.T, n int) {
	t.Helper()
	for range n {
		rec := agent.NotifyRecord{Kind: agent.NotifyObserved, Key: "k", At: time.Now()}
		if err := r.trail.Append(context.Background(), "notify:t", r.seq, rec); err != nil {
			t.Fatal(err)
		}
		r.seq++
	}
}

// settle waits until the background publisher has handled the appends so far and, when held
// is true, the trail is held back.
func (r *eventRig) settle(t *testing.T, held bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if held && len(r.store.HeldBack()) == 1 {
			return
		}
		if !held && len(r.anchor.sizes()) > 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("publisher did not settle (held %v, heads %v)", held, r.anchor.sizes())
}

func TestMinInterval_EventTrailHoldsBackAndTimerPublishes(t *testing.T) {
	r := newEventRig(t, 10*time.Second)
	r.appendN(t, 1)
	r.settle(t, false)
	if err := r.store.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := r.anchor.sizes(); len(got) != 1 {
		t.Fatalf("heads = %v, want the first head at once", got)
	}
	r.clk.Advance(time.Second)
	r.appendN(t, 3)
	r.settle(t, true)
	if got := r.anchor.sizes(); len(got) != 1 {
		t.Fatalf("heads inside the interval = %v", got)
	}
	r.clk.Advance(9 * time.Second) // 10 s since the head
	got := r.anchor.sizes()
	if len(got) != 2 || got[1] != 4 {
		t.Fatalf("heads after the timer = %v, want a second head of size 4", got)
	}
	if h := r.store.HeldBack(); len(h) != 0 {
		t.Fatalf("still held: %v", h)
	}
}

func TestMinInterval_EventTrailFlushReanchorAndSweepAreImmediate(t *testing.T) {
	for name, act := range map[string]func(r *eventRig) error{
		"Flush":           func(r *eventRig) error { return r.store.Flush(context.Background()) },
		"Reanchor":        func(r *eventRig) error { return r.store.Reanchor(context.Background(), "notify:t") },
		"ReanchorPending": func(r *eventRig) error { return r.store.ReanchorPending(context.Background()) },
		"AnchorTrails":    func(r *eventRig) error { return r.trail.AnchorTrails(context.Background(), []string{"notify:t"}) },
	} {
		t.Run(name, func(t *testing.T) {
			r := newEventRig(t, time.Hour)
			r.appendN(t, 1)
			r.settle(t, false)
			r.store.Flush(context.Background())
			r.appendN(t, 2)
			r.settle(t, true)
			if err := act(r); err != nil {
				t.Fatal(err)
			}
			if got := r.anchor.sizes(); got[len(got)-1] != 3 {
				t.Fatalf("heads %v after %s, want one of size 3 at once", got, name)
			}
			r.clk.Advance(2 * time.Hour)
			if n := len(r.anchor.sizes()); n != 2 {
				t.Errorf("%d heads: a disarmed timer published again", n)
			}
		})
	}
}

func TestMinInterval_EventTrailATimerFailureLeavesItDisarmed(t *testing.T) {
	r := newEventRig(t, 10*time.Second)
	r.appendN(t, 1)
	r.settle(t, false)
	r.store.Flush(context.Background())
	r.appendN(t, 2)
	r.settle(t, true)
	r.anchor.mu.Lock()
	r.anchor.fail = errors.New("anchor down")
	r.anchor.mu.Unlock()
	r.clk.Advance(10 * time.Second)
	select {
	case <-r.errs:
	case <-time.After(5 * time.Second):
		t.Fatal("the timer's failure was not reported")
	}
	if h := r.store.HeldBack(); len(h) != 1 {
		t.Fatalf("HeldBack = %v after the timer failed, want the trail re-armed", h)
	}
	if u := r.store.Unanchored(); len(u) != 1 {
		t.Fatalf("Unanchored = %v, want the trail remembered for a retry", u)
	}
}

// A crash between a held-back write and its timer: the timer is gone, but the restart path anchors
// the run, so the records are not left unanchored for good.
func TestMinInterval_AfterACrashTheRestartPathAnchorsHeldBackRuns(t *testing.T) {
	ctx := context.Background()
	_, priv, _ := ed25519.GenerateKey(nil)
	inner := agent.NewMemStore()
	clk := newManualClock()
	anchorA := &timedAnchor{clk: clk}
	a, err := audit.NewAuditedStore(inner, edS(priv), anchorA)
	if err != nil {
		t.Fatal(err)
	}
	a.WithMinInterval(time.Hour).SetClock(clk.Now, clk.After)
	j := agenttest.MustJournal(a)
	for _, n := range []string{"a", "b", "c"} {
		if _, err := journaltest.Do(ctx, j, "run-1", n, func(context.Context) (agent.Record, error) { return agent.Record{Kind: agent.StepValue}, nil }); err != nil {
			t.Fatal(err)
		}
	}
	recs, _ := agenttest.MustJournal(inner).History(ctx, "run-1")
	if got := anchorA.sizes(); got[len(got)-1] == len(recs) {
		t.Fatalf("heads %v already cover all %d records: nothing was held back", got, len(recs))
	}
	// The process dies: its timer never fires, and no Flush ran. A new process opens the same store.
	anchorB := &timedAnchor{clk: clk}
	b, err := audit.NewAuditedStore(inner, edS(priv), anchorB)
	if err != nil {
		t.Fatal(err)
	}
	b.WithMinInterval(time.Hour).SetClock(clk.Now, clk.After)
	clk.Advance(2 * time.Hour) // the dead process's timer would have fired; nothing does
	if len(anchorB.sizes()) != 0 {
		t.Fatal("a new store anchored before the restart path ran")
	}
	if err := b.ReanchorRuns(ctx, agent.RunFilter{}); err != nil {
		t.Fatal(err)
	}
	if got := anchorB.sizes(); len(got) != 1 || got[0] != len(recs) {
		t.Fatalf("heads after the restart path = %v, want one covering all %d records", got, len(recs))
	}
	// A store that cannot list runs says so.
	c, err := audit.NewAuditedStore(onlyStore{inner}, edS(priv), anchorB)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ReanchorRuns(ctx, agent.RunFilter{}); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("ReanchorRuns without a Lister = %v, want ErrConfig", err)
	}
}

// onlyStore hides a store's optional capabilities.
type onlyStore struct{ agent.Store }

// A crash on the event store: the notifier's recovery pass (AnchorTrails) anchors the trail a dead
// process held back.
func TestMinInterval_AfterACrashAnchorTrailsAnchorsHeldBackTrails(t *testing.T) {
	ctx := context.Background()
	_, priv, _ := ed25519.GenerateKey(nil)
	inner := audit.NewMemEventStore()
	clk := newManualClock()
	anchorA := &timedAnchor{clk: clk}
	sa, err := audit.NewAuditedEventStore(inner, edS(priv), anchorA)
	if err != nil {
		t.Fatal(err)
	}
	sa.WithMinInterval(time.Hour).WithCooldown(0).SetClock(clk.Now, clk.After)
	trailA := audit.NewNotifyTrail(sa)
	for i := range 3 {
		if err := trailA.Append(ctx, "notify:t", i, agent.NotifyRecord{Kind: agent.NotifyObserved, Key: "k", At: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			sa.Flush(ctx)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(sa.HeldBack()) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	anchorB := &timedAnchor{clk: clk}
	sb, err := audit.NewAuditedEventStore(inner, edS(priv), anchorB)
	if err != nil {
		t.Fatal(err)
	}
	sb.WithMinInterval(time.Hour).WithCooldown(0).SetClock(clk.Now, clk.After)
	clk.Advance(2 * time.Hour) // the dead process's timer would have fired
	if err := audit.NewNotifyTrail(sb).AnchorTrails(ctx, []string{"notify:t"}); err != nil {
		t.Fatal(err)
	}
	if got := anchorB.sizes(); len(got) != 1 || got[0] != 3 {
		t.Fatalf("heads after the recovery pass = %v, want one of size 3", got)
	}
}

// After an outage the re-armed timer publishes by itself, so the bound survives the outage.
func TestMinInterval_ATimerRetriesAfterAnOutage(t *testing.T) {
	r := newIntervalRig(t, 10*time.Second)
	r.write(t, "a")
	r.write(t, "b")
	r.anchor.mu.Lock()
	r.anchor.fail = errors.New("anchor down")
	r.anchor.mu.Unlock()
	r.clk.Advance(10 * time.Second)
	r.anchor.mu.Lock()
	r.anchor.fail = nil
	r.anchor.mu.Unlock()
	r.clk.Advance(10 * time.Second)
	recs, _ := r.j.History(context.Background(), "run-1")
	if got := r.anchor.sizes(); got[len(got)-1] != len(recs) || len(r.store.Pending()) != 0 {
		t.Fatalf("heads %v pending %v, want the retry to cover all %d records", got, r.store.Pending(), len(recs))
	}
}

// A long outage reports the failure once, not on every retry.
func TestMinInterval_ARetryStreakIsReportedOnce(t *testing.T) {
	r := newIntervalRig(t, 10*time.Second)
	r.write(t, "a")
	r.write(t, "b")
	r.anchor.mu.Lock()
	r.anchor.fail = errors.New("anchor down")
	r.anchor.mu.Unlock()
	for range 5 {
		r.clk.Advance(10 * time.Second)
	}
	if len(r.errs) != 1 {
		t.Fatalf("%d reports over 5 failed retries, want 1", len(r.errs))
	}
	r.anchor.mu.Lock()
	r.anchor.fail = nil
	r.anchor.mu.Unlock()
	r.clk.Advance(10 * time.Second)
	r.write(t, "c")
	r.clk.Advance(10 * time.Second)
	r.anchor.mu.Lock()
	r.anchor.fail = errors.New("down again")
	r.anchor.mu.Unlock()
	r.write(t, "d")
	r.clk.Advance(10 * time.Second)
	if len(r.errs) != 2 {
		t.Fatalf("%d reports after a second streak, want 2", len(r.errs))
	}
}
