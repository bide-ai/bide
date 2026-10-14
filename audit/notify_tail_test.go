package audit_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
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
		if !found || rep.Trails[0].Tail != 1 || rep.Trails[0].TailSince.IsZero() || rep.Trails[0].TailAge <= 0 {
			t.Errorf("%s: no unattested verdict or no tail range: %+v %+v", name, rep.Events, rep.Trails[0])
		}
		// A bound shorter than the tail's age does not accept it.
		if _, err := tailVerify(t, b, head, pub, func(in *audit.NotifyVerifyInput) { in.MaxTailAge = time.Second }); !errors.Is(err, audit.ErrNotVerified) {
			t.Errorf("%s: a bound below the tail's age accepted it: %v", name, err)
		}
		// The opt-in accepts the tail, and still reports it.
		rep, err = tailVerify(t, b, head, pub, func(in *audit.NotifyVerifyInput) { in.MaxTailAge = time.Hour })
		if errors.Is(err, audit.ErrNotVerified) && !strings.Contains(strings.Join(rep.Problems, ";"), "overdue") || rep.Trails[0].Tail != 1 {
			t.Errorf("%s: MaxTailAge: err %v tail %d", name, err, rep.Trails[0].Tail)
		}
		for _, e := range rep.Events {
			if e.Status == audit.NotifyStatusUnattested {
				t.Errorf("%s: MaxTailAge still reports unattested", name)
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

func routing(t *testing.T, opts ...agent.NotifyOption) *agent.NotifyRouting {
	t.Helper()
	r, err := agent.RoutingOf(opts...)
	if err != nil {
		t.Fatal(err)
	}
	return &r
}

type nopSink struct{}

func (nopSink) Notify(context.Context, agent.Notification) error { return nil }

func TestVerifyNotifications_RoutingOwesEachRoutedReceiver(t *testing.T) {
	gate := agent.ApprovalKey("r1", "c1", 0)
	halt := agent.HaltedKey("r1", agent.OpTool, "c1", 1)
	risk, fin := agent.ToKey("risk", gate), agent.ToKey("finance", gate)
	opts := []agent.NotifyOption{
		agent.WithNotifyReceiver("risk", nopSink{}), agent.WithNotifyReceiver("finance", nopSink{}), agent.WithNotifyReceiver("oncall", nopSink{}),
		agent.WithNotifyApproverRoute("rita", "risk"), agent.WithNotifyApproverRoute("fin", "finance"),
		agent.WithNotifyRoute(agent.NotificationOutcomeUnknown, "oncall"),
	}
	rt := routing(t, opts...)
	with := func(extra ...func(*audit.NotifyVerifyInput)) func(*audit.NotifyVerifyInput) {
		return func(in *audit.NotifyVerifyInput) {
			in.Routing, in.Now, in.MaxClockSkew = rt, at(2000), 10*time.Second
			for _, f := range extra {
				f(in)
			}
		}
	}
	gateJ := func(in *audit.NotifyVerifyInput) {
		jx, _ := journalGateWith(t, "r1", []string{"rita", "fin"})
		in.Journals = []audit.JournalExport{jx}
	}
	// Approvals go to the approvers' receivers, halts to on-call: no false no_delivery for the others.
	b := newTrail(t)
	deliver(b, obsTo(risk, agent.NotificationApprovalPending, 1, "risk"), 2, 3)
	deliver(b, obsTo(fin, agent.NotificationApprovalPending, 1, "finance"), 2, 3)
	deliver(b, obsTo(halt, agent.NotificationOutcomeUnknown, 1, "oncall"), 2, 3)
	if rep, err := verifyNotify(t, b, with(gateJ)); err != nil {
		t.Fatalf("routed deliveries all present: %v %v", err, rep.Problems)
	}
	// The finance delivery is missing.
	b2 := newTrail(t)
	deliver(b2, obsTo(risk, agent.NotificationApprovalPending, 1, "risk"), 2, 3)
	rep, err := verifyNotify(t, b2, with(gateJ))
	if !errors.Is(err, audit.ErrNotVerified) || notifyStatusOf(t, rep, fin) != audit.NotifyStatusNoDelivery {
		t.Fatalf("err %v %+v", err, rep.Events)
	}
	// Without the journal the approvers are unknown, so an approver-routed gate is not checked.
	if rep, err := verifyNotify(t, b2, with()); err != nil {
		t.Fatalf("approvers unknown: %v %v", err, rep.Problems)
	}
	// A halt that went to the default receiver, not on-call.
	h := newTrail(t).complete(halt, agent.NotificationOutcomeUnknown, 0)
	rep, err = verifyNotify(t, h, with())
	if !errors.Is(err, audit.ErrNotVerified) || notifyStatusOf(t, rep, agent.ToKey("oncall", halt)) != audit.NotifyStatusNoDelivery {
		t.Fatalf("err %v %+v", err, rep.Events)
	}
	// Within the grace period it is not yet due.
	rep, err = verifyNotify(t, h, with(func(in *audit.NotifyVerifyInput) { in.Now = at(30) }))
	if err != nil || notifyStatusOf(t, rep, agent.ToKey("oncall", halt)) != audit.NotifyStatusNotYetDue {
		t.Fatalf("grace: %v %+v", err, rep.Events)
	}
	// No routing at all: nothing is checked.
	if _, err := verifyNotify(t, h, nil); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyNotifications_RoutingOwesDueEscalationSteps(t *testing.T) {
	gate := agent.ApprovalKey("r1", "c1", 0)
	rt := routing(t, agent.WithNotifyReceiver("lead", nopSink{}), agent.WithNotifyReceiver("head", nopSink{}),
		agent.WithNotifyEscalation(agent.NotificationApprovalPending,
			agent.EscalationStep{Receiver: "lead", Deadline: 10 * time.Minute}, agent.EscalationStep{Receiver: "head", Deadline: 10 * time.Minute}))
	run := func(b *trailBuilder, now int) (audit.NotifyReport, error) {
		return verifyNotify(t, b, func(in *audit.NotifyVerifyInput) { in.Routing, in.Now, in.MaxClockSkew = rt, at(now), 10*time.Second })
	}
	b := newTrail(t)
	deliver(b, obsTo(gate, agent.NotificationApprovalPending, 1, ""), 2, 3) // deadline 301s, undecided
	if _, err := run(b, 200); err != nil {
		t.Fatalf("no step is due before the deadline: %v", err)
	}
	rep, err := run(b, 1000)
	if !errors.Is(err, audit.ErrNotVerified) || notifyStatusOf(t, rep, agent.EscalationKey(gate, 1)) != audit.NotifyStatusNoDelivery {
		t.Fatalf("step 1 due, not delivered: %v %+v", err, rep.Events)
	}
	// Step 1 delivered, to the wrong receiver.
	wrong := newTrail(t)
	deliver(wrong, obsTo(gate, agent.NotificationApprovalPending, 1, ""), 2, 3)
	deliver(wrong, step(gate, 1, 310, 600, "head"), 311, 312)
	if _, err := run(wrong, 1000); !errors.Is(err, audit.ErrNotVerified) {
		t.Fatalf("step 1 to the wrong receiver: %v", err)
	}
	// Step 1 delivered; step 2 is due at 910s and not delivered.
	b.add()
	deliver(b, step(gate, 1, 310, 600, "lead"), 311, 312)
	if _, err := run(b, 800); err != nil {
		t.Fatalf("step 2 not yet due: %v", err)
	}
	rep, err = run(b, 1500)
	if !errors.Is(err, audit.ErrNotVerified) || notifyStatusOf(t, rep, agent.EscalationKey(gate, 2)) != audit.NotifyStatusNoDelivery {
		t.Fatalf("step 2 due: %v %+v", err, rep.Events)
	}
	deliver(b, step(gate, 2, 911, 600, "head"), 912, 913)
	if rep, err := run(b, 1500); err != nil {
		t.Fatalf("both steps delivered: %v %v", err, rep.Problems)
	}
	// A cleared event stops the chain.
	c := newTrail(t)
	deliver(c, obsTo(gate, agent.NotificationApprovalPending, 1, ""), 2, 3)
	c.add(agent.NotifyRecord{Kind: agent.NotifyObserved, Key: agent.ResolvedKey(gate), At: at(6), EventKind: "resolved", EventAt: at(5), Reason: "decided"})
	rep, _ = run(c, 1500)
	for _, e := range rep.Events {
		if e.Status == audit.NotifyStatusNoDelivery {
			t.Fatalf("a cleared event owes no step: %+v", e)
		}
	}
}

// journalGateWith pauses an m-of-n gate over approvers (1 of them needed) and exports the journal.
func journalGateWith(t *testing.T, run string, approvers []string) (audit.JournalExport, *agent.Journal) {
	t.Helper()
	pol := &agent.ApprovalPolicy{Need: 1, Approvers: approvers}
	tool := agent.MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithApproval(pol))
	verifiers := func(id string) (agent.ApproverVerifier, bool) {
		return quorumVerifier(id), slices.Contains(approvers, id)
	}
	j, _ := agent.NewJournal(agent.NewMemStore())
	a, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{}`)), j, agent.WithTools(tool), agent.WithApproverVerifiers(verifiers))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), run, agent.UserText("go")); !errors.As(err, new(*agent.ApprovalPending)) {
		t.Fatalf("Run = %v", err)
	}
	x, err := audit.ExportJournal(context.Background(), j, run)
	if err != nil {
		t.Fatal(err)
	}
	return x, j
}

// An escalation chain stops when the journal shows the event resolved, and, for a halt, when it is
// acknowledged.
func TestVerifyNotifications_RoutingEscalationStops(t *testing.T) {
	ctx := context.Background()
	rt := routing(t, agent.WithNotifyReceiver("lead", nopSink{}),
		agent.WithNotifyEscalation(agent.NotificationApprovalPending, agent.EscalationStep{Receiver: "lead", Deadline: time.Minute}),
		agent.WithNotifyEscalation(agent.NotificationOutcomeUnknown, agent.EscalationStep{Receiver: "lead", Deadline: time.Minute}))
	gate := agent.ApprovalKey("r1", "c1", 0)
	mem := agent.NewMemStore()
	j, _ := agent.NewJournal(mem)
	tool := agent.MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithApproval(agent.SingleApproval()))
	a, _ := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{}`)), j, agent.WithTools(tool))
	if _, err := a.Run(ctx, "r1", agent.UserText("go")); !errors.As(err, new(*agent.ApprovalPending)) {
		t.Fatal(err)
	}
	b := newTrail(t)
	deliver(b, obsTo(gate, agent.NotificationApprovalPending, 1, ""), 2, 3)
	run := func(jx audit.JournalExport) error {
		_, err := verifyNotify(t, b, func(in *audit.NotifyVerifyInput) {
			in.Routing, in.Now, in.MaxClockSkew, in.Journals = rt, time.Now().Add(time.Hour), 10*time.Second, []audit.JournalExport{jx}
		})
		return err
	}
	jx, _ := audit.ExportJournal(ctx, j, "r1")
	if err := run(jx); !errors.Is(err, audit.ErrNotVerified) {
		t.Fatalf("an undecided gate owes step 1: %v", err)
	}
	if _, _, err := mem.Insert(ctx, "r1", agent.ToolResultStep("c1"), []byte(`{"name":"`+agent.ToolResultStep("c1")+`","kind":"tool_result","tool_use_id":"c1"}`)); err != nil {
		t.Fatal(err)
	}
	jx, _ = audit.ExportJournal(ctx, j, "r1")
	if err := run(jx); err != nil && strings.Contains(err.Error(), "escalat") {
		t.Fatalf("a decided gate owes no step: %v", err)
	}
	// An acknowledged halt owes no step; an unacknowledged one does.
	halt := agent.HaltedKey("r1", agent.OpTool, "c9", 1)
	h := newTrail(t).add(observed(halt, agent.NotificationOutcomeUnknown, 0, 1), claimed(halt, 1), acked(halt, 2, 3))
	if _, err := verifyNotify(t, h, func(in *audit.NotifyVerifyInput) { in.Routing, in.Now, in.MaxClockSkew = rt, at(2000), 10*time.Second }); err != nil {
		t.Fatalf("an acknowledged halt owes no step: %v", err)
	}
	u := newTrail(t).add(observed(halt, agent.NotificationOutcomeUnknown, 0, 1), claimed(halt, 1))
	if _, err := verifyNotify(t, u, func(in *audit.NotifyVerifyInput) { in.Routing, in.Now, in.MaxClockSkew = rt, at(2000), 10*time.Second }); err == nil {
		t.Fatal("an unacknowledged halt owes a step")
	}
}

