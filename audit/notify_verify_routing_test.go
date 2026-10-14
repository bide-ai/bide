package audit_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// Routing and escalation in the verifier: records name a receiver, an approval can be delivered to
// several (each under its own key), and an escalation step is an event of its own that is valid only
// when it is due and its event is not satisfied.

func obsTo(key string, kind agent.NotificationKind, obsSec int, receiver string) agent.NotifyRecord {
	r := observed(key, kind, obsSec-1, obsSec)
	r.Receiver = receiver
	return r
}

// step is the observed record of step level of the chain of orig, at sec, with a deadline of dl seconds.
func step(orig string, level, sec, dl int, receiver string) agent.NotifyRecord {
	k := agent.EscalationKey(orig, level)
	return agent.NotifyRecord{Kind: agent.NotifyObserved, Key: k, At: at(sec), EventKind: string(agent.NotificationEscalated),
		EventAt: at(sec), DeadlineAt: at(sec + dl), Receiver: receiver, Level: level}
}

func deliver(b *trailBuilder, rec agent.NotifyRecord, sentSec, ackSec int) {
	b.add(rec, claimed(rec.Key, int(rec.At.Sub(t0)/time.Second)), acked(rec.Key, sentSec, ackSec))
}

func problemWith(t *testing.T, b *trailBuilder, want string) {
	t.Helper()
	rep, err := verifyNotify(t, b, func(in *audit.NotifyVerifyInput) { in.Now = at(2000); in.MaxClockSkew = 10 * time.Second })
	if !errors.Is(err, audit.ErrNotVerified) {
		t.Fatalf("err = %v, want ErrNotVerified; report %+v", err, rep.Problems)
	}
	for _, p := range rep.Problems {
		if strings.Contains(p, want) {
			return
		}
	}
	t.Fatalf("no problem mentions %q: %v", want, rep.Problems)
}

func TestVerifyNotifications_RoutedAndEscalatedTrailVerifies(t *testing.T) {
	gate := agent.ApprovalKey("r1", "c1", 0)
	b := newTrail(t)
	deliver(b, obsTo(gate, agent.NotificationApprovalPending, 1, "approvers"), 2, 3)
	// The gate is still undecided when its deadline (301s) passes: an escalation to lead, then head.
	deliver(b, step(gate, 1, 310, 600, "lead"), 311, 312)
	deliver(b, step(gate, 2, 911, 600, "head"), 912, 913)
	rep, err := verifyNotify(t, b, func(in *audit.NotifyVerifyInput) { in.Now = at(1000); in.MaxClockSkew = 10 * time.Second })
	if err != nil {
		t.Fatalf("err %v, problems %v", err, rep.Problems)
	}
	got := map[string]string{}
	for _, e := range rep.Events {
		got[e.Key] = e.Receiver
		if e.Status != audit.NotifyStatusAcked {
			t.Fatalf("%s is %s", e.Key, e.Status)
		}
	}
	if got[gate] != "approvers" || got[agent.EscalationKey(gate, 1)] != "lead" || got[agent.EscalationKey(gate, 2)] != "head" {
		t.Fatalf("receivers = %v", got)
	}
	for _, e := range rep.Events {
		if e.Key == agent.EscalationKey(gate, 2) && e.Level != 2 {
			t.Fatalf("level = %d", e.Level)
		}
	}
	if c := rep.CountsByReceiver(); c["lead"][audit.NotifyStatusAcked] != 1 || c["approvers"][audit.NotifyStatusAcked] != 1 {
		t.Fatalf("CountsByReceiver = %v", c)
	}
}

