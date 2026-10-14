package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Routing and escalation of the notifier: an event goes to the receiver its kind (or, for an
// approval, its approvers) names, each receiver with its own claim and acknowledgement in the
// trail; an event not acknowledged (an approval not decided) by its deadline is escalated to the
// next receiver of a bounded chain, which has a deadline of its own. See docs/design/notifier.md,
// "Routing and escalation".

func routeClock() *notifyClock {
	return &notifyClock{t: time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)}
}

func approvalEvent(approvers ...string) notifyEvent {
	return notifyEvent{key: ApprovalKey("r", "c1", 0), kind: NotificationApprovalPending, runID: "r", opRun: "r",
		approval: &NotifyApproval{ToolUseID: "c1", ToolName: "refund", Need: 1, Of: len(approvers), Approvers: approvers}}
}

func recsFor(t *testing.T, tr NotifyTrail, key string) []NotifyRecord {
	return trailOf(t, tr, "r", key)
}

func foldTrail(t *testing.T, tr NotifyTrail) NotifyState {
	t.Helper()
	recs, err := tr.Load(context.Background(), NotifyTrailName("r"))
	if err != nil {
		t.Fatal(err)
	}
	return FoldNotify(recs)
}

func mustRound(t *testing.T, e *notifyEngine, ev notifyEvent) {
	t.Helper()
	if err := e.roundRouted(context.Background(), ev); err != nil {
		t.Fatalf("round %s: %v", ev.key, err)
	}
}

func TestRouting_AKindGoesToItsReceiverAndTheTrailNamesIt(t *testing.T) {
	clk := routeClock()
	def, oncall, appr := &fakeSink{}, &fakeSink{}, &fakeSink{}
	trail := NewMemNotifyTrail()
	e := testEngine(t, memJournal(), def, trail, clk, "p1",
		WithNotifyReceiver("oncall", oncall), WithNotifyReceiver("approvers", appr),
		WithNotifyRoute(NotificationOutcomeUnknown, "oncall"), WithNotifyRoute(NotificationApprovalPending, "approvers"))

	halt, ap := haltEvent(), approvalEvent()
	mustRound(t, e, halt)
	mustRound(t, e, ap)

	if oncall.count() != 1 || appr.count() != 1 || def.count() != 0 {
		t.Fatalf("deliveries: oncall=%d approvers=%d default=%d, want 1 1 0", oncall.count(), appr.count(), def.count())
	}
	if n := oncall.calls[0]; n.Kind != NotificationOutcomeUnknown || n.Receiver != "oncall" || n.ID != NotifyID(halt.key) {
		t.Fatalf("oncall got %+v", n)
	}
	if n := appr.calls[0]; n.Kind != NotificationApprovalPending || n.Receiver != "approvers" || n.ID != NotifyID(ap.key) {
		t.Fatalf("approvers got %+v", n)
	}
	// Each receiver's events have their own claim and acknowledgement in the one trail.
	st := foldTrail(t, trail)
	by := st.ByReceiver()
	if got := by["oncall"]; len(got) != 1 || got[0] != halt.key {
		t.Fatalf("ByReceiver[oncall] = %v", got)
	}
	if got := by["approvers"]; len(got) != 1 || got[0] != ap.key {
		t.Fatalf("ByReceiver[approvers] = %v", got)
	}
	for _, k := range []string{halt.key, ap.key} {
		es := st.Events[k]
		if es.Acked == nil || es.Rounds != 1 {
			t.Fatalf("%s: acked=%v rounds=%d", k, es.Acked, es.Rounds)
		}
	}
	if r := recsFor(t, trail, halt.key)[0]; r.Kind != NotifyObserved || r.Receiver != "oncall" {
		t.Fatalf("observed record = %+v", r)
	}
}

