package audit_test

import (
	"bytes"
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

var t0 = time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)

const trailName = "notify:r1"

// trailBuilder writes records into a MemEventStore through the real adapter, as a notifier does.
type trailBuilder struct {
	t  *testing.T
	es *audit.MemEventStore
	tr *audit.NotifyTrail
	n  int
}

func newTrail(t *testing.T) *trailBuilder {
	es := audit.NewMemEventStore()
	return &trailBuilder{t: t, es: es, tr: audit.NewNotifyTrail(es)}
}

func (b *trailBuilder) add(recs ...agent.NotifyRecord) *trailBuilder {
	b.t.Helper()
	for _, r := range recs {
		if r.RunID == "" && r.Kind != agent.NotifyFailingSince && r.Kind != agent.NotifyRecovered {
			r.RunID = "r1"
		}
		if err := b.tr.Append(context.Background(), trailName, b.n, r); err != nil {
			b.t.Fatal(err)
		}
		b.n++
	}
	return b
}

func (b *trailBuilder) export() audit.NotifyTrailExport {
	b.t.Helper()
	x, err := audit.ExportNotifyTrail(context.Background(), b.es, trailName)
	if err != nil {
		b.t.Fatal(err)
	}
	return x
}

func at(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }

func observed(key string, kind agent.NotificationKind, eventSec, obsSec int) agent.NotifyRecord {
	return agent.NotifyRecord{Kind: agent.NotifyObserved, Key: key, At: at(obsSec), EventKind: string(kind), EventAt: at(eventSec), DeadlineAt: at(obsSec).Add(5 * time.Minute)}
}

func claimed(key string, sec int) agent.NotifyRecord {
	return agent.NotifyRecord{Kind: agent.NotifyClaimed, Key: key, At: at(sec), Holder: "p1", Until: at(sec).Add(2 * time.Minute)}
}

func acked(key string, sentSec, sec int) agent.NotifyRecord {
	return agent.NotifyRecord{Kind: agent.NotifyAcked, Key: key, At: at(sec), FirstSentAt: at(sentSec), Attempts: 2, Status: 200,
		Auth: &agent.NotifyAuth{Signature: "hmac-sha256", SecretSlot: "current", MTLS: true, TLSVersion: "1.3"}}
}

func cleared(key string, sec int, reason string) agent.NotifyRecord {
	return agent.NotifyRecord{Kind: agent.NotifyCleared, Key: key, At: at(sec), Reason: reason}
}

// complete is an event observed one second after it began, claimed, and acknowledged a second later.
func (b *trailBuilder) complete(key string, kind agent.NotificationKind, eventSec int) *trailBuilder {
	return b.add(observed(key, kind, eventSec, eventSec+1), claimed(key, eventSec+1), acked(key, eventSec+2, eventSec+2))
}

func verifyNotify(t *testing.T, b *trailBuilder, mod func(*audit.NotifyVerifyInput)) (audit.NotifyReport, error) {
	t.Helper()
	in := audit.NotifyVerifyInput{Trails: []audit.NotifyTrailExport{b.export()}, Unanchored: true, Now: at(60)}
	if mod != nil {
		mod(&in)
	}
	return audit.VerifyNotifications(in)
}

func notifyStatusOf(t *testing.T, rep audit.NotifyReport, key string) audit.NotifyStatus {
	t.Helper()
	for _, e := range rep.Events {
		if e.Key == key {
			return e.Status
		}
	}
	t.Fatalf("no verdict for %q in %+v", key, rep.Events)
	return ""
}

