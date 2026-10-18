package audit_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// stopRigN is a control run with a basic control, the notifier's trail over an event store, and a clock.
type stopRigN struct {
	t     *testing.T
	j     *agent.Journal
	ctl   *agent.StopControl
	es    *audit.MemEventStore
	trail *audit.NotifyTrail
	now   time.Time
}

func newStopRigN(t *testing.T) *stopRigN {
	t.Helper()
	j, err := agent.NewJournal(agent.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ctl, err := agent.NewStopControl(j, agent.WithStopMode(agent.StopBasic), agent.WithStopClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	es := audit.NewMemEventStore()
	return &stopRigN{t: t, j: j, ctl: ctl, es: es, trail: audit.NewNotifyTrail(es), now: now}
}

func (r *stopRigN) set(tool string) agent.StopEntry {
	r.t.Helper()
	e, err := r.ctl.SetStop(context.Background(), agent.StopRequest{Scope: agent.StopScope{Kind: agent.StopTool, Name: tool}, Reason: "incident", By: "dayna", Host: "h"})
	if err != nil {
		r.t.Fatal(err)
	}
	return e
}

func (r *stopRigN) lift(id string) {
	r.t.Helper()
	if _, err := r.ctl.LiftStop(context.Background(), id, agent.LiftRequest{By: "dayna"}); err != nil {
		r.t.Fatal(err)
	}
}

// notify runs the notifier's sweep over the control run.
func (r *stopRigN) notify() {
	r.t.Helper()
	err := agent.NotifyPending(context.Background(), r.j, nopSink{}, r.trail, agent.WithNotifyStops(r.ctl), agent.WithNotifyHolder("p"))
	if err != nil {
		r.t.Fatal(err)
	}
}

// verify audits the control run's events against the trail, judged at now+d.
func (r *stopRigN) verify(d time.Duration, strict bool) (audit.NotifyReport, error) {
	r.t.Helper()
	ctx := context.Background()
	st, err := r.ctl.State(ctx)
	if err != nil {
		r.t.Fatal(err)
	}
	var in audit.NotifyVerifyInput
	in.Unanchored, in.Strict, in.Now = true, strict, r.now.Add(d)
	in.StopEvents = agent.StopNotifyEvents(st, in.Now, 0)
	x, err := audit.ExportNotifyTrail(ctx, r.es, agent.NotifyTrailName(agent.StopControlRun))
	if err != nil {
		r.t.Fatal(err)
	}
	if len(x.Leaves) > 0 {
		in.Trails = append(in.Trails, x)
	}
	return audit.VerifyNotifications(in)
}

func statusesOf(rep audit.NotifyReport) map[string]audit.NotifyStatus {
	out := map[string]audit.NotifyStatus{}
	for _, e := range rep.Events {
		out[e.Kind+" "+e.Key] = e.Status
	}
	return out
}

// The real engine's trail of a stop and its lift verifies: the stop_set, its all-clear, the stop_lifted.
func TestVerifyNotifications_TheStopTrailVerifies(t *testing.T) {
	r := newStopRigN(t)
	e := r.set("refund")
	r.notify()
	if rep, err := r.verify(time.Hour, false); err != nil || len(rep.Problems) != 0 || rep.Counts()[audit.NotifyStatusAcked] != 1 {
		t.Fatalf("a notified stop: %v %+v", err, rep.Problems)
	}
	r.lift(e.ID)
	r.notify()
	rep, err := r.verify(time.Hour, true)
	if err != nil {
		t.Fatalf("%v %+v", err, rep.Problems)
	}
	got := statusesOf(rep)
	for _, key := range []string{"stop_set " + agent.StopSetKey(e.ID), "stop_lifted " + agent.StopLiftedKey(e.ID), "resolved " + agent.ResolvedKey(agent.StopSetKey(e.ID))} {
		if got[key] != audit.NotifyStatusAcked {
			t.Errorf("%s: %q, want acked (%v)", key, got[key], got)
		}
	}
}

// A stop no notifier saw, a lift no notifier saw, and an all-clear that was never sent are found.
func TestVerifyNotifications_AnUnnotifiedStopAndLiftAreFound(t *testing.T) {
	r := newStopRigN(t)
	r.set("payout")
	rep, err := r.verify(time.Hour, false)
	if err == nil || !slices.ContainsFunc(rep.Events, func(e audit.NotifyEventVerdict) bool {
		return e.Kind == "stop_set" && e.Status == audit.NotifyStatusNeverNotified
	}) {
		t.Fatalf("a stop nobody was told of: %v %+v", err, rep.Events)
	}
	// Within the grace period (the notifier's detection window) it is not yet a failure.
	if _, err := r.verify(time.Minute, false); err != nil {
		t.Fatalf("within the grace period: %v", err)
	}

	r2 := newStopRigN(t)
	e := r2.set("refund")
	r2.notify()
	r2.lift(e.ID) // lifted, and the notifier never ran again
	rep, err = r2.verify(time.Hour, false)
	if err == nil {
		t.Fatal("a lift no notifier saw passed")
	}
	got := statusesOf(rep)
	if got["stop_lifted "+agent.StopLiftedKey(e.ID)] != audit.NotifyStatusNeverNotified || got["resolved "+agent.ResolvedKey(agent.StopSetKey(e.ID))] != audit.NotifyStatusNeverNotified {
		t.Fatalf("%v", got)
	}
}

// A stop set and lifted before any notifier looked owes no stop_set (nothing was current to tell), only its
// lift: the notifier observes the set and clears it before delivery, and the audit reads that as it is.
func TestVerifyNotifications_ASetAndLiftBetweenSweeps(t *testing.T) {
	r := newStopRigN(t)
	e := r.set("refund")
	r.lift(e.ID)
	r.notify()
	rep, err := r.verify(time.Hour, false)
	if err != nil {
		t.Fatalf("%v %+v", err, rep.Problems)
	}
	got := statusesOf(rep)
	if got["stop_lifted "+agent.StopLiftedKey(e.ID)] != audit.NotifyStatusAcked || got["stop_set "+agent.StopSetKey(e.ID)] != audit.NotifyStatusClearedBeforeNotification {
		t.Fatalf("%v", got)
	}
	// With no notifier run at all, the set is a warning (a failure under -strict) and the lift is owed.
	r2 := newStopRigN(t)
	e2 := r2.set("refund")
	r2.lift(e2.ID)
	rep2, err := r2.verify(time.Hour, false)
	got2 := statusesOf(rep2)
	if err == nil || got2["stop_set "+agent.StopSetKey(e2.ID)] != audit.NotifyStatusUnobservedResolved || got2["stop_lifted "+agent.StopLiftedKey(e2.ID)] != audit.NotifyStatusNeverNotified {
		t.Fatalf("%v %v", err, got2)
	}
}

// The window: a stop older than it is history and owes nothing.
func TestVerifyNotifications_TheStopWindow(t *testing.T) {
	r := newStopRigN(t)
	r.set("refund")
	if rep, err := r.verify(30*24*time.Hour, false); err != nil || len(rep.Events) != 0 {
		t.Fatalf("a month-old stop: %v %+v", err, rep.Events)
	}
	st, _ := r.ctl.State(context.Background())
	if ev := agent.StopNotifyEvents(st, r.now.Add(30*24*time.Hour), 60*24*time.Hour); len(ev) != 1 {
		t.Fatalf("a wider window: %v", ev)
	}
}

// The notifier and the audit judge by one rule: whatever a sweep raises, the audit expects, and nothing more.
func TestStopNotifyEvents_AgreeWithWhatTheNotifierRaises(t *testing.T) {
	r := newStopRigN(t)
	a := r.set("a")
	r.set("b")
	r.lift(a.ID)
	r.notify()
	st, _ := r.ctl.State(context.Background())
	owed := map[string]bool{}
	for _, ev := range agent.StopNotifyEvents(st, r.now.Add(time.Minute), 0) {
		owed[ev.Key] = true
	}
	recs, err := r.trail.Load(context.Background(), agent.NotifyTrailName(agent.StopControlRun))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, rec := range recs {
		if rec.Kind == agent.NotifyObserved && strings.HasPrefix(rec.Key, "stop:") {
			seen[rec.Key] = true
		}
	}
	for k := range seen {
		if !owed[k] {
			t.Errorf("the notifier raised %s, which the audit does not expect", k)
		}
	}
	for k := range owed {
		if !seen[k] && !strings.HasPrefix(k, agent.StopSetKey(a.ID)) { // a's set was cleared before any delivery
			t.Errorf("the audit expects %s, which the notifier did not raise", k)
		}
	}
}

func TestParseStopKey(t *testing.T) {
	id := strings.Repeat("cd", 32)
	for _, tc := range []struct {
		key  string
		kind agent.NotificationKind
		ok   bool
	}{{agent.StopSetKey(id), agent.NotificationStopSet, true}, {agent.StopLiftedKey(id), agent.NotificationStopLifted, true}, {"halted:x", "", false}, {"stop:", "", false}, {"stop:a:b", "", false}} {
		got, kind, ok := agent.ParseStopKey(tc.key)
		if ok != tc.ok || kind != tc.kind || ok && got != id {
			t.Errorf("%q: %q %q %v", tc.key, got, kind, ok)
		}
	}
}

// An observed stop event whose key is not the key of its kind is a violation: a stop_set under the key of a lift,
// or of another kind of event, is not a history a notifier writes.
func TestVerifyNotifications_AStopEventUnderAnotherKindsKey(t *testing.T) {
	id := strings.Repeat("ab", 32)
	for name, key := range map[string]string{"a lift's key": agent.StopLiftedKey(id), "an approval's key": "approval:r:c:0", "no id": "stop:"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			es := audit.NewMemEventStore()
			trail := audit.NewNotifyTrail(es)
			at := time.Now()
			rec := agent.NotifyRecord{Kind: agent.NotifyObserved, Key: key, RunID: agent.StopControlRun, At: at, EventKind: string(agent.NotificationStopSet), EventAt: at, DeadlineAt: at.Add(5 * time.Minute)}
			if err := trail.Append(ctx, agent.NotifyTrailName(agent.StopControlRun), 0, rec); err != nil {
				t.Fatal(err)
			}
			x, err := audit.ExportNotifyTrail(ctx, es, agent.NotifyTrailName(agent.StopControlRun))
			if err != nil {
				t.Fatal(err)
			}
			rep, err := audit.VerifyNotifications(audit.NotifyVerifyInput{Trails: []audit.NotifyTrailExport{x}, Unanchored: true, Now: at.Add(time.Minute)})
			if err == nil || !slices.ContainsFunc(rep.Problems, func(p string) bool { return strings.Contains(p, "is not the key of a stop_set event") }) {
				t.Fatalf("%v %+v", err, rep.Problems)
			}
		})
	}
}