func TestRouting_TheDefaultReceiverLeavesTheTrailAsItWas(t *testing.T) {
	clk := routeClock()
	def := &fakeSink{}
	trail := NewMemNotifyTrail()
	e := testEngine(t, memJournal(), def, trail, clk, "p1", WithNotifyReceiver("oncall", &fakeSink{}), WithNotifyRoute(NotificationRunAborted, "oncall"))
	halt := haltEvent()
	mustRound(t, e, halt)
	for _, r := range recsFor(t, trail, halt.key) {
		if r.Receiver != "" || r.Level != 0 {
			t.Fatalf("a default delivery's record names a receiver: %+v", r)
		}
	}
	if n := def.calls[0]; n.Receiver != "default" {
		t.Fatalf("notification.Receiver = %q, want default", n.Receiver)
	}
	if got := foldTrail(t, trail).ByReceiver()["default"]; len(got) != 1 {
		t.Fatalf("ByReceiver[default] = %v", got)
	}
}

func TestRouting_ADownReceiverDoesNotHoldBackAnother(t *testing.T) {
	clk := routeClock()
	oncall, appr := &fakeSink{fail: 1000}, &fakeSink{}
	trail := NewMemNotifyTrail()
	e := testEngine(t, memJournal(), &fakeSink{}, trail, clk, "p1",
		WithNotifyReceiver("oncall", oncall), WithNotifyReceiver("approvers", appr),
		WithNotifyRoute(NotificationOutcomeUnknown, "oncall"), WithNotifyRoute(NotificationApprovalPending, "approvers"))
	halt, ap := haltEvent(), approvalEvent()
	if err := e.roundRouted(context.Background(), halt); err == nil {
		t.Fatal("a round to a down receiver reported success")
	}
	mustRound(t, e, ap)
	st := foldTrail(t, trail)
	if st.Events[halt.key].Acked != nil || st.Events[ap.key].Acked == nil {
		t.Fatalf("acked: halt=%v approval=%v; want only the approval", st.Events[halt.key].Acked, st.Events[ap.key].Acked)
	}
}

func TestRouting_ApproverRoutesFanOutWithAKeyPerReceiver(t *testing.T) {
	clk := routeClock()
	def, risk, fin := &fakeSink{}, &fakeSink{}, &fakeSink{}
	trail := NewMemNotifyTrail()
	e := testEngine(t, memJournal(), def, trail, clk, "p1",
		WithNotifyReceiver("risk", risk), WithNotifyReceiver("finance", fin),
		WithNotifyApproverRoute("alice", "risk"), WithNotifyApproverRoute("bob", "finance"), WithNotifyApproverRoute("carol", "finance"))

	routed := e.routed(approvalEvent("alice", "bob", "carol"))
	if len(routed) != 2 {
		t.Fatalf("routed to %d receivers, want 2", len(routed))
	}
	if routed[0].key == routed[1].key || routed[0].key == approvalEvent().key {
		t.Fatalf("a fan-out must give each receiver a key of its own: %q %q", routed[0].key, routed[1].key)
	}
	mustRound(t, e, approvalEvent("alice", "bob", "carol"))
	if risk.count() != 1 || fin.count() != 1 || def.count() != 0 {
		t.Fatalf("deliveries: risk=%d finance=%d default=%d", risk.count(), fin.count(), def.count())
	}
	if risk.calls[0].ID == fin.calls[0].ID {
		t.Fatal("two receivers share a delivery ID")
	}
	if risk.calls[0].Approval == nil || len(risk.calls[0].Approval.Approvers) != 3 {
		t.Fatalf("the notification lost the gate's approvers: %+v", risk.calls[0].Approval)
	}
	st := foldTrail(t, trail)
	if len(st.ByReceiver()["risk"]) != 1 || len(st.ByReceiver()["finance"]) != 1 {
		t.Fatalf("ByReceiver = %v", st.ByReceiver())
	}
}

