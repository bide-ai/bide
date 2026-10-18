package agent

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func stopSweep(t *testing.T, ctl *StopControl, sink Notifier, trail NotifyTrail, clk *notifyClock, opts ...NotifyOption) error {
	t.Helper()
	return sweep(t, ctl.j, sink, trail, clk, append([]NotifyOption{WithNotifyStops(ctl)}, opts...)...)
}

func kindsOf(s *fakeSink) []NotificationKind {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []NotificationKind
	for _, n := range s.calls {
		out = append(out, n.Kind)
	}
	return out
}

func setTool(t *testing.T, ctl *StopControl, tool string) StopEntry {
	t.Helper()
	e, err := ctl.SetStop(context.Background(), StopRequest{Scope: StopScope{Kind: StopTool, Name: tool}, Reason: "provider incident", By: "dayna", Host: "ops-1"})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// A sweep given the control notifies once of a stop that is set, however many times it runs, with the stop's
// own detail and nothing secret.
func TestStopNotify_SweepNotifiesOnceWithTheStopsDetail(t *testing.T) {
	ctl := newControl(t, NewMemStore())
	e := setTool(t, ctl, "refund")
	trail, sink, clk := NewMemNotifyTrail(), &fakeSink{}, sweepClock()
	for range 3 {
		if err := stopSweep(t, ctl, sink, trail, clk); err != nil {
			t.Fatal(err)
		}
	}
	if sink.count() != 1 {
		t.Fatalf("%d notifications over 3 sweeps, want 1: %v", sink.count(), kindsOf(sink))
	}
	n := sink.calls[0]
	s := n.Stop
	if n.Kind != NotificationStopSet || n.Key != StopSetKey(e.ID) || n.RunID != StopControlRun || s == nil ||
		s.ID != e.ID || s.Scope != (StopScope{Kind: StopTool, Name: "refund"}) || s.Reason != "provider incident" || s.By != "dayna" || s.Host != "ops-1" ||
		s.Mode != StopBasic || s.Signed || s.Compensation != CompensationAllow {
		t.Fatalf("notification %+v stop %+v", n, s)
	}
}

// Without WithNotifyStops a sweep raises nothing for a stop.
func TestStopNotify_SweepWithoutTheControlRaisesNothing(t *testing.T) {
	ctl := newControl(t, NewMemStore())
	setTool(t, ctl, "refund")
	sink := &fakeSink{}
	if err := sweep(t, ctl.j, sink, NewMemNotifyTrail(), sweepClock()); err != nil || sink.count() != 0 {
		t.Fatalf("%v, %d notifications", err, sink.count())
	}
}

// A lift notifies once, when the lift rule is met: the stop_set is ended by the all-clear (reason "lifted")
// and the lift is told as a fact of its own.
func TestStopNotify_LiftNotifiesOnceAndClearsTheStop(t *testing.T) {
	ctx := context.Background()
	ctl := newControl(t, NewMemStore())
	e := setTool(t, ctl, "refund")
	trail, sink, clk := NewMemNotifyTrail(), &fakeSink{}, sweepClock()
	if err := stopSweep(t, ctl, sink, trail, clk); err != nil {
		t.Fatal(err)
	}
	if _, err := ctl.LiftStop(ctx, e.ID, LiftRequest{By: "dayna"}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := stopSweep(t, ctl, sink, trail, clk); err != nil {
			t.Fatal(err)
		}
	}
	got := kindsOf(sink)
	slices.Sort(got)
	if want := []NotificationKind{NotificationResolved, NotificationStopLifted, NotificationStopSet}; !slices.Equal(got, want) {
		t.Fatalf("notifications %v, want one each of %v", got, want)
	}
	for _, n := range sink.calls {
		switch n.Kind {
		case NotificationResolved:
			if n.Reason != "lifted" || n.Clears == nil || n.Clears.Key != StopSetKey(e.ID) || n.Clears.Kind != NotificationStopSet {
				t.Fatalf("all-clear %+v / %+v", n, n.Clears)
			}
		case NotificationStopLifted:
			if n.Key != StopLiftedKey(e.ID) || n.Stop == nil || len(n.Stop.LiftedBy) != 1 || n.Stop.LiftedBy[0] != "dayna" {
				t.Fatalf("lifted %+v / %+v", n, n.Stop)
			}
		}
	}
}

// A stop set and lifted between two sweeps is told as a lift (the set is cleared before anyone was told
// of it, so nothing is owed for it), and a partial audited lift raises nothing.
func TestStopNotify_ASetAndLiftBetweenSweepsAndAPartialLift(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	ctl := audited(t, store, auditedPolicy(2, true))
	e, err := ctl.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopGlobal}, Reason: "incident", By: "ops1", Signer: sign("ops1")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ctl.LiftStop(ctx, e.ID, LiftRequest{By: "ops1", Signer: sign("ops1")}); err != nil {
		t.Fatal(err)
	}
	trail, sink, clk := NewMemNotifyTrail(), &fakeSink{}, sweepClock()
	if err := stopSweep(t, ctl, sink, trail, clk); err != nil {
		t.Fatal(err)
	}
	if got := kindsOf(sink); !slices.Equal(got, []NotificationKind{NotificationStopSet}) {
		t.Fatalf("after one of two lifts: %v, want only the stop_set", got)
	}
	if !sink.calls[0].Stop.Signed || sink.calls[0].Stop.Mode != StopAudited {
		t.Fatalf("an audited stop is signed: %+v", sink.calls[0].Stop)
	}
	if _, err := ctl.LiftStop(ctx, e.ID, LiftRequest{By: "risk", Signer: sign("risk")}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := stopSweep(t, ctl, sink, trail, clk); err != nil {
			t.Fatal(err)
		}
	}
	got := kindsOf(sink)
	slices.Sort(got)
	if want := []NotificationKind{NotificationResolved, NotificationStopLifted, NotificationStopSet}; !slices.Equal(got, want) {
		t.Fatalf("after the quorum: %v, want %v (one lift notification)", got, want)
	}
}