func TestVerifyNotifications_CompleteTrail(t *testing.T) {
	key := agent.ApprovalKey("r1", "c1", 0)
	rep, err := verifyNotify(t, newTrail(t).complete(key, agent.NotificationApprovalPending, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Events) != 1 {
		t.Fatalf("events = %+v", rep.Events)
	}
	e := rep.Events[0]
	if e.Status != audit.NotifyStatusAcked || e.Attempts != 2 || !e.AckedAt.Equal(at(2)) || !e.ObservedAt.Equal(at(1)) || !e.EventAt.Equal(at(0)) ||
		e.Detection != time.Second || e.Delivery != time.Second || e.Total != 2*time.Second || e.Auth == nil || !e.Auth.MTLS || e.Rounds != 1 {
		t.Fatalf("verdict = %+v", e)
	}
	if rep.Trails[0].Records != 3 || rep.Trails[0].Anchored {
		t.Fatalf("trail = %+v", rep.Trails[0])
	}
}

func TestVerifyNotifications_LateAck(t *testing.T) {
	key := agent.HaltedKey("r1", agent.OpTool, "c1", 1)
	b := newTrail(t).add(observed(key, agent.NotificationOutcomeUnknown, 0, 1), claimed(key, 1), acked(key, 400, 400))
	rep, err := verifyNotify(t, b, nil)
	if !errors.Is(err, audit.ErrNotVerified) {
		t.Fatalf("err = %v, want ErrNotVerified", err)
	}
	if notifyStatusOf(t, rep, key) != audit.NotifyStatusAckedLate || len(rep.Problems) != 1 || !strings.Contains(rep.Problems[0], "deadline") {
		t.Fatalf("report = %+v", rep)
	}
}

func TestVerifyNotifications_BoundLaterThanTheDeadlineIsEnforced(t *testing.T) {
	key := agent.ApprovalKey("r1", "c1", 0)
	// Observed 100s after the event began, acknowledged at 110s: within the 5 minute deadline.
	b := newTrail(t).add(observed(key, agent.NotificationApprovalPending, 0, 100), claimed(key, 100), acked(key, 110, 110))
	if _, err := verifyNotify(t, b, nil); err != nil {
		t.Fatalf("without a bound: %v", err)
	}
	rep, err := verifyNotify(t, b, func(in *audit.NotifyVerifyInput) { in.MaxAck = time.Minute })
	if !errors.Is(err, audit.ErrNotVerified) || notifyStatusOf(t, rep, key) != audit.NotifyStatusAckedLate {
		t.Fatalf("with a 1m bound: status %s err %v", notifyStatusOf(t, rep, key), err)
	}
	if _, err := verifyNotify(t, b, func(in *audit.NotifyVerifyInput) { in.MaxAck = 2 * time.Minute }); err != nil {
		t.Fatalf("with a 2m bound: %v", err)
	}
}

func TestVerifyNotifications_OpenEvents(t *testing.T) {
	pend, fail, over := agent.ApprovalKey("r1", "p", 0), agent.ApprovalKey("r1", "f", 0), agent.ApprovalKey("r1", "o", 0)
	b := newTrail(t).add(
		observed(pend, agent.NotificationApprovalPending, 0, 1), claimed(pend, 1),
		observed(fail, agent.NotificationApprovalPending, 0, 1), claimed(fail, 1),
		agent.NotifyRecord{Kind: agent.NotifyFailing, Key: fail, At: at(5), Failures: 8},
		observed(over, agent.NotificationApprovalPending, 0, 1), claimed(over, 1))
	rep, err := verifyNotify(t, b, nil)
	if err != nil {
		t.Fatalf("open events within their deadline are not problems: %v", err)
	}
	if notifyStatusOf(t, rep, pend) != audit.NotifyStatusPending || notifyStatusOf(t, rep, fail) != audit.NotifyStatusFailing {
		t.Fatalf("report = %+v", rep.Events)
	}
	// Past the deadline they are overdue, and that fails.
	rep, err = verifyNotify(t, b, func(in *audit.NotifyVerifyInput) { in.Now = at(1).Add(6 * time.Minute) })
	if !errors.Is(err, audit.ErrNotVerified) || notifyStatusOf(t, rep, pend) != audit.NotifyStatusOverdue || notifyStatusOf(t, rep, over) != audit.NotifyStatusOverdue {
		t.Fatalf("err %v report %+v", err, rep.Events)
	}
}

func TestVerifyNotifications_ClearedEvents(t *testing.T) {
	before, after := agent.ApprovalKey("r1", "b", 0), agent.ApprovalKey("r1", "a", 0)
	b := newTrail(t).add(
		observed(before, agent.NotificationApprovalPending, 0, 1), cleared(before, 2, "decided"),
		observed(after, agent.NotificationApprovalPending, 0, 1), claimed(after, 1), cleared(after, 5, "decided"))
	rep, err := verifyNotify(t, b, nil)
	if err != nil || notifyStatusOf(t, rep, agent.ResolvedKey(after)) != audit.NotifyStatusNotYetDue {
		t.Fatalf("an all-clear within the grace period is not yet late: %v %+v", err, rep.Events)
	}
	late := func(in *audit.NotifyVerifyInput) { in.Now = at(60).Add(time.Hour) }
	rep, err = verifyNotify(t, b, late)
	if !errors.Is(err, audit.ErrNotVerified) {
		t.Fatalf("a cleared event whose delivery was attempted owes an all-clear: %v", err)
	}
	if notifyStatusOf(t, rep, before) != audit.NotifyStatusClearedBeforeNotification || notifyStatusOf(t, rep, after) != audit.NotifyStatusClearedUnacknowledged {
		t.Fatalf("report = %+v", rep.Events)
	}
	if notifyStatusOf(t, rep, agent.ResolvedKey(after)) != audit.NotifyStatusNeverNotified {
		t.Fatalf("the missing all-clear is not reported: %+v", rep.Events)
	}
	if len(rep.Problems) != 1 {
		t.Fatalf("problems = %v", rep.Problems)
	}
	// With the all-clear delivered the trail is complete.
	rk := agent.ResolvedKey(after)
	b.add(agent.NotifyRecord{Kind: agent.NotifyObserved, Key: rk, At: at(6), EventKind: string(agent.NotificationResolved), EventAt: at(5), Reason: "decided"},
		claimed(rk, 6), acked(rk, 7, 7))
	if _, err := verifyNotify(t, b, late); err != nil {
		t.Fatalf("with the all-clear: %v", err)
	}
}

func TestVerifyNotifications_OverdueNoticeIsOwed(t *testing.T) {
	key := agent.ApprovalKey("r1", "c1", 0)
	b := newTrail(t).add(observed(key, agent.NotificationApprovalPending, 0, 1), claimed(key, 1),
		agent.NotifyRecord{Kind: agent.NotifyOverdue, Key: key, At: at(1).Add(6 * time.Minute), Latency: 6 * time.Minute})
	in := func(in *audit.NotifyVerifyInput) { in.Now = at(1).Add(30 * time.Minute) }
	rep, err := verifyNotify(t, b, in)
	if !errors.Is(err, audit.ErrNotVerified) || notifyStatusOf(t, rep, agent.OverdueKey(key)) != audit.NotifyStatusNeverNotified {
		t.Fatalf("err %v report %+v", err, rep.Events)
	}
}

func TestVerifyNotifications_RecoveryEpisodeOwesAnEvent(t *testing.T) {
	b := newTrail(t).add(agent.NotifyRecord{Kind: agent.NotifyFailingSince, At: at(0), RunID: "r1", Reason: "storage"})
	rep, err := verifyNotify(t, b, func(in *audit.NotifyVerifyInput) { in.Now = at(0).Add(10 * time.Minute) })
	if err != nil || len(rep.Events) != 0 {
		t.Fatalf("an episode shorter than the threshold owes nothing: %v %+v", err, rep.Events)
	}
	key := agent.RecoveryKey("r1", 0)
	rep, err = verifyNotify(t, b, func(in *audit.NotifyVerifyInput) { in.Now = at(0).Add(time.Hour) })
	if !errors.Is(err, audit.ErrNotVerified) || notifyStatusOf(t, rep, key) != audit.NotifyStatusNeverNotified {
		t.Fatalf("err %v report %+v", err, rep.Events)
	}
	b.complete(key, agent.NotificationRecoveryFailing, 16*60)
	if _, err := verifyNotify(t, b, func(in *audit.NotifyVerifyInput) { in.Now = at(0).Add(time.Hour) }); err != nil {
		t.Fatalf("with the event: %v", err)
	}
}

// ---- the journal cross-check ----

// journalWithGate runs an agent that pauses at an approval gate, with no notifier, and exports its
// journal: the run needs a person and nothing was told.
func journalWithGate(t *testing.T, run string) (audit.JournalExport, *agent.Journal) {
	t.Helper()
	ctx := context.Background()
	tool := agent.MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithApproval(agent.SingleApproval()))
	j, err := agent.NewJournal(agent.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{}`), agenttest.TextTurn("done")), j, agent.WithTools(tool))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, run, agent.UserText("go")); !errors.As(err, new(*agent.ApprovalPending)) {
		t.Fatalf("Run = %v, want *ApprovalPending", err)
	}
	x, err := audit.ExportJournal(ctx, j, run)
	if err != nil {
		t.Fatal(err)
	}
	return x, j
}

func TestVerifyNotifications_AnUnnotifiedGateIsCaught(t *testing.T) {
	jx, _ := journalWithGate(t, "r1")
	// The trail has an unrelated, complete event, so a check of the trail alone passes.
	b := newTrail(t).complete(agent.ApprovalKey("r1", "other", 0), agent.NotificationApprovalPending, 0)
	if _, err := verifyNotify(t, b, nil); err != nil {
		t.Fatalf("the trail alone is clean: %v", err)
	}
	later := func(in *audit.NotifyVerifyInput) {
		in.Journals = []audit.JournalExport{jx}
		in.Now = time.Now().Add(time.Hour)
	}
	rep, err := verifyNotify(t, b, later)
	gate := agent.ApprovalKey("r1", "c1", 0)
	if !errors.Is(err, audit.ErrNotVerified) || notifyStatusOf(t, rep, gate) != audit.NotifyStatusNeverNotified {
		t.Fatalf("err %v report %+v", err, rep.Events)
	}
	for _, e := range rep.Events {
		if e.Key == gate && (!e.InJournal || e.Kind != "approval_pending") {
			t.Fatalf("verdict = %+v", e)
		}
	}
	// A gate that has only just begun is not yet late.
	rep, err = verifyNotify(t, b, func(in *audit.NotifyVerifyInput) { in.Journals = []audit.JournalExport{jx}; in.Now = time.Now() })
	if err != nil || notifyStatusOf(t, rep, gate) != audit.NotifyStatusNotYetDue {
		t.Fatalf("a gate within the grace period: %v %+v", err, rep.Events)
	}
	// With no trail at all the run is still found.
	rep, err = audit.VerifyNotifications(audit.NotifyVerifyInput{Journals: []audit.JournalExport{jx}, Unanchored: true, Now: time.Now().Add(time.Hour)})
	if !errors.Is(err, audit.ErrNotVerified) || notifyStatusOf(t, rep, gate) != audit.NotifyStatusNeverNotified {
		t.Fatalf("no trail: err %v report %+v", err, rep.Events)
	}
	// Once the trail observes and acknowledges the gate it passes.
	b.complete(gate, agent.NotificationApprovalPending, 0)
	rep, err = verifyNotify(t, b, later)
	if err != nil || notifyStatusOf(t, rep, gate) != audit.NotifyStatusAcked {
		t.Fatalf("with the gate notified: %v %+v", err, rep.Events)
	}
}

// crashBeforeResultStore refuses to store a tool's result: a process that died after the effect, before
// recording what it did.
type crashBeforeResultStore struct{ agent.Store }

func (s crashBeforeResultStore) Insert(ctx context.Context, run, name string, data []byte) (agent.Entry, bool, error) {
	if strings.HasPrefix(name, "tool:") {
		return agent.Entry{}, false, errors.New("crash")
	}
	return s.Store.Insert(ctx, run, name, data)
}

func TestVerifyNotifications_AnUnnotifiedHaltIsCaught(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	charge := agent.MustFunc("charge", "", func(context.Context, struct{}) (string, error) { return "ok", nil })
	drive := func(s agent.Store) error {
		j, err := agent.NewJournal(s)
		if err != nil {
			t.Fatal(err)
		}
		a, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "charge", `{}`), agenttest.TextTurn("done")), j, agent.WithTools(charge))
		if err != nil {
			t.Fatal(err)
		}
		_, err = a.Run(ctx, "r1", agent.UserText("go"))
		return err
	}
	_ = drive(crashBeforeResultStore{mem}) // dies before the result is recorded
	if err := drive(mem); !errors.As(err, new(*agent.OutcomeUnknown)) {
		t.Fatalf("second drive = %v, want *OutcomeUnknown", err)
	}
	j, _ := agent.NewJournal(mem)
	jx, err := audit.ExportJournal(ctx, j, "r1")
	if err != nil {
		t.Fatal(err)
	}
	b := newTrail(t).complete(agent.ApprovalKey("r1", "other", 0), agent.NotificationApprovalPending, 0)
	rep, err := verifyNotify(t, b, func(in *audit.NotifyVerifyInput) {
		in.Journals = []audit.JournalExport{jx}
		in.Now = time.Now().Add(time.Hour)
	})
	if !errors.Is(err, audit.ErrNotVerified) {
		t.Fatalf("an un-notified halt passed: %+v", rep.Events)
	}
	found := false
	for _, e := range rep.Events {
		if e.Kind == "outcome_unknown" && e.Status == audit.NotifyStatusNeverNotified && e.InJournal {
			found = true
		}
	}
	if !found {
		t.Fatalf("report = %+v", rep.Events)
	}
}

// The real engine's trail, for a gate it saw, verifies against the journal with no violation.
func TestVerifyNotifications_ARealNotifierTrailVerifies(t *testing.T) {
	ctx := context.Background()
	es := audit.NewMemEventStore()
	tool := agent.MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithApproval(agent.SingleApproval()))
	j, _ := agent.NewJournal(agent.NewMemStore())
	sink := &countSink{}
	a, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{}`), agenttest.TextTurn("done")), j, agent.WithTools(tool),
		agent.WithNotifier(sink, audit.NewNotifyTrail(es), agent.WithNotifyHolder("p1")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, "r1", agent.UserText("go")); !errors.As(err, new(*agent.ApprovalPending)) {
		t.Fatalf("Run = %v", err)
	}
	if err := a.FlushNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	jx, err := audit.ExportJournal(ctx, j, "r1")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := audit.ExportNotifyTrail(ctx, es, trailName)
	if err != nil {
		t.Fatal(err)
	}
	trails := []audit.NotifyTrailExport{tx}
	for _, name := range []string{agent.OpenSegment(time.Now()), agent.OpenIndex(time.Now().UTC().Year())} {
		ox, err := audit.ExportNotifyTrail(ctx, es, name)
		if err != nil || len(ox.Leaves) == 0 {
			t.Fatalf("the notifier wrote no %s: %v", name, err)
		}
		trails = append(trails, ox)
	}
	rep, err := audit.VerifyNotifications(audit.NotifyVerifyInput{Trails: trails, Journals: []audit.JournalExport{jx}, Unanchored: true})
	if err != nil {
		t.Fatalf("%v\n%+v", err, rep)
	}
	if len(rep.Events) != 1 || rep.Events[0].Status != audit.NotifyStatusAcked || !rep.Events[0].InJournal || sink.n != 1 {
		t.Fatalf("report = %+v (sink %d)", rep.Events, sink.n)
	}
}