func TestRouting_ApproverFanOutOneReceiverDownTheOtherStillGetsItsOwn(t *testing.T) {
	clk := routeClock()
	risk, fin := &fakeSink{fail: 1000}, &fakeSink{}
	trail := NewMemNotifyTrail()
	e := testEngine(t, memJournal(), &fakeSink{}, trail, clk, "p1",
		WithNotifyReceiver("risk", risk), WithNotifyReceiver("finance", fin),
		WithNotifyApproverRoute("alice", "risk"), WithNotifyApproverRoute("bob", "finance"))
	_ = e.roundRouted(context.Background(), approvalEvent("alice", "bob"))
	if fin.count() != 1 {
		t.Fatalf("finance got %d, want 1: the first receiver's ack must not stand for the second", fin.count())
	}
	st := foldTrail(t, trail)
	var acked, open int
	for _, k := range st.Keys {
		if st.Events[k].Acked != nil {
			acked++
		} else {
			open++
		}
	}
	if acked != 1 || open != 1 {
		t.Fatalf("acked=%d open=%d, want 1 and 1", acked, open)
	}
}

func TestRouting_OneRoutedApproverKeepsTheEventKey(t *testing.T) {
	e := testEngine(t, memJournal(), &fakeSink{}, NewMemNotifyTrail(), routeClock(), "p1",
		WithNotifyReceiver("risk", &fakeSink{}), WithNotifyApproverRoute("alice", "risk"), WithNotifyApproverRoute("bob", "risk"))
	routed := e.routed(approvalEvent("alice", "bob"))
	if len(routed) != 1 || routed[0].key != approvalEvent().key || routed[0].receiver != "risk" {
		t.Fatalf("routed = %+v", routed)
	}
	// An approver with no route, and a 1-of-1 gate, fall back to the kind's receiver.
	if r := e.routed(approvalEvent()); len(r) != 1 || r[0].receiver != "" {
		t.Fatalf("a gate with no approvers routed to %+v", r)
	}
}

func TestRouting_TheAllClearGoesToTheReceiverOfTheEvent(t *testing.T) {
	ctx := context.Background()
	clk := routeClock()
	j := memJournal()
	def, oncall := &fakeSink{}, &fakeSink{}
	trail := NewMemNotifyTrail()
	e := testEngine(t, j, def, trail, clk, "p1", WithNotifyReceiver("oncall", oncall), WithNotifyRoute(NotificationOutcomeUnknown, "oncall"))
	halt := haltEvent()
	mustRound(t, e, halt)
	putResult(t, j, "r", "c1")
	scanAndFlush(t, e, "r")
	if got := resolvedCalls(oncall); len(got) != 1 || got[0].Clears.Key != halt.key || got[0].Receiver != "oncall" {
		t.Fatalf("oncall's all-clears = %+v", got)
	}
	if len(resolvedCalls(def)) != 0 {
		t.Fatal("the all-clear went to the default receiver")
	}
	if r := recsFor(t, trail, ResolvedKey(halt.key))[0]; r.Receiver != "oncall" {
		t.Fatalf("all-clear observed record = %+v", r)
	}
	_ = ctx
}

