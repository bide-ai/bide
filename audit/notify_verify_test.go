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

func statusOf(t *testing.T, rep audit.NotifyReport, key string) audit.NotifyStatus {
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
	if statusOf(t, rep, key) != audit.NotifyStatusAckedLate || len(rep.Problems) != 1 || !strings.Contains(rep.Problems[0], "deadline") {
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
	if !errors.Is(err, audit.ErrNotVerified) || statusOf(t, rep, key) != audit.NotifyStatusAckedLate {
		t.Fatalf("with a 1m bound: status %s err %v", statusOf(t, rep, key), err)
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
	if statusOf(t, rep, pend) != audit.NotifyStatusPending || statusOf(t, rep, fail) != audit.NotifyStatusFailing {
		t.Fatalf("report = %+v", rep.Events)
	}
	// Past the deadline they are overdue, and that fails.
	rep, err = verifyNotify(t, b, func(in *audit.NotifyVerifyInput) { in.Now = at(1).Add(6 * time.Minute) })
	if !errors.Is(err, audit.ErrNotVerified) || statusOf(t, rep, pend) != audit.NotifyStatusOverdue || statusOf(t, rep, over) != audit.NotifyStatusOverdue {
		t.Fatalf("err %v report %+v", err, rep.Events)
	}
}

func TestVerifyNotifications_ClearedEvents(t *testing.T) {
	before, after := agent.ApprovalKey("r1", "b", 0), agent.ApprovalKey("r1", "a", 0)
	b := newTrail(t).add(
		observed(before, agent.NotificationApprovalPending, 0, 1), cleared(before, 2, "decided"),
		observed(after, agent.NotificationApprovalPending, 0, 1), claimed(after, 1), cleared(after, 5, "decided"))
	rep, err := verifyNotify(t, b, nil)
	if err != nil || statusOf(t, rep, agent.ResolvedKey(after)) != audit.NotifyStatusNotYetDue {
		t.Fatalf("an all-clear within the grace period is not yet late: %v %+v", err, rep.Events)
	}
	late := func(in *audit.NotifyVerifyInput) { in.Now = at(60).Add(time.Hour) }
	rep, err = verifyNotify(t, b, late)
	if !errors.Is(err, audit.ErrNotVerified) {
		t.Fatalf("a cleared event whose delivery was attempted owes an all-clear: %v", err)
	}
	if statusOf(t, rep, before) != audit.NotifyStatusClearedBeforeNotification || statusOf(t, rep, after) != audit.NotifyStatusClearedUnacknowledged {
		t.Fatalf("report = %+v", rep.Events)
	}
	if statusOf(t, rep, agent.ResolvedKey(after)) != audit.NotifyStatusNeverNotified {
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
	if !errors.Is(err, audit.ErrNotVerified) || statusOf(t, rep, agent.OverdueKey(key)) != audit.NotifyStatusNeverNotified {
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
	if !errors.Is(err, audit.ErrNotVerified) || statusOf(t, rep, key) != audit.NotifyStatusNeverNotified {
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
	if !errors.Is(err, audit.ErrNotVerified) || statusOf(t, rep, gate) != audit.NotifyStatusNeverNotified {
		t.Fatalf("err %v report %+v", err, rep.Events)
	}
	for _, e := range rep.Events {
		if e.Key == gate && (!e.InJournal || e.Kind != "approval_pending") {
			t.Fatalf("verdict = %+v", e)
		}
	}
	// A gate that has only just begun is not yet late.
	rep, err = verifyNotify(t, b, func(in *audit.NotifyVerifyInput) { in.Journals = []audit.JournalExport{jx}; in.Now = time.Now() })
	if err != nil || statusOf(t, rep, gate) != audit.NotifyStatusNotYetDue {
		t.Fatalf("a gate within the grace period: %v %+v", err, rep.Events)
	}
	// With no trail at all the run is still found.
	rep, err = audit.VerifyNotifications(audit.NotifyVerifyInput{Journals: []audit.JournalExport{jx}, Unanchored: true, Now: time.Now().Add(time.Hour)})
	if !errors.Is(err, audit.ErrNotVerified) || statusOf(t, rep, gate) != audit.NotifyStatusNeverNotified {
		t.Fatalf("no trail: err %v report %+v", err, rep.Events)
	}
	// Once the trail observes and acknowledges the gate it passes.
	b.complete(gate, agent.NotificationApprovalPending, 0)
	rep, err = verifyNotify(t, b, later)
	if err != nil || statusOf(t, rep, gate) != audit.NotifyStatusAcked {
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
	rep, err := audit.VerifyNotifications(audit.NotifyVerifyInput{Trails: []audit.NotifyTrailExport{tx}, Journals: []audit.JournalExport{jx}, Unanchored: true})
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

	// A trail grown after the head still verifies, and says how much the head does not attest.
	b.complete(agent.ApprovalKey("r1", "c3", 0), agent.NotificationApprovalPending, 10)
	rep, err = anchored(b, b.export(), head, pub)
	if err != nil || !rep.Trails[0].Anchored || rep.Trails[0].Tail != 3 {
		t.Fatalf("grown trail: %v %+v", err, rep.Trails)
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
	journalKind := head
	journalKind.Kind = audit.TreeJournal
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
	_, err := audit.VerifyNotifications(audit.NotifyVerifyInput{Heads: []audit.SignedTreeHead{head}, Verifier: audit.Ed25519Verifier{Pub: pub}, Now: at(60)})
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
	if statusOf(t, rep, gate) != audit.NotifyStatusAckedLate || statusOf(t, rep, agent.OverdueKey(gate)) != audit.NotifyStatusAckedLate {
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
	if statusOf(t, rep, gate) != audit.NotifyStatusAcked || statusOf(t, rep, agent.ResolvedKey(gate)) != audit.NotifyStatusAcked {
		t.Fatalf("events %+v", rep.Events)
	}
}