type countSink struct{ n int }

func (s *countSink) Notify(context.Context, agent.Notification) error { s.n++; return nil }

// ---- tamper evidence ----

func headFor(t *testing.T, b *trailBuilder, priv ed25519.PrivateKey) audit.SignedTreeHead {
	t.Helper()
	h, err := audit.SignNotifyTrail(context.Background(), b.es, trailName, audit.Ed25519Signer{Priv: priv}, at(30))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func anchored(b *trailBuilder, x audit.NotifyTrailExport, h audit.SignedTreeHead, pub ed25519.PublicKey) (audit.NotifyReport, error) {
	return audit.VerifyNotifications(audit.NotifyVerifyInput{
		Trails: []audit.NotifyTrailExport{x}, Heads: []audit.SignedTreeHead{h}, Verifier: audit.Ed25519Verifier{Pub: pub}, Now: at(60)})
}

func TestVerifyNotifications_TamperedTrailsAreCaught(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	k1, k2 := agent.ApprovalKey("r1", "c1", 0), agent.ApprovalKey("r1", "c2", 0)
	b := newTrail(t).complete(k1, agent.NotificationApprovalPending, 0).complete(k2, agent.NotificationApprovalPending, 5)
	good := b.export()
	head := headFor(t, b, priv)

	rep, err := anchored(b, good, head, pub)
	if err != nil || !rep.Trails[0].Anchored || rep.Trails[0].HeadSize != 6 || rep.Trails[0].Tail != 0 {
		t.Fatalf("untampered: %v %+v", err, rep.Trails)
	}

	cp := func() audit.NotifyTrailExport {
		x := good
		x.Leaves = slices.Clone(good.Leaves)
		return x
	}
	cases := map[string]func(*audit.NotifyTrailExport){
		"deleted ack":      func(x *audit.NotifyTrailExport) { x.Leaves = slices.Delete(x.Leaves, 2, 3) },
		"deleted last":     func(x *audit.NotifyTrailExport) { x.Leaves = x.Leaves[:5] },
		"deleted all":      func(x *audit.NotifyTrailExport) { x.Leaves = nil },
		"reordered":        func(x *audit.NotifyTrailExport) { x.Leaves[0], x.Leaves[3] = x.Leaves[3], x.Leaves[0] },
		"swapped neighbor": func(x *audit.NotifyTrailExport) { x.Leaves[1], x.Leaves[2] = x.Leaves[2], x.Leaves[1] },
		"replaced record": func(x *audit.NotifyTrailExport) {
			other := newTrail(t).complete(k1, agent.NotificationApprovalPending, 0).export()
			x.Leaves[2] = other.Leaves[2] // a differently salted ack of the same event
		},
		"flipped byte": func(x *audit.NotifyTrailExport) {
			x.Leaves[2] = bytes.Replace(x.Leaves[2], []byte("200"), []byte("201"), 1)
		},
	}
	for name, mod := range cases {
		x := cp()
		mod(&x)
		rep, err := anchored(b, x, head, pub)
		if err == nil {
			t.Errorf("%s: a tampered trail verified", name)
			continue
		}
		if len(rep.Trails) == 0 || len(rep.Trails[0].Violations) == 0 && !errors.Is(err, audit.ErrMalformed) {
			t.Errorf("%s: err = %v with no trail violation: %+v", name, err, rep.Trails)
		}
	}

	// A trail grown after the head reports how much the head does not attest, and the events that
	// rest on the tail are unattested unless the tail is accepted.
	b.complete(agent.ApprovalKey("r1", "c3", 0), agent.NotificationApprovalPending, 10)
	rep, err = anchored(b, b.export(), head, pub)
	if !errors.Is(err, audit.ErrNotVerified) || !rep.Trails[0].Anchored || rep.Trails[0].Tail != 3 {
		t.Fatalf("grown trail: %v %+v", err, rep.Trails)
	}
	rep, err = audit.VerifyNotifications(audit.NotifyVerifyInput{Trails: []audit.NotifyTrailExport{b.export()}, Heads: []audit.SignedTreeHead{head},
		Verifier: audit.Ed25519Verifier{Pub: pub}, Now: at(60), MaxTailAge: time.Hour})
	if err != nil || rep.Trails[0].Tail != 3 {
		t.Fatalf("grown trail, tail accepted: %v %+v", err, rep.Trails)
	}
}

func TestVerifyNotifications_HeadChecks(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	otherPub, _, _ := ed25519.GenerateKey(nil)
	k := agent.ApprovalKey("r1", "c1", 0)
	b := newTrail(t).complete(k, agent.NotificationApprovalPending, 0)
	x, head := b.export(), headFor(t, b, priv)

	wrongTrail := head
	wrongTrail.RunID = "notify:r2"
	lg, err := audit.LoadEventLog(context.Background(), b.es, trailName)
	if err != nil {
		t.Fatal(err)
	}
	th := lg.TreeHead(trailName, at(30).UnixNano())
	th.Kind = audit.TreeJournal // validly signed, and a commitment to a journal
	journalKind, err := audit.SignTreeHead(th, audit.Ed25519Signer{Priv: priv})
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		head audit.SignedTreeHead
		pub  ed25519.PublicKey
	}{
		"wrong key":     {head, otherPub},
		"other trail":   {wrongTrail, pub},
		"journal head":  {journalKind, pub},
		"forged signer": {head, otherPub},
	} {
		if _, err := anchored(b, x, tc.head, tc.pub); err == nil {
			t.Errorf("%s: verified", name)
		}
	}
	// A head for a trail that was not supplied: the trail was deleted.
	_, err = audit.VerifyNotifications(audit.NotifyVerifyInput{Heads: []audit.SignedTreeHead{head}, Verifier: audit.Ed25519Verifier{Pub: pub}, Now: at(60)})
	if !errors.Is(err, audit.ErrNotVerified) {
		t.Errorf("a head with no trail: %v", err)
	}
	// No head at all: not verified unless the caller says it accepts an unanchored trail.
	rep, err := audit.VerifyNotifications(audit.NotifyVerifyInput{Trails: []audit.NotifyTrailExport{x}, Verifier: audit.Ed25519Verifier{Pub: pub}, Now: at(60)})
	if !errors.Is(err, audit.ErrNotVerified) || !strings.Contains(strings.Join(rep.Trails[0].Violations, ";"), "no signed head") {
		t.Errorf("an unanchored trail passed: %v %+v", err, rep.Trails)
	}
	if _, err := audit.VerifyNotifications(audit.NotifyVerifyInput{Trails: []audit.NotifyTrailExport{x}, Now: at(60)}); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("no verifier and not unanchored = %v, want ErrConfig", err)
	}
	// A head signed in the future is refused.
	if _, err := audit.VerifyNotifications(audit.NotifyVerifyInput{Trails: []audit.NotifyTrailExport{x}, Heads: []audit.SignedTreeHead{head}, Verifier: audit.Ed25519Verifier{Pub: pub}, Now: at(-3600)}); err == nil {
		t.Error("a head from the future verified")
	}
}