func TestRouting_ConfigurationErrors(t *testing.T) {
	cases := map[string][]NotifyOption{
		"route to an unknown receiver":     {WithNotifyRoute(NotificationOutcomeUnknown, "nobody")},
		"approver route to unknown":        {WithNotifyApproverRoute("alice", "nobody")},
		"escalation to an unknown":         {WithNotifyEscalation(NotificationOutcomeUnknown, EscalationStep{Receiver: "nobody", Deadline: time.Minute})},
		"bad receiver name":                {WithNotifyReceiver("On Call!", &fakeSink{})},
		"empty receiver name":              {WithNotifyReceiver("", &fakeSink{})},
		"reserved receiver name":           {WithNotifyReceiver("default", &fakeSink{})},
		"nil receiver":                     {WithNotifyReceiver("oncall", nil)},
		"duplicate receiver":               {WithNotifyReceiver("a", &fakeSink{}), WithNotifyReceiver("a", &fakeSink{})},
		"route of an unroutable kind":      {WithNotifyReceiver("a", &fakeSink{}), WithNotifyRoute(NotificationResolved, "a")},
		"empty approver":                   {WithNotifyReceiver("a", &fakeSink{}), WithNotifyApproverRoute("", "a")},
		"escalation with no step":          {WithNotifyEscalation(NotificationOutcomeUnknown)},
		"escalation for run_aborted":       {WithNotifyReceiver("a", &fakeSink{}), WithNotifyEscalation(NotificationRunAborted, EscalationStep{Receiver: "a", Deadline: time.Minute})},
		"escalation deadline not positive": {WithNotifyReceiver("a", &fakeSink{}), WithNotifyEscalation(NotificationOutcomeUnknown, EscalationStep{Receiver: "a"})},
		"escalation chain too long": {WithNotifyReceiver("a", &fakeSink{}), WithNotifyEscalation(NotificationOutcomeUnknown,
			EscalationStep{Receiver: "a", Deadline: time.Minute}, EscalationStep{Receiver: "a", Deadline: time.Minute}, EscalationStep{Receiver: "a", Deadline: time.Minute},
			EscalationStep{Receiver: "a", Deadline: time.Minute}, EscalationStep{Receiver: "a", Deadline: time.Minute})},
		"escalation set twice": {WithNotifyReceiver("a", &fakeSink{}), WithNotifyEscalation(NotificationOutcomeUnknown, EscalationStep{Receiver: "a", Deadline: time.Minute}),
			WithNotifyEscalation(NotificationOutcomeUnknown, EscalationStep{Receiver: "a", Deadline: time.Minute})},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := newNotifyEngine(memJournal(), &fakeSink{}, NewMemNotifyTrail(), opts, "test")
			if !errors.Is(err, ErrConfig) {
				t.Fatalf("err = %v, want ErrConfig", err)
			}
		})
	}
}

// escalationEngine is an engine whose halts escalate to "lead" after the base deadline (5 minutes),
// then to "head" 10 minutes after that, with a chain of two.
func escalationEngine(t *testing.T, j *Journal, trail NotifyTrail, clk *notifyClock, holder string, sinks map[string]Notifier, opts ...NotifyOption) *notifyEngine {
	t.Helper()
	all := []NotifyOption{
		WithNotifyReceiver("lead", sinks["lead"]), WithNotifyReceiver("head", sinks["head"]),
		WithNotifyEscalation(NotificationOutcomeUnknown,
			EscalationStep{Receiver: "lead", Deadline: 10 * time.Minute}, EscalationStep{Receiver: "head", Deadline: 20 * time.Minute}),
		WithNotifyEscalation(NotificationApprovalPending, EscalationStep{Receiver: "lead", Deadline: 10 * time.Minute}),
	}
	return testEngine(t, j, sinks["default"], trail, clk, holder, append(all, opts...)...)
}

func watch(t *testing.T, e *notifyEngine, ev notifyEvent) roundResult {
	t.Helper()
	res, err := e.round(context.Background(), watchOf(ev))
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if err := e.flush(context.Background()); err != nil {
		t.Logf("flush: %v", err)
	}
	return res
}