// A stop the control rejects (a forged record) raises nothing: only accepted stops are news.
func TestStopNotify_ARejectedRecordRaisesNothing(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	real := audited(t, store, auditedPolicy(1, false))
	evil := audited(t, store, StopPolicy{DeploymentID: "dep-1", Setters: []string{"eve"}, Lifters: []string{"eve"}, Verifiers: fakeVerifiers("eve"), LiftQuorum: 1})
	_, _ = evil.SetStop(ctx, StopRequest{Scope: StopScope{Kind: StopGlobal}, Reason: "denial of service", By: "eve", Signer: sign("eve")})
	if st := mustState(t, real); len(st.Stops) != 0 || st.RejectedTotal == 0 {
		t.Fatalf("setup: %+v", st)
	}
	sink := &fakeSink{}
	if err := stopSweep(t, real, sink, NewMemNotifyTrail(), sweepClock()); err != nil || sink.count() != 0 {
		t.Fatalf("%v, %d notifications for a forged stop", err, sink.count())
	}
}

// Only stops within the window are news; an older one is history.
func TestStopNotify_TheWindow(t *testing.T) {
	ctl := newControl(t, NewMemStore())
	setTool(t, ctl, "refund")
	clk := sweepClock()
	clk.advance(30 * 24 * time.Hour) // a month after the stop was set (2026-10-17)
	sink := &fakeSink{}
	if err := stopSweep(t, ctl, sink, NewMemNotifyTrail(), clk); err != nil || sink.count() != 0 {
		t.Fatalf("a month-old stop: %v, %d notifications", err, sink.count())
	}
	if err := stopSweep(t, ctl, sink, NewMemNotifyTrail(), clk, WithNotifyStopsWithin(60*24*time.Hour)); err != nil || sink.count() != 1 {
		t.Fatalf("within a wider window: %v, %d notifications", err, sink.count())
	}
	if WithNotifyStopsWithin(time.Second) == nil {
		t.Fatal("nil option")
	}
	if err := stopSweep(t, ctl, sink, NewMemNotifyTrail(), clk, WithNotifyStopsWithin(time.Second)); err == nil {
		t.Fatal("a window under a minute must be refused")
	}
}