// ---- strict reading ----

func TestVerifyNotifications_MalformedLeavesAreRefused(t *testing.T) {
	k := agent.ApprovalKey("r1", "c1", 0)
	good := newTrail(t).complete(k, agent.NotificationApprovalPending, 0).export()
	mut := func(i int, from, to string) audit.NotifyTrailExport {
		x := good
		x.Leaves = slices.Clone(good.Leaves)
		if !bytes.Contains(x.Leaves[i], []byte(from)) {
			t.Fatalf("leaf %d has no %q: %s", i, from, x.Leaves[i])
		}
		x.Leaves[i] = bytes.Replace(x.Leaves[i], []byte(from), []byte(to), 1)
		return x
	}
	for name, x := range map[string]audit.NotifyTrailExport{
		"unknown record kind": mut(0, `"notify_observed"`, `"notify_teleported"`),
		"unknown field":       mut(0, `"event":{`, `"event":{"zzz":1,`),
		"kind mismatch":       mut(1, `"notify_claimed"`, `"notify_acked"`),
		"duplicate key":       mut(0, `"event":{`, `"event":{"key":"x",`),
		"not an event leaf":   {Format: good.Format, Trail: good.Trail, Leaves: [][]byte{[]byte("hello")}},
		"not a notify leaf":   mut(0, `"notify_observed"`, `"tool_started"`),
	} {
		_, err := audit.VerifyNotifications(audit.NotifyVerifyInput{Trails: []audit.NotifyTrailExport{x}, Unanchored: true, Now: at(60)})
		if !errors.Is(err, audit.ErrMalformed) {
			t.Errorf("%s: err = %v, want ErrMalformed", name, err)
		}
	}
	kinds := mut(0, `"notify_observed"`, `"notify_teleported"`)
	kinds.Leaves[0] = bytes.Replace(kinds.Leaves[0], []byte(`"kind":"observed"`), []byte(`"kind":"teleported"`), 1)
	if _, err := audit.VerifyNotifications(audit.NotifyVerifyInput{Trails: []audit.NotifyTrailExport{kinds}, Unanchored: true, Now: at(60)}); !errors.Is(err, audit.ErrMalformed) || !strings.Contains(err.Error(), "not one this version reads") {
		t.Errorf("a record kind this version does not know: %v", err)
	}
	bad := good
	bad.Format = "bide.audit.notify-trail-export.v0"
	if _, err := audit.VerifyNotifications(audit.NotifyVerifyInput{Trails: []audit.NotifyTrailExport{bad}, Unanchored: true}); !errors.Is(err, audit.ErrFormat) {
		t.Errorf("format = %v, want ErrFormat", err)
	}
}

func TestVerifyNotifications_InvalidHistoriesAreRefused(t *testing.T) {
	k, k2 := agent.ApprovalKey("r1", "c1", 0), agent.ApprovalKey("r1", "c2", 0)
	obs := func() agent.NotifyRecord { return observed(k, agent.NotificationApprovalPending, 0, 1) }
	for name, tc := range map[string]struct {
		recs []agent.NotifyRecord
		want string
	}{
		"ack before observation":       {[]agent.NotifyRecord{claimed(k, 1), acked(k, 2, 2)}, "not been observed"},
		"ack without claim":            {[]agent.NotifyRecord{obs(), acked(k, 2, 2)}, "never claimed"},
		"claim after ack":              {[]agent.NotifyRecord{obs(), claimed(k, 1), acked(k, 2, 2), claimed(k, 3)}, "follows the event's acknowledgement"},
		"ack after clear":              {[]agent.NotifyRecord{obs(), cleared(k, 2, "decided"), acked(k, 3, 3)}, "follows the event's clearing"},
		"double observation":           {[]agent.NotifyRecord{obs(), obs()}, "second time"},
		"reordered in time":            {[]agent.NotifyRecord{obs(), claimed(k, 1), observed(k2, agent.NotificationApprovalPending, 0, 1), acked(k, 2, 2), claimed(k2, -3600)}, "out of order"},
		"ack with a 500":               {[]agent.NotifyRecord{obs(), claimed(k, 1), func() agent.NotifyRecord { r := acked(k, 2, 2); r.Status = 500; return r }()}, "not a 2xx"},
		"ack with no attempts":         {[]agent.NotifyRecord{obs(), claimed(k, 1), func() agent.NotifyRecord { r := acked(k, 2, 2); r.Attempts = 0; return r }()}, "attempts"},
		"ack sent after it was acked":  {[]agent.NotifyRecord{obs(), claimed(k, 1), acked(k, 9, 2)}, "first send time"},
		"clear with a made up reason":  {[]agent.NotifyRecord{obs(), cleared(k, 2, "whim")}, "reason"},
		"overdue before the deadline":  {[]agent.NotifyRecord{obs(), claimed(k, 1), agent.NotifyRecord{Kind: agent.NotifyOverdue, Key: k, At: at(20), Latency: 19 * time.Second}}, "deadline"},
		"overdue with a wrong latency": {[]agent.NotifyRecord{obs(), claimed(k, 1), agent.NotifyRecord{Kind: agent.NotifyOverdue, Key: k, At: at(1).Add(6 * time.Minute), Latency: time.Second}}, "latency"},
		"notice without an overdue":    {[]agent.NotifyRecord{obs(), observed(agent.OverdueKey(k), agent.NotificationOverdue, 1, 2)}, "overdue record"},
		"all-clear without a clear":    {[]agent.NotifyRecord{obs(), claimed(k, 1), agent.NotifyRecord{Kind: agent.NotifyObserved, Key: agent.ResolvedKey(k), At: at(3), EventKind: "resolved", EventAt: at(3), Reason: "decided"}}, "before it was acknowledged or cleared"},
		"recovered with no episode":    {[]agent.NotifyRecord{{Kind: agent.NotifyRecovered, At: at(1)}}, "not open"},
		"event kind and key differ":    {[]agent.NotifyRecord{observed(k, agent.NotificationOutcomeUnknown, 0, 1)}, "does not begin"},
		"unknown event kind":           {[]agent.NotifyRecord{observed(k, "teleport", 0, 1)}, "event kind"},
		"record of another run":        {[]agent.NotifyRecord{func() agent.NotifyRecord { r := obs(); r.RunID = "r2"; return r }()}, "names run"},
		"no deadline":                  {[]agent.NotifyRecord{func() agent.NotifyRecord { r := obs(); r.DeadlineAt = time.Time{}; return r }()}, "deadline"},
	} {
		b := newTrail(t)
		for i, r := range tc.recs { // write the raw sequence, so a history the engine would not write is stored as is
			if r.RunID == "" && r.Kind != agent.NotifyFailingSince && r.Kind != agent.NotifyRecovered {
				r.RunID = "r1"
			}
			if err := b.tr.Append(context.Background(), trailName, i, r); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		rep, err := verifyNotify(t, b, nil)
		if !errors.Is(err, audit.ErrNotVerified) {
			t.Errorf("%s: err = %v, want ErrNotVerified", name, err)
			continue
		}
		if !strings.Contains(strings.Join(rep.Trails[0].Violations, "\n"), tc.want) {
			t.Errorf("%s: violations %q do not mention %q", name, rep.Trails[0].Violations, tc.want)
		}
	}
}

func TestVerifyNotifications_Inputs(t *testing.T) {
	x := newTrail(t).export()
	if _, err := audit.VerifyNotifications(audit.NotifyVerifyInput{Trails: []audit.NotifyTrailExport{x, x}, Unanchored: true}); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("two exports of one trail = %v, want ErrConfig", err)
	}
	// An empty input verifies: there is nothing that needed a person.
	if rep, err := audit.VerifyNotifications(audit.NotifyVerifyInput{Unanchored: true}); err != nil || len(rep.Events) != 0 {
		t.Errorf("empty = %v %+v", err, rep)
	}
}