func TestEscalation_AnUnacknowledgedEventGoesToTheNextReceiverAtItsDeadline(t *testing.T) {
	clk := routeClock()
	def, lead, head := &fakeSink{fail: 1000}, &fakeSink{}, &fakeSink{}
	trail := NewMemNotifyTrail()
	e := escalationEngine(t, memJournal(), trail, clk, "p1", map[string]Notifier{"default": def, "lead": lead, "head": head})
	halt := haltEvent()
	_, _ = e.round(context.Background(), halt) // the sink is down: observed, claimed, not acked

	watch(t, e, halt)
	if lead.count() != 0 {
		t.Fatal("escalated before the deadline")
	}
	clk.advance(6 * time.Minute)
	res := watch(t, e, halt)
	if lead.count() != 1 {
		t.Fatalf("lead got %d notifications, want 1", lead.count())
	}
	n := lead.calls[0]
	if n.Kind != NotificationEscalated || n.Receiver != "lead" || n.Escalates == nil || n.Escalates.Key != halt.key ||
		n.Escalates.ID != NotifyID(halt.key) || n.Escalates.Level != 1 || n.Escalates.Kind != NotificationOutcomeUnknown {
		t.Fatalf("escalation = %+v / %+v", n, n.Escalates)
	}
	if n.Halt == nil || n.Halt.Op.ID != "c1" || n.Reason != "deadline" {
		t.Fatalf("the escalation lost the halt's detail: %+v", n)
	}
	if n.ID != NotifyID(EscalationKey(halt.key, 1)) || n.ID == NotifyID(halt.key) {
		t.Fatalf("escalation ID = %s", n.ID)
	}
	// It is in the trail with a receiver, a level and a deadline of its own.
	st := foldTrail(t, trail)
	es := st.Events[EscalationKey(halt.key, 1)]
	if es == nil || es.Receiver != "lead" || es.Level != 1 || es.Kind != string(NotificationEscalated) || es.Acked == nil || es.Rounds != 1 {
		t.Fatalf("escalation event = %+v", es)
	}
	if got, want := es.DeadlineAt.Sub(es.ObservedAt), 10*time.Minute; got != want {
		t.Fatalf("escalation deadline = %v after observed, want %v", got, want)
	}
	if es.Overdue != nil {
		t.Fatal("an escalation is itself escalated, not overdue-noticed")
	}
	if !res.next.IsZero() && !res.done && res.next.Before(clk.now()) {
		t.Fatalf("watch next in the past: %v", res.next)
	}
}

func TestEscalation_TheChainContinuesWhileTheEventIsUnacknowledgedAndStopsAtItsBound(t *testing.T) {
	clk := routeClock()
	def, lead, head := &fakeSink{fail: 1000}, &fakeSink{fail: 1000}, &fakeSink{fail: 1000}
	trail := NewMemNotifyTrail()
	e := escalationEngine(t, memJournal(), trail, clk, "p1", map[string]Notifier{"default": def, "lead": lead, "head": head})
	halt := haltEvent()
	_, _ = e.round(context.Background(), halt)

	clk.advance(6 * time.Minute)
	watch(t, e, halt)
	if lead.count() == 0 || head.count() != 0 {
		t.Fatalf("level 1: lead=%d head=%d", lead.count(), head.count())
	}
	clk.advance(11 * time.Minute) // past level 1's own deadline
	watch(t, e, halt)
	if head.count() == 0 {
		t.Fatal("level 2 did not fire at level 1's deadline")
	}
	st := foldTrail(t, trail)
	if es := st.Events[EscalationKey(halt.key, 2)]; es == nil || es.Receiver != "head" || es.Level != 2 {
		t.Fatalf("level 2 = %+v", es)
	}
	clk.advance(2 * time.Hour)
	res := watch(t, e, halt)
	if st := foldTrail(t, trail); st.Events[EscalationKey(halt.key, 3)] != nil {
		t.Fatal("the chain went past its bound")
	}
	if !res.done {
		t.Fatalf("an exhausted chain left its watcher running: %+v", res)
	}
}

func TestEscalation_AnAcknowledgedEventIsNotEscalated(t *testing.T) {
	clk := routeClock()
	def, lead := &fakeSink{}, &fakeSink{}
	trail := NewMemNotifyTrail()
	e := escalationEngine(t, memJournal(), trail, clk, "p1", map[string]Notifier{"default": def, "lead": lead, "head": &fakeSink{}})
	halt := haltEvent()
	mustRound(t, e, halt)
	clk.advance(time.Hour)
	res := watch(t, e, halt)
	if lead.count() != 0 || !res.done {
		t.Fatalf("an acked halt was escalated: lead=%d res=%+v", lead.count(), res)
	}
}