// Routing and escalation apply as for any kind.
func TestStopNotify_Routing(t *testing.T) {
	ctl := newControl(t, NewMemStore())
	e := setTool(t, ctl, "refund")
	def, stops := &fakeSink{}, &fakeSink{}
	trail := NewMemNotifyTrail()
	err := NotifyPending(context.Background(), ctl.j, def, trail, fixedNotifyClock(sweepClock()), WithNotifyHolder("s"), WithNotifyStops(ctl),
		WithNotifyReceiver("stops", stops), WithNotifyRoute(NotificationStopSet, "stops"), WithNotifyRoute(NotificationStopLifted, "stops"))
	if err != nil {
		t.Fatal(err)
	}
	if def.count() != 0 || stops.count() != 1 || stops.calls[0].Receiver != "stops" || stops.calls[0].Kind != NotificationStopSet {
		t.Fatalf("default %d, stops %d %+v", def.count(), stops.count(), stops.calls)
	}
	if _, err := ctl.LiftStop(context.Background(), e.ID, LiftRequest{By: "dayna"}); err != nil {
		t.Fatal(err)
	}
	err = NotifyPending(context.Background(), ctl.j, def, trail, fixedNotifyClock(sweepClock()), WithNotifyHolder("s"), WithNotifyStops(ctl),
		WithNotifyReceiver("stops", stops), WithNotifyRoute(NotificationStopSet, "stops"), WithNotifyRoute(NotificationStopLifted, "stops"))
	if err != nil || def.count() != 0 || stops.count() != 3 {
		t.Fatalf("after the lift: %v, default %d stops %d (%v)", err, def.count(), stops.count(), kindsOf(stops))
	}
}

// A stop that nobody acknowledged escalates through the chain, and the escalation carries the stop's detail
// even when it is chased by a process that only has the key from the trail.
func TestStopNotify_Escalation(t *testing.T) {
	ctl := newControl(t, NewMemStore())
	e := setTool(t, ctl, "refund")
	def, lead := &fakeSink{fail: 1000}, &fakeSink{}
	trail, clk := NewMemNotifyTrail(), routeClock()
	eng := testEngine(t, ctl.j, def, trail, clk, "p1", WithNotifyReceiver("lead", lead),
		WithNotifyEscalation(NotificationStopSet, EscalationStep{Receiver: "lead", Deadline: 10 * time.Minute}))
	eng.cfg.stops = ctl
	ev := eng.stopEvents(mustState(t, ctl))[0]
	_, _ = eng.round(context.Background(), ev) // the sink is down: observed, claimed, not acked
	clk.advance(6 * time.Minute)
	chase(t, eng, ev)
	if lead.count() != 1 {
		t.Fatalf("lead got %d notifications, want 1", lead.count())
	}
	n := lead.calls[0]
	if n.Kind != NotificationEscalated || n.Escalates == nil || n.Escalates.Key != StopSetKey(e.ID) || n.Escalates.Kind != NotificationStopSet || n.Stop == nil || n.Stop.Scope.Name != "refund" {
		t.Fatalf("escalation = %+v / %+v / %+v", n, n.Escalates, n.Stop)
	}
	// Rebuilt from the trail alone, the event is filled in from the control run.
	recs, err := trail.Load(context.Background(), NotifyTrailName(StopControlRun))
	if err != nil {
		t.Fatal(err)
	}
	es := FoldNotify(recs).Events[StopSetKey(e.ID)]
	if es == nil {
		t.Fatalf("the stop event is not on the trail of the control run: %v", recKinds(recs))
	}
	rebuilt, ok := eventFromKey(es)
	if !ok || rebuilt.stop == nil || rebuilt.stop.Scope.Kind != "" {
		t.Fatalf("%+v %v", rebuilt, ok)
	}
	eng.fillDetail(context.Background(), &rebuilt)
	if rebuilt.stop.Scope.Name != "refund" || rebuilt.stop.Reason != "provider incident" || rebuilt.stop.By != "dayna" {
		t.Fatalf("fillDetail: %+v", rebuilt.stop)
	}
}