func TestVerifyNotifications_AFanOutIsOneVerdictPerReceiver(t *testing.T) {
	gate := agent.ApprovalKey("r1", "c1", 0)
	risk, fin := agent.ToKey("risk", gate), agent.ToKey("finance", gate)
	b := newTrail(t)
	deliver(b, obsTo(risk, agent.NotificationApprovalPending, 1, "risk"), 2, 3)
	b.add(obsTo(fin, agent.NotificationApprovalPending, 1, "finance"), claimed(fin, 1)) // finance never acknowledged
	rep, err := verifyNotify(t, b, func(in *audit.NotifyVerifyInput) { in.Now = at(2) })
	if err != nil {
		t.Fatal(err)
	}
	if statusOf(t, rep, risk) != audit.NotifyStatusAcked || statusOf(t, rep, fin) != audit.NotifyStatusPending {
		t.Fatalf("report = %+v", rep.Events)
	}
	// Past the deadline the receiver that never acknowledged is the problem, and the other is not.
	rep, err = verifyNotify(t, b, func(in *audit.NotifyVerifyInput) { in.Now = at(2000); in.MaxClockSkew = 10 * time.Second })
	if !errors.Is(err, audit.ErrNotVerified) || statusOf(t, rep, fin) != audit.NotifyStatusOverdue || statusOf(t, rep, risk) != audit.NotifyStatusAcked {
		t.Fatalf("err %v report %+v", err, rep.Events)
	}
}

func TestVerifyNotifications_AJournalGateIsNotifiedWhenAnyDeliveryOfItIsInTheTrail(t *testing.T) {
	jx, _ := journalWithGate(t, "r1")
	gate := agent.ApprovalKey("r1", "c1", 0)
	later := func(in *audit.NotifyVerifyInput) {
		in.Journals = []audit.JournalExport{jx}
		in.Now = time.Now().Add(time.Hour)
	}
	b := newTrail(t)
	deliver(b, obsTo(agent.ToKey("risk", gate), agent.NotificationApprovalPending, 1, "risk"), 2, 3)
	deliver(b, obsTo(agent.ToKey("finance", gate), agent.NotificationApprovalPending, 1, "finance"), 2, 3)
	rep, err := verifyNotify(t, b, func(in *audit.NotifyVerifyInput) { later(in); in.Now = at(10) })
	if err != nil {
		t.Fatalf("a fanned-out gate is notified: %v %+v", err, rep.Problems)
	}
	for _, e := range rep.Events {
		if e.Key == gate {
			t.Fatalf("a verdict for the unrouted key of a fanned-out gate: %+v", e)
		}
	}
	// A fan-out that reached one receiver only: the other is the one the journal cannot account for.
	b2 := newTrail(t)
	deliver(b2, obsTo(agent.ToKey("risk", gate), agent.NotificationApprovalPending, 1, "risk"), 2, 3)
	if rep, err := verifyNotify(t, b2, func(in *audit.NotifyVerifyInput) { later(in); in.Now = at(10) }); err != nil {
		t.Fatalf("one delivery is a delivery (the trail cannot know the routing): %v %+v", err, rep.Problems)
	}
	// No delivery at all is still never_notified.
	rep, err = verifyNotify(t, newTrail(t), later)
	if !errors.Is(err, audit.ErrNotVerified) || statusOf(t, rep, gate) != audit.NotifyStatusNeverNotified {
		t.Fatalf("err %v report %+v", err, rep.Events)
	}
}