func TestEscalation_AClearedEventStopsTheChainAndTheEscalatedReceiverGetsTheAllClear(t *testing.T) {
	clk := routeClock()
	j := memJournal()
	def, lead, head := &fakeSink{fail: 3}, &fakeSink{}, &fakeSink{} // the default sink is down for the first round only
	trail := NewMemNotifyTrail()
	e := escalationEngine(t, j, trail, clk, "p1", map[string]Notifier{"default": def, "lead": lead, "head": head})
	halt := haltEvent()
	_, _ = e.round(context.Background(), halt)
	clk.advance(6 * time.Minute)
	watch(t, e, halt)
	if lead.count() != 1 {
		t.Fatalf("lead = %d", lead.count())
	}

	putResult(t, j, "r", "c1") // the halt is resolved
	clk.advance(30 * time.Minute)
	watch(t, e, halt)
	scanAndFlush(t, e, "r")
	if head.count() != 0 {
		t.Fatalf("head was escalated to after the halt was resolved: %d", head.count())
	}
	if st := foldTrail(t, trail); st.Events[EscalationKey(halt.key, 2)] != nil {
		t.Fatal("a level was observed after the clear")
	}
	if got := resolvedCalls(lead); len(got) != 1 || got[0].Clears.Key != EscalationKey(halt.key, 1) || got[0].Clears.Kind != NotificationEscalated {
		t.Fatalf("lead's all-clears = %+v", got)
	}
	if len(resolvedCalls(head)) != 0 {
		t.Fatal("head, never told, got an all-clear")
	}
	if got := resolvedCalls(def); len(got) != 1 || got[0].Clears.Key != halt.key {
		t.Fatalf("the default receiver's all-clears = %+v", got)
	}
}

func TestEscalation_AClearBeforeTheDeadlineMeansNoEscalation(t *testing.T) {
	clk := routeClock()
	j := memJournal()
	lead := &fakeSink{}
	trail := NewMemNotifyTrail()
	e := escalationEngine(t, j, trail, clk, "p1", map[string]Notifier{"default": &fakeSink{fail: 1000}, "lead": lead, "head": &fakeSink{}})
	halt := haltEvent()
	_, _ = e.round(context.Background(), halt)
	putResult(t, j, "r", "c1")
	clk.advance(10 * time.Minute)
	res := watch(t, e, halt)
	if lead.count() != 0 || !res.done {
		t.Fatalf("lead=%d res=%+v", lead.count(), res)
	}
}

func TestEscalation_AnApprovalAcknowledgedButUndecidedIsEscalated(t *testing.T) {
	clk := routeClock()
	j := memJournal()
	def, lead := &fakeSink{}, &fakeSink{}
	trail := NewMemNotifyTrail()
	e := escalationEngine(t, j, trail, clk, "p1", map[string]Notifier{"default": def, "lead": lead, "head": &fakeSink{}})
	ap := approvalEvent("alice", "bob")
	mustRound(t, e, ap) // delivered and acknowledged at once
	if foldTrail(t, trail).Events[ap.key].Acked == nil {
		t.Fatal("setup: the approval was not acked")
	}
	clk.advance(6 * time.Minute)
	watch(t, e, ap)
	if lead.count() != 1 || lead.calls[0].Reason != "undecided" || lead.calls[0].Approval == nil || lead.calls[0].Approval.ToolUseID != "c1" {
		t.Fatalf("lead = %+v", lead.calls)
	}
}

func TestEscalation_ADecidedApprovalIsNotEscalated(t *testing.T) {
	clk := routeClock()
	j := memJournal()
	lead := &fakeSink{}
	trail := NewMemNotifyTrail()
	e := escalationEngine(t, j, trail, clk, "p1", map[string]Notifier{"default": &fakeSink{}, "lead": lead, "head": &fakeSink{}})
	ap := approvalEvent("alice")
	mustRound(t, e, ap)
	putResult(t, j, "r", "c1")
	clk.advance(time.Hour)
	res := watch(t, e, ap)
	if lead.count() != 0 || !res.done {
		t.Fatalf("a decided approval was escalated: lead=%d res=%+v", lead.count(), res)
	}
}