// ---- trails the real engine writes ----

// toggleSink fails (permanently, so a round does not sleep) while down is set.
type toggleSink struct {
	down bool
	n    int
}

func (s *toggleSink) Notify(context.Context, agent.Notification) error {
	if s.down {
		return agent.ErrNotifyPermanent
	}
	s.n++
	return nil
}

type stepClock struct{ now time.Time }

func (c *stepClock) Now() time.Time { return c.now }

func sweepOpts(c *stepClock) []agent.NotifyOption {
	return []agent.NotifyOption{agent.WithNotifyClock(c.Now), agent.WithNotifyHolder("sweeper"), agent.WithNotifyDeadline(time.Minute),
		agent.WithNotifyRetry(time.Millisecond, time.Millisecond), agent.WithNotifyHaltAge(time.Minute)}
}

// A sink that is down past the deadline makes the engine write overdue and a notice; the verifier
// reads that history without violation, reports the late acknowledgement as the problem it is, and
// finds the notice delivered.
func TestVerifyNotifications_TheEnginesOverdueHistoryHasNoViolations(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	j, _ := agent.NewJournal(mem)
	tool := agent.MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithApproval(agent.SingleApproval()))
	a, _ := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{}`)), j, agent.WithTools(tool))
	if _, err := a.Run(ctx, "r1", agent.UserText("go")); !errors.As(err, new(*agent.ApprovalPending)) {
		t.Fatal(err)
	}
	es := audit.NewMemEventStore()
	trail, sink := audit.NewNotifyTrail(es), &toggleSink{down: true}
	clk := &stepClock{now: time.Now()}
	sweep := func() {
		_ = agent.NotifyPending(ctx, j, sink, trail, sweepOpts(clk)...)
	}
	sweep()
	clk.now = clk.now.Add(2 * time.Minute)
	sweep() // past the deadline: overdue, and the notice cannot be delivered either
	sink.down = false
	for range 6 { // the claims of the failed rounds lapse and the backoff passes, then both are delivered
		clk.now = clk.now.Add(30 * time.Second)
		sweep()
	}

	tx, _ := audit.ExportNotifyTrail(ctx, es, trailName)
	jx, _ := audit.ExportJournal(ctx, j, "r1")
	rep, err := audit.VerifyNotifications(audit.NotifyVerifyInput{Trails: []audit.NotifyTrailExport{tx}, Journals: []audit.JournalExport{jx}, Unanchored: true, Now: clk.now})
	for _, v := range rep.Trails[0].Violations {
		t.Errorf("the engine wrote a trail the verifier calls invalid: %s", v)
	}
	gate := agent.ApprovalKey("r1", "c1", 0)
	// Both were delivered after their deadlines: the gate's one minute, the notice's one minute from
	// when it was raised.
	if notifyStatusOf(t, rep, gate) != audit.NotifyStatusAckedLate || !slices.Contains([]audit.NotifyStatus{audit.NotifyStatusAckedLate, audit.NotifyStatusOverdue}, notifyStatusOf(t, rep, agent.OverdueKey(gate))) {
		t.Fatalf("err %v events %+v", err, rep.Events)
	}
	if !errors.Is(err, audit.ErrNotVerified) || len(rep.Problems) != 2 {
		t.Fatalf("problems = %v", rep.Problems)
	}
}

// An acknowledged gate that is then decided gets an all-clear; the trail with it has no violation.
func TestVerifyNotifications_TheEnginesAllClearHistoryHasNoViolations(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	j, _ := agent.NewJournal(mem)
	tool := agent.MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithApproval(agent.SingleApproval()))
	a, _ := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{}`)), j, agent.WithTools(tool))
	if _, err := a.Run(ctx, "r1", agent.UserText("go")); !errors.As(err, new(*agent.ApprovalPending)) {
		t.Fatal(err)
	}
	es := audit.NewMemEventStore()
	trail, sink := audit.NewNotifyTrail(es), &toggleSink{}
	clk := &stepClock{now: time.Now()}
	sweep := func() {
		if err := agent.NotifyPending(ctx, j, sink, trail, sweepOpts(clk)...); err != nil {
			t.Fatal(err)
		}
	}
	sweep()
	if _, _, err := mem.Insert(ctx, "r1", agent.ToolResultStep("c1"), []byte(`{"name":"`+agent.ToolResultStep("c1")+`","kind":"tool_result","tool_use_id":"c1"}`)); err != nil {
		t.Fatal(err)
	}
	clk.now = clk.now.Add(time.Second)
	sweep()
	if sink.n != 2 {
		t.Fatalf("%d notifications, want the gate and its all-clear", sink.n)
	}
	tx, _ := audit.ExportNotifyTrail(ctx, es, trailName)
	rep, err := audit.VerifyNotifications(audit.NotifyVerifyInput{Trails: []audit.NotifyTrailExport{tx}, Unanchored: true, Now: clk.now})
	if err != nil {
		t.Fatalf("%v\n%+v", err, rep)
	}
	gate := agent.ApprovalKey("r1", "c1", 0)
	if notifyStatusOf(t, rep, gate) != audit.NotifyStatusAcked || notifyStatusOf(t, rep, agent.ResolvedKey(gate)) != audit.NotifyStatusAcked {
		t.Fatalf("events %+v", rep.Events)
	}
}

// ---- an all-clear owed after the gate was decided ----

type quorumVerifier string

func (quorumVerifier) Alg() agent.Alg            { return "test" }
func (v quorumVerifier) PublicKey() []byte       { return []byte(v) }
func (v quorumVerifier) KeyIDs() []string        { return []string{"test:" + string(v)} }
func (v quorumVerifier) Verify(m, s []byte) bool { return bytes.Equal(s, append([]byte(v+"|"), m...)) }