func TestStopNotify_KeysRoundTrip(t *testing.T) {
	id := strings.Repeat("ab", 32)
	for _, tc := range []struct {
		key    string
		kind   NotificationKind
		lifted bool
	}{{StopSetKey(id), NotificationStopSet, false}, {StopLiftedKey(id), NotificationStopLifted, true}, {ToKey("ops", StopSetKey(id)), NotificationStopSet, false}} {
		got, lifted, ok := stopIDOfKey(tc.key)
		if !ok || got != id || lifted != tc.lifted || kindOfKey(tc.key) != tc.kind {
			t.Errorf("%s: id %q lifted %v ok %v kind %q", tc.key, got, lifted, ok, kindOfKey(tc.key))
		}
	}
	for _, bad := range []string{"stop:", "stop:a:b", "stop:a:lifted:x", "halted:x"} {
		if _, _, ok := stopIDOfKey(bad); ok {
			t.Errorf("%q parsed as a stop key", bad)
		}
	}
	es := &NotifyEvent{Key: StopSetKey(id), Kind: string(NotificationStopSet), RunID: StopControlRun}
	if ev, ok := eventFromKey(es); !ok || ev.stop == nil || ev.stop.ID != id {
		t.Fatalf("eventFromKey: %+v %v", ev, ok)
	}
}

// waitFor polls cond for up to 5 seconds.
func stopWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func stopWorld(t *testing.T, store Store, sink Notifier, trail NotifyTrail, holder string) (*StopControl, *StopSwitch, *StopNotifier) {
	t.Helper()
	ctl := newControl(t, store, WithStopClock(time.Now))
	n, err := NewStopNotifier(ctl.j, WithNotifier(sink, trail, WithNotifyHolder(holder)), ctl)
	if err != nil {
		t.Fatal(err)
	}
	sw, err := NewStopSwitch(ctl, WithStopPoll(10*time.Millisecond), WithStopObserver(n.Observe))
	if err != nil {
		t.Fatal(err)
	}
	return ctl, sw, n
}

// A switch's observer notifies within the poll interval, and the sweep that comes later does not notify
// again: the trail's claim on the event key is the one dedupe.
func TestStopNotify_TheSwitchHookNotifiesAndTheSweepDoesNotRepeat(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	ctl, sw, n := stopWorld(t, store, sink, trail, "proc-a")
	sw.GlobalStop(ctx) // a first read: nothing is stopped
	e := setTool(t, ctl, "refund")
	stopWait(t, "the stop_set", func() bool { sw.GlobalStop(ctx); return sink.count() == 1 })
	if err := n.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sweep(t, ctl.j, sink, trail, &notifyClock{t: time.Now()}, WithNotifyStops(ctl)); err != nil {
		t.Fatal(err)
	}
	if sink.count() != 1 || sink.calls[0].Key != StopSetKey(e.ID) {
		t.Fatalf("%d notifications after the hook and the sweep: %v", sink.count(), kindsOf(sink))
	}
}