func TestEscalation_AnotherProcessResumesTheChainFromTheTrail(t *testing.T) {
	ctx := context.Background()
	clk := routeClock()
	j := memJournal()
	def, lead := &fakeSink{fail: 1000}, &fakeSink{}
	trail := NewMemNotifyTrail()
	e1 := escalationEngine(t, j, trail, clk, "p1", map[string]Notifier{"default": def, "lead": lead, "head": &fakeSink{}})
	halt := haltEvent()
	_, _ = e1.round(ctx, halt)
	// The first process is gone. A second one finds the event through the trail's scan.
	clk.advance(6 * time.Minute)
	e2 := escalationEngine(t, j, trail, clk, "p2", map[string]Notifier{"default": def, "lead": lead, "head": &fakeSink{}})
	scanAndFlush(t, e2, "r")
	// The scan queued the event's watcher; run it as the worker would.
	if _, err := e2.round(ctx, watchOf(rebuilt(t, trail, halt.key))); err != nil {
		t.Fatal(err)
	}
	if err := e2.flush(ctx); err != nil {
		t.Logf("flush: %v", err)
	}
	if lead.count() != 1 {
		t.Fatalf("lead = %d, want 1", lead.count())
	}
}

func rebuilt(t *testing.T, tr NotifyTrail, key string) notifyEvent {
	t.Helper()
	es := foldTrail(t, tr).Events[key]
	ev, ok := eventFromKey(es)
	if !ok {
		t.Fatalf("eventFromKey(%s) failed", key)
	}
	return ev
}

func TestEscalation_TwoProcessesRaceToOneObservedEscalation(t *testing.T) {
	ctx := context.Background()
	clk := routeClock()
	j := memJournal()
	def, lead := &fakeSink{fail: 1000}, &fakeSink{}
	trail := NewMemNotifyTrail()
	sinks := map[string]Notifier{"default": def, "lead": lead, "head": &fakeSink{}}
	e1 := escalationEngine(t, j, trail, clk, "p1", sinks)
	e2 := escalationEngine(t, j, trail, clk, "p2", sinks)
	halt := haltEvent()
	_, _ = e1.round(ctx, halt)
	clk.advance(6 * time.Minute)
	watch(t, e1, halt)
	watch(t, e2, halt)
	if n := len(recsFor(t, trail, EscalationKey(halt.key, 1))); n < 3 {
		t.Fatalf("escalation records = %d", n)
	}
	obs := 0
	for _, r := range recsFor(t, trail, EscalationKey(halt.key, 1)) {
		if r.Kind == NotifyObserved {
			obs++
		}
	}
	if obs != 1 || lead.count() != 1 {
		t.Fatalf("observed %d times, lead notified %d times; want 1 and 1", obs, lead.count())
	}
}

func TestEscalation_EscalatedKeysParseBack(t *testing.T) {
	halt := haltEvent()
	for _, key := range []string{EscalationKey(halt.key, 2), EscalationKey(ToKey("risk", approvalEvent().key), 1)} {
		es := &NotifyEvent{Key: key, Kind: string(NotificationEscalated), RunID: "r", Receiver: "lead", Level: 1}
		ev, ok := eventFromKey(es)
		if !ok || ev.kind != NotificationEscalated || !strings.HasPrefix(key, "escalated:") || ev.origKey == "" || ev.receiver != "lead" {
			t.Fatalf("eventFromKey(%q) = %+v, %v", key, ev, ok)
		}
	}
	es := &NotifyEvent{Key: ToKey("risk", approvalEvent().key), Kind: string(NotificationApprovalPending), RunID: "r", Receiver: "risk"}
	ev, ok := eventFromKey(es)
	if !ok || ev.approval == nil || ev.approval.ToolUseID != "c1" || ev.receiver != "risk" {
		t.Fatalf("a receiver-qualified approval key: %+v, %v", ev, ok)
	}
}