// allClearFixture pauses an m-of-n gate, lets a sweep notify it (acknowledged), then has two
// approvers decide it at decidedAt and the run finish, with no further sweep: the all-clear is owed.
func allClearFixture(t *testing.T, decidedAt time.Time) (*audit.MemEventStore, audit.NotifyTrailExport, audit.JournalExport) {
	t.Helper()
	ctx := context.Background()
	pol := &agent.ApprovalPolicy{Need: 2, Approvers: []string{"alice", "bob"}}
	tool := agent.MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithApproval(pol))
	verifiers := func(id string) (agent.ApproverVerifier, bool) {
		return quorumVerifier(id), id == "alice" || id == "bob"
	}
	j, _ := agent.NewJournal(agent.NewMemStore())
	a, _ := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{}`), agenttest.TextTurn("done")), j,
		agent.WithTools(tool), agent.WithApproverVerifiers(verifiers))
	var pend *agent.ApprovalPending
	if _, err := a.Run(ctx, "r1", agent.UserText("go")); !errors.As(err, &pend) {
		t.Fatalf("Run = %v", err)
	}
	es := audit.NewMemEventStore()
	if err := agent.NotifyPending(ctx, j, &toggleSink{}, audit.NewNotifyTrail(es), agent.WithNotifyHolder("sweeper")); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"alice", "bob"} {
		sig := append([]byte(who+"|"), agent.ApprovalDecisionBytes(pend.Subject(), who, true, decidedAt)...)
		err := agent.SubmitDecision(ctx, j, agent.Decision{RunID: "r1", ToolUseID: "c1", ApproverID: who, Approved: true, DecidedAt: decidedAt, Alg: "test", Signature: sig},
			agent.WithClock(func() time.Time { return decidedAt }))
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.Run(ctx, "r1", agent.UserText("go")); err != nil {
		t.Fatalf("finishing run: %v", err)
	}
	tx, _ := audit.ExportNotifyTrail(ctx, es, trailName)
	jx, err := audit.ExportJournal(ctx, j, "r1")
	if err != nil {
		t.Fatal(err)
	}
	return es, tx, jx
}

func TestVerifyNotifications_AnAllClearOwedAfterTheGateWasDecided(t *testing.T) {
	decided := time.Now().Truncate(time.Millisecond)
	_, tx, jx := allClearFixture(t, decided)
	gate := agent.ApprovalKey("r1", "c1", 0)
	run := func(now time.Time) (audit.NotifyReport, error) {
		return audit.VerifyNotifications(audit.NotifyVerifyInput{Trails: []audit.NotifyTrailExport{tx}, Journals: []audit.JournalExport{jx}, Unanchored: true, Now: now})
	}
	rep, err := run(decided.Add(time.Minute))
	if err != nil || notifyStatusOf(t, rep, agent.ResolvedKey(gate)) != audit.NotifyStatusNotYetDue {
		t.Fatalf("within the grace period: %v %+v", err, rep.Events)
	}
	rep, err = run(decided.Add(10 * time.Minute))
	if !errors.Is(err, audit.ErrNotVerified) || notifyStatusOf(t, rep, agent.ResolvedKey(gate)) != audit.NotifyStatusNeverNotified {
		t.Fatalf("after it: %v %+v", err, rep.Events)
	}
}

// A gate resolved with no time in the journal (a tool's result) owes an all-clear that cannot be
// judged late: a warning, which fails the audit only under Strict.
func TestVerifyNotifications_AnUntimedResolutionIsAWarningAndFailsUnderStrict(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	j, _ := agent.NewJournal(mem)
	tool := agent.MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithApproval(agent.SingleApproval()))
	a, _ := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{}`)), j, agent.WithTools(tool))
	if _, err := a.Run(ctx, "r1", agent.UserText("go")); !errors.As(err, new(*agent.ApprovalPending)) {
		t.Fatal(err)
	}
	es := audit.NewMemEventStore()
	if err := agent.NotifyPending(ctx, j, &toggleSink{}, audit.NewNotifyTrail(es), agent.WithNotifyHolder("sweeper")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mem.Insert(ctx, "r1", agent.ToolResultStep("c1"), []byte(`{"name":"`+agent.ToolResultStep("c1")+`","kind":"tool_result","tool_use_id":"c1"}`)); err != nil {
		t.Fatal(err)
	}
	tx, _ := audit.ExportNotifyTrail(ctx, es, trailName)
	jx, _ := audit.ExportJournal(ctx, j, "r1")
	in := audit.NotifyVerifyInput{Trails: []audit.NotifyTrailExport{tx}, Journals: []audit.JournalExport{jx}, Unanchored: true, Now: time.Now().Add(time.Hour)}
	gate := agent.ApprovalKey("r1", "c1", 0)
	rep, err := audit.VerifyNotifications(in)
	if err != nil || notifyStatusOf(t, rep, agent.ResolvedKey(gate)) != audit.NotifyStatusAllClearUntimed {
		t.Fatalf("%v %+v", err, rep.Events)
	}
	in.Strict = true
	if _, err := audit.VerifyNotifications(in); !errors.Is(err, audit.ErrNotVerified) {
		t.Fatalf("strict: %v", err)
	}
	// An unobserved decided gate is the other warning.
	in.Trails = nil
	in.Strict = false
	if rep, err := audit.VerifyNotifications(in); err != nil || notifyStatusOf(t, rep, gate) != audit.NotifyStatusUnobservedResolved {
		t.Fatalf("%v %+v", err, rep.Events)
	}
	in.Strict = true
	if _, err := audit.VerifyNotifications(in); !errors.Is(err, audit.ErrNotVerified) {
		t.Fatalf("strict, unobserved: %v", err)
	}
}