// Two processes with a switch each on one store (and one trail) send one notification between them.
func TestStopNotify_TwoSwitchesOneNotification(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	ctlA, swA, nA := stopWorld(t, store, sink, trail, "proc-a")
	_, swB, nB := stopWorld(t, store, sink, trail, "proc-b")
	swA.GlobalStop(ctx)
	swB.GlobalStop(ctx)
	setTool(t, ctlA, "refund")
	stopWait(t, "a notification", func() bool { swA.GlobalStop(ctx); swB.GlobalStop(ctx); return sink.count() >= 1 })
	for _, n := range []*StopNotifier{nA, nB} {
		if err := n.Flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if sink.count() != 1 {
		t.Fatalf("%d notifications from two processes, want 1", sink.count())
	}
}

// The observer is handed off the switch's path: a function that blocks forever, or panics, never delays
// or fails a check point, and a state that arrives meanwhile is kept (the latest only) for the next call.
func TestStopObserver_NeverDelaysOrFailsAnyCheck(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	block := make(chan struct{})
	defer close(block)
	var calls atomic.Int32
	ctl := newControl(t, store, WithStopClock(time.Now))
	sw, err := NewStopSwitch(ctl, WithStopPoll(10*time.Millisecond), WithStopObserver(func(StopState) {
		calls.Add(1)
		<-block // never returns
	}))
	if err != nil {
		t.Fatal(err)
	}
	sw.GlobalStop(ctx)
	setTool(t, ctl, "a")
	stopWait(t, "the observer to be called", func() bool { sw.GlobalStop(ctx); return calls.Load() >= 1 })
	for i := range 20 { // while the observer is stuck, stops keep being set and checks keep being answered
		setTool(t, ctl, "t"+string(rune('b'+i)))
		start := time.Now()
		if h := sw.Hold(ctx, RunRef{RunID: "r"}, StopQuery{Tool: "t" + string(rune('b'+i))}, true, true); h == nil {
			t.Fatalf("a stop of tool %d does not hold the call", i)
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("a check took %v while the observer was blocked", d)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("the blocked observer was called %d times", calls.Load())
	}
}

func TestStopObserver_APanicEndsOnlyItsCall(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var seen []int
	ctl := newControl(t, NewMemStore(), WithStopClock(time.Now))
	sw, err := NewStopSwitch(ctl, WithStopPoll(10*time.Millisecond), WithStopObserver(func(st StopState) {
		mu.Lock()
		seen = append(seen, len(st.Stops))
		mu.Unlock()
		panic("observer bug")
	}))
	if err != nil {
		t.Fatal(err)
	}
	setTool(t, ctl, "a")
	stopWait(t, "the first call", func() bool { sw.GlobalStop(ctx); mu.Lock(); defer mu.Unlock(); return len(seen) == 1 })
	if h := sw.Hold(ctx, RunRef{RunID: "r"}, StopQuery{Tool: "a"}, true, true); h == nil {
		t.Fatal("the stop must hold the call after the observer panicked")
	}
	setTool(t, ctl, "b")
	stopWait(t, "the second call, after a panic", func() bool { sw.GlobalStop(ctx); mu.Lock(); defer mu.Unlock(); return len(seen) == 2 })
}

// The observer is told of changes, not of every poll.
func TestStopObserver_OnlyOnChange(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	ctl := newControl(t, NewMemStore(), WithStopClock(time.Now))
	sw, _ := NewStopSwitch(ctl, WithStopPoll(10*time.Millisecond), WithStopObserver(func(StopState) { calls.Add(1) }))
	e := setTool(t, ctl, "a")
	for range 30 {
		sw.GlobalStop(ctx)
		time.Sleep(12 * time.Millisecond)
	}
	stopWait(t, "the first observation", func() bool { return calls.Load() == 1 })
	if _, err := ctl.LiftStop(ctx, e.ID, LiftRequest{By: "dayna"}); err != nil {
		t.Fatal(err)
	}
	stopWait(t, "the lift", func() bool { sw.GlobalStop(ctx); return calls.Load() == 2 })
	for range 10 {
		sw.GlobalStop(ctx)
		time.Sleep(12 * time.Millisecond)
	}
	if calls.Load() != 2 {
		t.Fatalf("%d calls, want 2 (one per change)", calls.Load())
	}
}

// A notifier that blocks, or panics, costs the notification and nothing else: the switch still enforces.
func TestStopNotify_ABlockedOrPanickingNotifierNeverTouchesEnforcement(t *testing.T) {
	ctx := context.Background()
	for name, sink := range map[string]Notifier{"blocks": blockSink{}, "panics": stopPanicSink{}} {
		t.Run(name, func(t *testing.T) {
			ctl, sw, _ := stopWorld(t, NewMemStore(), sink, NewMemNotifyTrail(), "p")
			sw.GlobalStop(ctx)
			setTool(t, ctl, "refund")
			for range 20 {
				start := time.Now()
				if h := sw.Hold(ctx, RunRef{RunID: "r"}, StopQuery{Tool: "refund"}, true, true); h == nil {
					t.Fatal("the stop does not hold the call")
				}
				if d := time.Since(start); d > time.Second {
					t.Fatalf("a check took %v", d)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

type blockSink struct{}

func (blockSink) Notify(ctx context.Context, _ Notification) error { <-ctx.Done(); return ctx.Err() }

type stopPanicSink struct{}

func (stopPanicSink) Notify(context.Context, Notification) error { panic("sink bug") }

// The lift of a long-running stop is told: the window is judged on the time of the lift, not of the set.
func TestStopNotify_TheLiftOfAnOldStopIsNotified(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	setClk := &notifyClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	ctl := newControl(t, store, WithStopClock(setClk.now))
	e := setTool(t, ctl, "refund")
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	clk := &notifyClock{t: setClk.now()}
	if err := stopSweep(t, ctl, sink, trail, clk); err != nil || sink.count() != 1 {
		t.Fatalf("the set: %v, %d", err, sink.count())
	}
	// Eight days later (past the default window of seven) the stop is lifted.
	setClk.advance(8 * 24 * time.Hour)
	clk.advance(8 * 24 * time.Hour)
	if _, err := ctl.LiftStop(ctx, e.ID, LiftRequest{By: "dayna"}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := stopSweep(t, ctl, sink, trail, clk); err != nil {
			t.Fatal(err)
		}
	}
	got := kindsOf(sink)
	slices.Sort(got)
	if want := []NotificationKind{NotificationResolved, NotificationStopLifted, NotificationStopSet}; !slices.Equal(got, want) {
		t.Fatalf("notifications %v, want %v: the lift of a stop older than the window was not told", got, want)
	}
}

// A stop set and lifted long ago is history: neither event is raised for it.
func TestStopNotify_ALongAgoLiftIsNotNews(t *testing.T) {
	ctx := context.Background()
	setClk := &notifyClock{t: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
	ctl := newControl(t, NewMemStore(), WithStopClock(setClk.now))
	e := setTool(t, ctl, "refund")
	setClk.advance(time.Hour)
	if _, err := ctl.LiftStop(ctx, e.ID, LiftRequest{By: "dayna"}); err != nil {
		t.Fatal(err)
	}
	clk := &notifyClock{t: setClk.now().Add(30 * 24 * time.Hour)}
	sink := &fakeSink{}
	if err := stopSweep(t, ctl, sink, NewMemNotifyTrail(), clk); err != nil || sink.count() != 0 {
		t.Fatalf("%v, %d notifications for a stop lifted a month ago", err, sink.count())
	}
}

// A process that is not given the control (no WithNotifyStops) cannot tell whether a stop still stands: it
// leaves stop_set events alone, so it neither escalates one forever nor claims it ended, and says so once.
func TestStopNotify_AProcessWithoutTheControlLeavesStopSetAlone(t *testing.T) {
	ctl := newControl(t, NewMemStore())
	setTool(t, ctl, "refund")
	trail, clk := NewMemNotifyTrail(), routeClock()
	def, lead := &fakeSink{fail: 1000}, &fakeSink{}
	opts := []NotifyOption{WithNotifyReceiver("lead", lead), WithNotifyEscalation(NotificationStopSet, EscalationStep{Receiver: "lead", Deadline: 10 * time.Minute})}
	withStops := testEngine(t, ctl.j, def, trail, clk, "p1", opts...)
	withStops.cfg.stops = ctl
	ev := withStops.stopEvents(mustState(t, ctl))[0]
	_, _ = withStops.round(context.Background(), ev) // observed and claimed; the sink is down

	var reports atomic.Int32
	without := testEngine(t, ctl.j, def, trail, clk, "p2", append(opts, WithNotifyErrors(func(error) { reports.Add(1) }))...)
	clk.advance(30 * time.Minute)
	for range 3 {
		scanAndFlush(t, without, StopControlRun)
	}
	if lead.count() != 0 {
		t.Fatalf("a process that cannot judge the stop escalated it: %v", kindsOf(lead))
	}
	if got := reports.Load(); got != 1 {
		t.Fatalf("it reported that stop notifications are off %d times, want once", got)
	}
}