// The observation of a step is checked on the trail prefix it is appended at, so a step that is
// submitted and then not due (the event acknowledged or cleared meanwhile, the deadline not reached,
// the step before it not observed) writes nothing.
func TestEscalation_AStepIsObservedOnlyWhenDueOnTheLoadedTrail(t *testing.T) {
	ctx := context.Background()
	newEng := func(t *testing.T) (*notifyEngine, *MemNotifyTrail, *notifyClock, *fakeSink, *fakeSink) {
		clk := routeClock()
		def, lead, trail := &fakeSink{fail: 1000}, &fakeSink{}, NewMemNotifyTrail()
		e := escalationEngine(t, memJournal(), trail, clk, "p1", map[string]Notifier{"default": def, "lead": lead, "head": &fakeSink{}})
		_, _ = e.round(ctx, haltEvent())
		return e, trail, clk, def, lead
	}
	step := func(e *notifyEngine, level int) notifyEvent { return e.escalationOf(haltEvent(), level) }

	t.Run("before the deadline", func(t *testing.T) {
		e, trail, _, _, lead := newEng(t)
		if _, err := e.round(ctx, step(e, 1)); err != nil {
			t.Fatal(err)
		}
		if lead.count() != 0 || foldTrail(t, trail).Events[EscalationKey(haltEvent().key, 1)] != nil {
			t.Fatal("a step was observed before its event's deadline")
		}
	})
	t.Run("event acknowledged meanwhile", func(t *testing.T) {
		e, trail, clk, def, lead := newEng(t)
		clk.advance(6 * time.Minute)
		def.mu.Lock()
		def.fail = 0
		def.mu.Unlock()
		clk.advance(time.Minute)
		if _, err := e.round(ctx, haltEvent()); err != nil { // the sink is back: the halt is acked
			t.Fatal(err)
		}
		if _, err := e.round(ctx, step(e, 1)); err != nil {
			t.Fatal(err)
		}
		if lead.count() != 0 || foldTrail(t, trail).Events[EscalationKey(haltEvent().key, 1)] != nil {
			t.Fatal("a step was observed after the event was acknowledged")
		}
	})
	t.Run("event cleared meanwhile", func(t *testing.T) {
		e, trail, clk, _, lead := newEng(t)
		putResult(t, e.j, "r", "c1")
		clk.advance(6 * time.Minute)
		_, _ = e.round(ctx, haltEvent()) // writes cleared
		if _, err := e.round(ctx, step(e, 1)); err != nil {
			t.Fatal(err)
		}
		if lead.count() != 0 || foldTrail(t, trail).Events[EscalationKey(haltEvent().key, 1)] != nil {
			t.Fatal("a step was observed after the event was cleared")
		}
	})
	t.Run("a later step without the one before", func(t *testing.T) {
		e, trail, clk, _, _ := newEng(t)
		clk.advance(time.Hour)
		if _, err := e.round(ctx, step(e, 2)); err != nil {
			t.Fatal(err)
		}
		if foldTrail(t, trail).Events[EscalationKey(haltEvent().key, 2)] != nil {
			t.Fatal("step 2 was observed with no step 1")
		}
	})
}

// The configuration in the notifications guide's worked example builds.
func TestRouting_TheGuidesWorkedExampleIsAValidConfiguration(t *testing.T) {
	_, err := newNotifyEngine(memJournal(), &fakeSink{}, NewMemNotifyTrail(), []NotifyOption{
		WithNotifyReceiver("approvers", &fakeSink{}), WithNotifyReceiver("oncall", &fakeSink{}), WithNotifyReceiver("lead", &fakeSink{}),
		WithNotifyRoute(NotificationApprovalPending, "approvers"), WithNotifyRoute(NotificationOutcomeUnknown, "oncall"),
		WithNotifyRoute(NotificationRecoveryFailing, "oncall"), WithNotifyDeadline(5 * time.Minute),
		WithNotifyEscalation(NotificationOutcomeUnknown, EscalationStep{Receiver: "lead", Deadline: 15 * time.Minute}, EscalationStep{Receiver: "default", Deadline: 30 * time.Minute}),
		WithNotifyEscalation(NotificationApprovalPending, EscalationStep{Receiver: "lead", Deadline: time.Hour}),
		WithNotifyApproverRoute("alice", "approvers"), WithNotifyApproverRoute("bob", "lead"),
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
}