// A forged ack stamped within a nonzero bound is accepted, without a signature: the documented cost
// of the opt-in. The default (0) rejects it, and so does a bound it is older than.
func TestVerifyNotifications_ARecentForgedTailRecordIsTrustedUnderABound(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	k := agent.ApprovalKey("r1", "c1", 0)
	b := newTrail(t).add(observed(k, agent.NotificationApprovalPending, 0, 1), claimed(k, 1))
	head := headFor(t, b, priv)
	b.add(acked(k, 55, 58)) // forged, stamped just before Now (at(60))
	if _, err := tailVerify(t, b, head, pub, nil); !errors.Is(err, audit.ErrNotVerified) {
		t.Fatalf("default: %v", err)
	}
	if _, err := tailVerify(t, b, head, pub, func(in *audit.NotifyVerifyInput) { in.MaxTailAge = time.Minute }); err != nil {
		t.Fatalf("within the bound it is trusted: %v", err)
	}
}

// The bound is judged per event, on the tail records of that event: an old record of another event
// does not make a recent one unattested, and an old record of its own does (the status, not only the
// trail-level violation).
func TestVerifyNotifications_TheBoundIsPerEvent(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	k1, k2 := agent.ApprovalKey("r1", "c1", 0), agent.ApprovalKey("r1", "c2", 0)
	b := newTrail(t).add(observed(k1, agent.NotificationApprovalPending, 0, 1), claimed(k1, 1), observed(k2, agent.NotificationApprovalPending, 0, 1), claimed(k2, 1))
	head := headFor(t, b, priv)
	b.add(acked(k1, 2, 3), acked(k2, 55, 58)) // k1's ack is 57s old at Now, k2's 2s
	rep, _ := tailVerify(t, b, head, pub, func(in *audit.NotifyVerifyInput) { in.MaxTailAge = 10 * time.Second })
	if notifyStatusOf(t, rep, k1) != audit.NotifyStatusUnattested {
		t.Fatalf("k1 rests on an old tail record: %+v", rep.Events)
	}
	if notifyStatusOf(t, rep, k2) != audit.NotifyStatusAcked {
		t.Fatalf("k2 rests on a recent one: %+v", rep.Events)
	}
}