// An event observed but never claimed (no delivery attempted) and then decided owes no all-clear.
func TestVerifyNotifications_NoAllClearIsOwedForAnUnattemptedEvent(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	j, _ := agent.NewJournal(mem)
	tool := agent.MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithApproval(agent.SingleApproval()))
	a, _ := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{}`)), j, agent.WithTools(tool))
	if _, err := a.Run(ctx, "r1", agent.UserText("go")); !errors.As(err, new(*agent.ApprovalPending)) {
		t.Fatal(err)
	}
	if _, _, err := mem.Insert(ctx, "r1", agent.ToolResultStep("c1"), []byte(`{"name":"`+agent.ToolResultStep("c1")+`","kind":"tool_result","tool_use_id":"c1"}`)); err != nil {
		t.Fatal(err)
	}
	jx, _ := audit.ExportJournal(ctx, j, "r1")
	gate := agent.ApprovalKey("r1", "c1", 0)
	b := newTrail(t).add(observed(gate, agent.NotificationApprovalPending, 0, 1))
	rep, err := verifyNotify(t, b, func(in *audit.NotifyVerifyInput) {
		in.Journals = []audit.JournalExport{jx}
		in.Now = time.Now().Add(time.Hour)
	})
	for _, e := range rep.Events {
		if e.Key == agent.ResolvedKey(gate) {
			t.Fatalf("an all-clear is owed for an event never delivered: %+v (err %v)", e, err)
		}
	}
}

// ---- the open trail: segments and an index ----

func recsTrail(t *testing.T, name string, recs ...agent.NotifyRecord) audit.NotifyTrailExport {
	t.Helper()
	es := audit.NewMemEventStore()
	tr := audit.NewNotifyTrail(es)
	for i, r := range recs {
		if err := tr.Append(context.Background(), name, i, r); err != nil {
			t.Fatal(err)
		}
	}
	x, err := audit.ExportNotifyTrail(context.Background(), es, name)
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func op(k agent.NotifyRecordKind, key string, sec int) agent.NotifyRecord {
	return agent.NotifyRecord{Kind: k, Key: key, RunID: "r1", At: at(sec)}
}

func day(n int) string { return agent.OpenSegment(t0.AddDate(0, 0, n)) }

func segRec(kind agent.NotifyRecordKind, sec int, next string) agent.NotifyRecord {
	return agent.NotifyRecord{Kind: kind, At: at(sec), Next: next}
}

func checkTrails(t *testing.T, xs ...audit.NotifyTrailExport) (audit.NotifyReport, error) {
	t.Helper()
	return audit.VerifyNotifications(audit.NotifyVerifyInput{Trails: xs, Unanchored: true, Now: at(60)})
}

func violationsOf(rep audit.NotifyReport) string {
	var all []string
	for _, tr := range rep.Trails {
		for _, v := range tr.Violations {
			all = append(all, tr.Trail+": "+v)
		}
	}
	return strings.Join(all, "\n")
}

// A two-day open trail as the notifier writes it: day 0 lists two events and sees one unlisted, is
// sealed, and day 1 starts with a checkpoint of what remains.
func openChain(t *testing.T) (day0, day1, index audit.NotifyTrailExport) {
	k1, k2 := agent.ApprovalKey("r1", "c1", 0), agent.ApprovalKey("r1", "c2", 0)
	day0 = recsTrail(t, day(0),
		segRec(agent.NotifyCheckpoint, 0, ""), op(agent.NotifyWatch, k1, 1), op(agent.NotifyWatch, k2, 2), op(agent.NotifyUnwatch, k1, 3),
		agent.NotifyRecord{Kind: agent.NotifyBackfilled, At: at(4), BackfillWindow: time.Hour}, segRec(agent.NotifySeal, 86400, day(1)))
	day1 = recsTrail(t, day(1), agent.NotifyRecord{Kind: agent.NotifyCheckpoint, At: at(86400), Backfilled: true, Backfills: 1, BackfillAt: at(4), BackfillWindow: time.Hour,
		Watching: []agent.Watched{{Key: k2, RunID: "r1", At: at(2)}}}, op(agent.NotifyUnwatch, k2, 86401))
	index = recsTrail(t, agent.OpenIndex(t0.Year()), segRec(agent.NotifySegment, 0, day(0)), segRec(agent.NotifySegment, 86400, day(1)))
	return
}

func TestVerifyNotifications_TheOpenTrail(t *testing.T) {
	d0, d1, idx := openChain(t)
	if rep, err := checkTrails(t, d0, d1, idx); err != nil || len(rep.Events) != 0 {
		t.Fatalf("a valid open trail: %v\n%s", err, violationsOf(rep))
	}
	k2 := agent.ApprovalKey("r1", "c2", 0)
	for name, tc := range map[string]struct {
		xs   []audit.NotifyTrailExport
		want string
	}{
		"a checkpoint that drops an event": {[]audit.NotifyTrailExport{d0, recsTrail(t, day(1), agent.NotifyRecord{Kind: agent.NotifyCheckpoint, At: at(86400), Backfilled: true}), idx}, "does not equal"},
		"a checkpoint that adds an event": {[]audit.NotifyTrailExport{d0, recsTrail(t, day(1), agent.NotifyRecord{Kind: agent.NotifyCheckpoint, At: at(86400), Backfilled: true,
			Watching: []agent.Watched{{Key: k2, RunID: "r1", At: at(2)}, {Key: "x", RunID: "r1", At: at(2)}}}), idx}, "does not equal"},
		"a checkpoint that loses the backfill": {[]audit.NotifyTrailExport{d0, recsTrail(t, day(1), agent.NotifyRecord{Kind: agent.NotifyCheckpoint, At: at(86400),
			Watching: []agent.Watched{{Key: k2, RunID: "r1", At: at(2)}}}), idx}, "backfilled is false"},
		"a continuation with no checkpoint": {[]audit.NotifyTrailExport{d0, recsTrail(t, day(1), op(agent.NotifyUnwatch, k2, 86400)), idx}, "does not start with a checkpoint"},
		"a record after the seal":           {[]audit.NotifyTrailExport{recsTrail(t, day(0), segRec(agent.NotifyCheckpoint, 0, ""), segRec(agent.NotifySeal, 86400, day(1)), op(agent.NotifyWatch, k2, 2)), d1, idx}, "follows the seal"},
		"an unwatch of an unlisted event":   {[]audit.NotifyTrailExport{recsTrail(t, day(0), op(agent.NotifyUnwatch, k2, 1))}, "not listed"},
		"a checkpoint in the middle":        {[]audit.NotifyTrailExport{recsTrail(t, day(0), op(agent.NotifyWatch, k2, 1), segRec(agent.NotifyCheckpoint, 2, ""))}, "only the first record"},
		"a seal into an earlier day":        {[]audit.NotifyTrailExport{recsTrail(t, day(1), segRec(agent.NotifySeal, 1, day(0)))}, "not a later segment"},
		"a third backfill": {[]audit.NotifyTrailExport{d0, recsTrail(t, day(1), agent.NotifyRecord{Kind: agent.NotifyCheckpoint, At: at(86400), Backfilled: true, Backfills: 1, BackfillAt: at(4), BackfillWindow: time.Hour,
			Watching: []agent.Watched{{Key: k2, RunID: "r1", At: at(2)}}}, segRec(agent.NotifyBackfilled, 86401, ""), segRec(agent.NotifyBackfilled, 86402, "")), idx}, "recorded 3 times"},
		"a checkpoint that loses the backfill time": {[]audit.NotifyTrailExport{d0, recsTrail(t, day(1), agent.NotifyRecord{Kind: agent.NotifyCheckpoint, At: at(86400), Backfilled: true,
			Watching: []agent.Watched{{Key: k2, RunID: "r1", At: at(2)}}}), idx}, "backfill pass"},
		"a checkpoint listing an event twice": {[]audit.NotifyTrailExport{recsTrail(t, day(0), agent.NotifyRecord{Kind: agent.NotifyCheckpoint, At: at(1),
			Watching: []agent.Watched{{Key: k2, RunID: "r1", At: at(1)}, {Key: k2, RunID: "r1", At: at(1)}}})}, "twice"},
		"a watch with no run":                     {[]audit.NotifyTrailExport{recsTrail(t, day(0), agent.NotifyRecord{Kind: agent.NotifyWatch, Key: k2, At: at(1)})}, "no event key or run"},
		"a run record in a segment":               {[]audit.NotifyTrailExport{recsTrail(t, day(0), op(agent.NotifyObserved, k2, 1))}, "does not belong"},
		"a segment no seal reaches":               {[]audit.NotifyTrailExport{d0, d1, recsTrail(t, day(5), segRec(agent.NotifyCheckpoint, 5, "")), idx}, "chain is broken"},
		"a first segment that carries events":     {[]audit.NotifyTrailExport{d1, idx}, "not here"},
		"an older segment missing from the index": {[]audit.NotifyTrailExport{d0, d1, recsTrail(t, agent.OpenIndex(t0.Year()), segRec(agent.NotifySegment, 86400, day(1)))}, "in no index"},
		"an index out of order":                   {[]audit.NotifyTrailExport{d0, d1, recsTrail(t, agent.OpenIndex(t0.Year()), segRec(agent.NotifySegment, 0, day(1)), segRec(agent.NotifySegment, 1, day(0)))}, "creation order"},
		"a run record in an index":                {[]audit.NotifyTrailExport{recsTrail(t, agent.OpenIndex(t0.Year()), op(agent.NotifyWatch, k2, 1))}, "does not belong in an index"},
		"an index naming a bad segment":           {[]audit.NotifyTrailExport{recsTrail(t, agent.OpenIndex(t0.Year()), segRec(agent.NotifySegment, 1, "notify:@open:soon"))}, "not a segment"},
		"a middle segment gone": {[]audit.NotifyTrailExport{d0, recsTrail(t, day(2), agent.NotifyRecord{Kind: agent.NotifyCheckpoint, At: at(2 * 86400), Backfilled: true}),
			recsTrail(t, agent.OpenIndex(t0.Year()), segRec(agent.NotifySegment, 0, day(0)), segRec(agent.NotifySegment, 1, day(1)), segRec(agent.NotifySegment, 2, day(2)))}, "neither the first nor the last"},
	} {
		rep, err := checkTrails(t, tc.xs...)
		if tc.want == "" { // the newest segment may be missing from the index (a crash before indexing it)
			if err != nil {
				t.Errorf("%s: %v\n%s", name, err, violationsOf(rep))
			}
			continue
		}
		if !errors.Is(err, audit.ErrNotVerified) || !strings.Contains(violationsOf(rep), tc.want) {
			t.Errorf("%s: err %v, violations:\n%s\nwant one with %q", name, err, violationsOf(rep), tc.want)
		}
	}
	// The newest segment may be missing from the index: a writer died before indexing it.
	if rep, err := checkTrails(t, d0, d1, recsTrail(t, agent.OpenIndex(t0.Year()), segRec(agent.NotifySegment, 0, day(0)))); err != nil {
		t.Errorf("the newest segment not yet indexed: %v\n%s", err, violationsOf(rep))
	}
	// A sealed segment whose successor was not yet written is a crash between the two, not a violation.
	if rep, err := checkTrails(t, d0, idx); err != nil {
		t.Errorf("a successor not yet written: %v\n%s", err, violationsOf(rep))
	}
	// The open kinds do not belong on a run's trail.
	for _, kind := range []agent.NotifyRecordKind{agent.NotifyWatch, agent.NotifyUnwatch, agent.NotifyCheckpoint, agent.NotifySeal, agent.NotifySegment, agent.NotifyBackfilled} {
		b := newTrail(t).add(agent.NotifyRecord{Kind: kind, Key: k2, At: at(1), Next: day(1)})
		rep, err := verifyNotify(t, b, nil)
		if !errors.Is(err, audit.ErrNotVerified) || !strings.Contains(strings.Join(rep.Trails[0].Violations, ";"), "not a run's") {
			t.Errorf("%s on a run trail: %v %+v", kind, err, rep.Trails)
		}
	}
}

func TestVerifyNotifications_OperationFieldsMatchTheKey(t *testing.T) {
	gate := agent.ApprovalKey("r1", "c1", 0)
	halt := agent.HaltedKey("r1", agent.OpTool, "c9", 1234)
	withOp := func(key string, kind agent.NotificationKind, run, opKind, id string) agent.NotifyRecord {
		r := observed(key, kind, 0, 1)
		r.OpRun, r.OpKind, r.OpID = run, opKind, id
		if to, _, ok := agent.SplitToKey(key); ok {
			r.Receiver = to
		}
		if lvl, _, ok := agent.ParseEscalationKey(key); ok {
			r.Level = lvl
		}
		return r
	}
	for name, tc := range map[string]struct {
		rec  agent.NotifyRecord
		want string // "" for valid
	}{
		"a fan-out gate":             {withOp(agent.ToKey("risk", gate), agent.NotificationApprovalPending, "r1", "gate", "c1"), ""},
		"a fan-out gate, wrong call": {withOp(agent.ToKey("risk", gate), agent.NotificationApprovalPending, "r1", "gate", "c2"), "does not match"},
		"a gate":                     {withOp(gate, agent.NotificationApprovalPending, "r1", "gate", "c1"), ""},
		"a tool halt":                {withOp(halt, agent.NotificationOutcomeUnknown, "r1", "tool", "c9"), ""},
		"no fields (older record)":   {observed(gate, agent.NotificationApprovalPending, 0, 1), ""},
		"gate with another call":     {withOp(gate, agent.NotificationApprovalPending, "r1", "gate", "c2"), "does not match"},
		"gate with another run":      {withOp(gate, agent.NotificationApprovalPending, "r2", "gate", "c1"), "does not match"},
		"halt with a step kind":      {withOp(halt, agent.NotificationOutcomeUnknown, "r1", "step", "c9"), "does not match"},
		"halt with the gate kind":    {withOp(halt, agent.NotificationOutcomeUnknown, "r1", "gate", "c9"), "not tool or step"},
		"gate with the tool kind":    {withOp(gate, agent.NotificationApprovalPending, "r1", "tool", "c1"), "not gate"},
		"only some fields":           {withOp(gate, agent.NotificationApprovalPending, "r1", "", "c1"), "not all three"},
		"fields on another kind":     {withOp(agent.AbortedKey("r1"), agent.NotificationRunAborted, "r1", "gate", "c1"), "names an operation"},
	} {
		rep, err := verifyNotify(t, newTrail(t).add(tc.rec), nil)
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if !errors.Is(err, audit.ErrNotVerified) || !strings.Contains(strings.Join(rep.Trails[0].Violations, ";"), tc.want) {
			t.Errorf("%s: err %v violations %v, want one with %q", name, err, rep.Trails[0].Violations, tc.want)
		}
	}
}

// An escalation step carries the operation of the event it escalates, judged against that event's key.
func TestVerifyNotifications_EscalationStepOperationFields(t *testing.T) {
	gate := agent.ApprovalKey("r1", "c1", 0)
	halt := agent.HaltedKey("r1", agent.OpTool, "c1", 1234)
	for name, tc := range map[string]struct {
		call, kind, key string
		want            string
	}{
		"the same call":        {"c1", "gate", gate, ""},
		"another call":         {"c2", "gate", gate, "does not match"},
		"a halt, the same":     {"c1", "tool", halt, ""},
		"a halt, another call": {"c2", "tool", halt, "does not match"},
	} {
		b := newTrail(t)
		kind := agent.NotificationApprovalPending
		if tc.key == halt {
			kind = agent.NotificationOutcomeUnknown
		}
		if kind == agent.NotificationOutcomeUnknown { // a halt's chain runs only while it is unacknowledged
			b.add(obsTo(tc.key, kind, 1, ""), claimed(tc.key, 1))
		} else {
			deliver(b, obsTo(tc.key, kind, 1, ""), 2, 3)
		}
		st := step(tc.key, 1, 310, 600, "lead")
		st.OpRun, st.OpKind, st.OpID = "r1", tc.kind, tc.call
		b.add(st)
		rep, err := verifyNotify(t, b, func(in *audit.NotifyVerifyInput) { in.Now = at(320); in.MaxClockSkew = 10 * time.Second })
		viol := strings.Join(rep.Trails[0].Violations, ";")
		if tc.want == "" {
			if viol != "" { // the unacknowledged halt is overdue by design here; only the trail's own rules are judged
				t.Errorf("%s: %v", name, viol)
			}
			continue
		}
		if !errors.Is(err, audit.ErrNotVerified) || !strings.Contains(viol, tc.want) {
			t.Errorf("%s: %v %v", name, err, viol)
		}

// ---- all-clear times from the records that resolve an event ----

// A single approver's decision (agent.Approve) carries its recorded_at, so an all-clear owed for
// the gate it decided is judged by the grace period like an m-of-n gate's, not left untimed.
func TestVerifyNotifications_ASingleApprovalTimesTheAllClear(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	j, _ := agent.NewJournal(mem)
	tool := agent.MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithApproval(agent.SingleApproval()))
	a, _ := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{}`)), j, agent.WithTools(tool))
	if _, err := a.Run(ctx, "r1", agent.UserText("go")); !errors.As(err, new(*agent.ApprovalPending)) {
		t.Fatal(err)
	}
	es := audit.NewMemEventStore()
	if err := agent.NotifyPending(ctx, j, &toggleSink{}, audit.NewNotifyTrail(es), agent.WithNotifyHolder("sweeper")); err != nil {
		t.Fatal(err)
	}
	decided := time.Now().Truncate(time.Millisecond)
	if err := agent.Approve(ctx, j, "r1", "c1", true, agent.WithClock(func() time.Time { return decided })); err != nil {
		t.Fatal(err)
	}
	tx, _ := audit.ExportNotifyTrail(ctx, es, trailName)
	jx, _ := audit.ExportJournal(ctx, j, "r1")
	gate := agent.ApprovalKey("r1", "c1", 0)
	run := func(now time.Time) (audit.NotifyReport, error) {
		return audit.VerifyNotifications(audit.NotifyVerifyInput{Trails: []audit.NotifyTrailExport{tx}, Journals: []audit.JournalExport{jx}, Unanchored: true, Now: now})
	}
	rep, err := run(decided.Add(time.Minute))
	if err != nil || notifyStatusOf(t, rep, agent.ResolvedKey(gate)) != audit.NotifyStatusNotYetDue {
		t.Fatalf("within the grace period: %v %+v", err, rep.Events)
	}
	rep, err = run(decided.Add(10 * time.Minute))
	if !errors.Is(err, audit.ErrNotVerified) || notifyStatusOf(t, rep, agent.ResolvedKey(gate)) != audit.NotifyStatusNeverNotified {
		t.Fatalf("after it: %v %+v", err, rep.Events)
	}
}