func TestVerifyNotifications_InvalidEscalationsAndRoutes(t *testing.T) {
	gate := agent.ApprovalKey("r1", "c1", 0)
	halt := agent.HaltedKey("r1", agent.OpTool, "c1", 1)
	t.Run("a step before its event's deadline", func(t *testing.T) {
		b := newTrail(t)
		deliver(b, obsTo(gate, agent.NotificationApprovalPending, 1, ""), 2, 3)
		b.add(step(gate, 1, 100, 600, "lead"))
		problemWith(t, b, "before")
	})
	t.Run("step 2 with no step 1", func(t *testing.T) {
		b := newTrail(t)
		deliver(b, obsTo(gate, agent.NotificationApprovalPending, 1, ""), 2, 3)
		b.add(step(gate, 2, 400, 600, "lead"))
		problemWith(t, b, "step 1")
	})
	t.Run("step 2 before step 1's deadline", func(t *testing.T) {
		b := newTrail(t)
		deliver(b, obsTo(gate, agent.NotificationApprovalPending, 1, ""), 2, 3)
		b.add(step(gate, 1, 310, 600, "lead"), claimed(agent.EscalationKey(gate, 1), 310), step(gate, 2, 400, 600, "head"))
		problemWith(t, b, "before")
	})
	t.Run("a step for an acknowledged halt", func(t *testing.T) {
		b := newTrail(t)
		deliver(b, obsTo(halt, agent.NotificationOutcomeUnknown, 1, "oncall"), 2, 3)
		b.add(step(halt, 1, 310, 600, "lead"))
		problemWith(t, b, "acknowledged")
	})
	t.Run("a step after the event was cleared", func(t *testing.T) {
		b := newTrail(t)
		b.add(obsTo(halt, agent.NotificationOutcomeUnknown, 1, "oncall"), claimed(halt, 1), cleared(halt, 20, "resolved"))
		b.add(step(halt, 1, 310, 600, "lead"))
		problemWith(t, b, "cleared")
	})
	t.Run("a step for an event not in the trail", func(t *testing.T) {
		b := newTrail(t)
		b.add(step(halt, 1, 310, 600, "lead"))
		problemWith(t, b, "not been observed")
	})
	t.Run("a level that is not the key's", func(t *testing.T) {
		b := newTrail(t)
		deliver(b, obsTo(gate, agent.NotificationApprovalPending, 1, ""), 2, 3)
		s := step(gate, 1, 310, 600, "lead")
		s.Level = 2
		b.add(s)
		problemWith(t, b, "level")
	})
	t.Run("a step past the bound of the chain", func(t *testing.T) {
		b := newTrail(t)
		deliver(b, obsTo(gate, agent.NotificationApprovalPending, 1, ""), 2, 3)
		b.add(step(gate, 5, 310, 600, "lead"))
		problemWith(t, b, "level")
	})
	t.Run("a step that is overdue", func(t *testing.T) {
		b := newTrail(t)
		deliver(b, obsTo(gate, agent.NotificationApprovalPending, 1, ""), 2, 3)
		k := agent.EscalationKey(gate, 1)
		b.add(step(gate, 1, 310, 600, "lead"), claimed(k, 310),
			agent.NotifyRecord{Kind: agent.NotifyOverdue, Key: k, At: at(1000), Latency: 690 * time.Second})
		problemWith(t, b, "never overdue")
	})
	t.Run("an invalid receiver name", func(t *testing.T) {
		b := newTrail(t)
		b.add(obsTo(gate, agent.NotificationApprovalPending, 1, "On Call!"))
		problemWith(t, b, "receiver")
	})
	t.Run("a receiver on a record that is not an observation", func(t *testing.T) {
		b := newTrail(t)
		c := claimed(gate, 1)
		c.Receiver = "risk"
		b.add(obsTo(gate, agent.NotificationApprovalPending, 1, "risk"), c)
		problemWith(t, b, "receiver")
	})
	t.Run("a fan-out key naming another receiver than the record's", func(t *testing.T) {
		b := newTrail(t)
		b.add(obsTo(agent.ToKey("risk", gate), agent.NotificationApprovalPending, 1, "finance"))
		problemWith(t, b, "receiver")
	})
	t.Run("an all-clear to another receiver than the event's", func(t *testing.T) {
		b := newTrail(t)
		deliver(b, obsTo(halt, agent.NotificationOutcomeUnknown, 1, "oncall"), 2, 3)
		cl := agent.ResolvedKey(halt)
		r := observed(cl, agent.NotificationResolved, 20, 21)
		r.DeadlineAt, r.Reason, r.Receiver = time.Time{}, "resolved", "lead"
		b.add(r)
		problemWith(t, b, "receiver")
	})
}

func TestVerifyNotifications_AnEscalationStepGetsAnAllClearLikeAnyEvent(t *testing.T) {
	gate := agent.ApprovalKey("r1", "c1", 0)
	step1 := agent.EscalationKey(gate, 1)
	b := newTrail(t)
	deliver(b, obsTo(gate, agent.NotificationApprovalPending, 1, "approvers"), 2, 3)
	deliver(b, step(gate, 1, 310, 600, "lead"), 311, 312)
	cl := observed(agent.ResolvedKey(step1), agent.NotificationResolved, 400, 401)
	cl.DeadlineAt, cl.Reason, cl.Receiver = time.Time{}, "decided", "lead"
	deliver(b, cl, 402, 403)
	if rep, err := verifyNotify(t, b, func(in *audit.NotifyVerifyInput) { in.Now = at(500) }); err != nil {
		t.Fatalf("%v %v", err, rep.Problems)
	}
}
