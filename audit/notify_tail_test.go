package audit_test

import (
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

func tailVerify(t *testing.T, b *trailBuilder, head audit.SignedTreeHead, pub ed25519.PublicKey, mod func(*audit.NotifyVerifyInput)) (audit.NotifyReport, error) {
	t.Helper()
	in := audit.NotifyVerifyInput{Trails: []audit.NotifyTrailExport{b.export()}, Heads: []audit.SignedTreeHead{head}, Verifier: audit.Ed25519Verifier{Pub: pub}, Now: at(60)}
	if mod != nil {
		mod(&in)
	}
	return audit.VerifyNotifications(in)
}

func TestVerifyNotifications_UnattestedTailVerdictsFailTheAudit(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	k, k2 := agent.ApprovalKey("r1", "c1", 0), agent.ApprovalKey("r1", "c2", 0)
	for name, add := range map[string]func(*trailBuilder){
		"an ack in the tail":         func(b *trailBuilder) { b.add(acked(k, 2, 3)) },
		"a clear in the tail":        func(b *trailBuilder) { b.add(cleared(k, 3, "decided")) },
		"an observation in the tail": func(b *trailBuilder) { b.add(observed(k2, agent.NotificationApprovalPending, 0, 4)) },
	} {
		b := newTrail(t).add(observed(k, agent.NotificationApprovalPending, 0, 1), claimed(k, 1))
		head := headFor(t, b, priv)
		add(b)
		rep, err := tailVerify(t, b, head, pub, nil)
		if !errors.Is(err, audit.ErrNotVerified) {
			t.Errorf("%s: err = %v, want ErrNotVerified", name, err)
			continue
		}
		found := false
		for _, e := range rep.Events {
			found = found || e.Status == audit.NotifyStatusUnattested
		}
		if !found || rep.Trails[0].Tail != 1 || rep.Trails[0].TailFirstAt.IsZero() || rep.Trails[0].TailLastAt.IsZero() {
			t.Errorf("%s: no unattested verdict or no tail range: %+v %+v", name, rep.Events, rep.Trails[0])
		}
		// The opt-in accepts the tail, and still reports it.
		rep, err = tailVerify(t, b, head, pub, func(in *audit.NotifyVerifyInput) { in.AcceptTail = true })
		if errors.Is(err, audit.ErrNotVerified) && !strings.Contains(strings.Join(rep.Problems, ";"), "overdue") || rep.Trails[0].Tail != 1 {
			t.Errorf("%s: AcceptTail: err %v tail %d", name, err, rep.Trails[0].Tail)
		}
		for _, e := range rep.Events {
			if e.Status == audit.NotifyStatusUnattested {
				t.Errorf("%s: AcceptTail still reports unattested", name)
			}
		}
	}
	// Records in the head are attested: no change to an already-complete trail.
	c := newTrail(t).complete(k, agent.NotificationApprovalPending, 0)
	h2 := headFor(t, c, priv)
	if _, err := tailVerify(t, c, h2, pub, nil); err != nil {
		t.Fatalf("a complete attested trail: %v", err)
	}
	// A tail that adds only a claim to an already-observed event changes no verdict.
	c.add(agent.NotifyRecord{Kind: agent.NotifyFailingSince, At: at(50), RunID: "r1"})
	if rep, err := tailVerify(t, c, h2, pub, nil); err != nil || rep.Trails[0].Tail != 1 {
		t.Fatalf("a harmless tail: %v", err)
	}
}

func TestVerifyNotifications_ExpectedReceivers(t *testing.T) {
	gate := agent.ApprovalKey("r1", "c1", 0)
	risk, fin := agent.ToKey("risk", gate), agent.ToKey("finance", gate)
	exp := func(rs ...string) func(*audit.NotifyVerifyInput) {
		return func(in *audit.NotifyVerifyInput) {
			in.ExpectedReceivers = rs
			in.Now = at(2000)
			in.MaxClockSkew = 10 * time.Second
		}
	}
	b := newTrail(t)
	deliver(b, obsTo(risk, agent.NotificationApprovalPending, 1, "risk"), 2, 3)
	// Not given: not checked.
	if _, err := verifyNotify(t, b, func(in *audit.NotifyVerifyInput) { in.Now = at(2000); in.MaxClockSkew = 10 * time.Second }); err != nil {
		t.Fatal(err)
	}
	rep, err := verifyNotify(t, b, exp("risk", "finance"))
	if !errors.Is(err, audit.ErrNotVerified) || notifyStatusOf(t, rep, fin) != audit.NotifyStatusNoDelivery {
		t.Fatalf("err %v report %+v", err, rep.Events)
	}
	deliver(b, obsTo(fin, agent.NotificationApprovalPending, 1, "finance"), 2, 3)
	if rep, err := verifyNotify(t, b, exp("risk", "finance")); err != nil {
		t.Fatalf("every receiver reached: %v %v", err, rep.Problems)
	}
	// An unrouted event is a delivery to the default receiver.
	d := newTrail(t).complete(agent.HaltedKey("r1", agent.OpTool, "c1", 1), agent.NotificationOutcomeUnknown, 0)
	if rep, err := verifyNotify(t, d, exp(agent.DefaultReceiver)); err != nil {
		t.Fatalf("default reached: %v %v", err, rep.Problems)
	}
	rep, err = verifyNotify(t, d, exp(agent.DefaultReceiver, "oncall"))
	if !errors.Is(err, audit.ErrNotVerified) || notifyStatusOf(t, rep, agent.ToKey("oncall", agent.HaltedKey("r1", agent.OpTool, "c1", 1))) != audit.NotifyStatusNoDelivery {
		t.Fatalf("err %v %+v", err, rep.Events)
	}
	// Within the grace period the missing delivery is not yet due.
	rep, err = verifyNotify(t, d, func(in *audit.NotifyVerifyInput) { in.ExpectedReceivers = []string{"oncall"}; in.Now = at(30) })
	if err != nil || notifyStatusOf(t, rep, agent.ToKey("oncall", agent.HaltedKey("r1", agent.OpTool, "c1", 1))) != audit.NotifyStatusNotYetDue {
		t.Fatalf("grace: %v %+v", err, rep.Events)
	}
	// Escalation steps, overdue notices and all-clears are not routed events of their own.
	e := newTrail(t)
	deliver(e, obsTo(gate, agent.NotificationApprovalPending, 1, ""), 2, 3)
	deliver(e, step(gate, 1, 310, 600, "lead"), 311, 312)
	if rep, err := verifyNotify(t, e, exp(agent.DefaultReceiver)); err != nil {
		t.Fatalf("a step must not need every receiver: %v %v", err, rep.Problems)
	}
}