// A halt resolved with agent.ResolveHalt is a journal event that is no longer current, timed by the
// injected result's recorded_at: its all-clear is judged late after the grace period.
func TestVerifyNotifications_AResolvedHaltTimesTheAllClear(t *testing.T) {
	ctx := context.Background()
	mem := agent.NewMemStore()
	charge := agent.MustFunc("charge", "", func(context.Context, struct{}) (string, error) { return "ok", nil })
	drive := func(s agent.Store) error {
		j, _ := agent.NewJournal(s)
		a, _ := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "charge", `{}`), agenttest.TextTurn("done")), j, agent.WithTools(charge))
		_, err := a.Run(ctx, "r1", agent.UserText("go"))
		return err
	}
	_ = drive(crashBeforeResultStore{mem})
	var halt *agent.OutcomeUnknown
	if err := drive(mem); !errors.As(err, &halt) {
		t.Fatalf("second drive = %v", err)
	}
	j, _ := agent.NewJournal(mem)
	clock := &stepClock{now: time.Now().Add(2 * time.Minute)}
	es := audit.NewMemEventStore()
	if err := agent.NotifyPending(ctx, j, &toggleSink{}, audit.NewNotifyTrail(es), sweepOpts(clock)...); err != nil {
		t.Fatal(err)
	}
	resolved := clock.now.Add(time.Minute).Truncate(time.Millisecond)
	if err := agent.ResolveHalt(ctx, j, halt.Ref(), agent.Outcome{Result: "ok", Evidence: "provider"}, agent.WithClock(func() time.Time { return resolved }), agent.WithoutLiveDriverCheck()); err != nil {
		t.Fatal(err)
	}
	tx, _ := audit.ExportNotifyTrail(ctx, es, trailName)
	jx, _ := audit.ExportJournal(ctx, j, "r1")
	events := agent.JournalNotifyEvents("r1", mustHistory(t, j, "r1"))
	var key string
	for _, e := range events {
		if e.Kind == agent.NotificationOutcomeUnknown && !e.Current && e.ResolvedAt.Equal(resolved) {
			key = e.Key
		}
	}
	if key == "" {
		t.Fatalf("no resolved halt timed %v in %+v", resolved, events)
	}
	run := func(now time.Time) (audit.NotifyReport, error) {
		return audit.VerifyNotifications(audit.NotifyVerifyInput{Trails: []audit.NotifyTrailExport{tx}, Journals: []audit.JournalExport{jx}, Unanchored: true, Now: now})
	}
	rep, err := run(resolved.Add(time.Minute))
	if err != nil || notifyStatusOf(t, rep, agent.ResolvedKey(key)) != audit.NotifyStatusNotYetDue {
		t.Fatalf("within the grace period: %v %+v", err, rep.Events)
	}
	rep, err = run(resolved.Add(10 * time.Minute))
	if !errors.Is(err, audit.ErrNotVerified) || notifyStatusOf(t, rep, agent.ResolvedKey(key)) != audit.NotifyStatusNeverNotified {
		t.Fatalf("after it: %v %+v", err, rep.Events)
	}
}