// A keyless record in the tail (recovered, failing_since) changes the recovery state, which decides
// whether a recovery_failing event is owed: under the strict default that is unattested, and a
// recovery failure that a tail-only `recovered` silenced must not pass.
func TestVerifyNotifications_KeylessTailRecordsAreUnattested(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	fail := agent.NotifyRecord{Kind: agent.NotifyFailingSince, At: at(0), RunID: "r1", Reason: "storage"}
	rec := agent.NotifyRecord{Kind: agent.NotifyRecovered, At: at(3000)}
	late := func(in *audit.NotifyVerifyInput) { in.Now = at(4000) }
	for name, tc := range map[string]struct {
		head, tail []agent.NotifyRecord
	}{
		"a recovered only in the tail":                     {[]agent.NotifyRecord{fail}, []agent.NotifyRecord{rec}},
		"an episode closed and another opened in the tail": {[]agent.NotifyRecord{fail}, []agent.NotifyRecord{rec, {Kind: agent.NotifyFailingSince, At: at(3100), RunID: "r1", Reason: "storage"}}},
		"a failing_since only in the tail":                 {nil, []agent.NotifyRecord{{Kind: agent.NotifyFailingSince, At: at(3500), RunID: "r1", Reason: "storage"}}},
	} {
		b := newTrail(t)
		if len(tc.head) == 0 { // a head needs one record: an unrelated complete event
			b.complete(agent.ApprovalKey("r1", "c0", 0), agent.NotificationApprovalPending, 0)
		}
		b.add(tc.head...)
		head := headFor(t, b, priv)
		b.add(tc.tail...)
		rep, err := tailVerify(t, b, head, pub, late)
		found := false
		for _, e := range rep.Events {
			found = found || e.Status == audit.NotifyStatusUnattested
		}
		if !errors.Is(err, audit.ErrNotVerified) || !found {
			t.Errorf("%s: err %v, events %+v", name, err, rep.Events)
		}
		// Within a bound that covers the record, it is trusted (documented).
		if _, err := tailVerify(t, b, head, pub, func(in *audit.NotifyVerifyInput) { late(in); in.MaxTailAge = 2 * time.Hour }); err != nil && strings.Contains(err.Error(), "unattested") {
			t.Errorf("%s: within the bound: %v", name, err)
		}
	}
	// A keyless record already under the head is attested: no unattested verdict.
	b := newTrail(t).add(fail, rec)
	head := headFor(t, b, priv)
	rep, _ := tailVerify(t, b, head, pub, late)
	for _, e := range rep.Events {
		if e.Status == audit.NotifyStatusUnattested {
			t.Fatalf("attested recovery state flagged: %+v", e)
		}
	}
}
