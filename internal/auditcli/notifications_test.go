package auditcli

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
)

var nt0 = time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)

func nat(sec int) time.Time { return nt0.Add(time.Duration(sec) * time.Second) }

const nTrail = "notify:r1"

// notifyFixture is a trail in a MemEventStore, written through the real adapter.
type notifyFixture struct {
	t    *testing.T
	dir  string
	es   *audit.MemEventStore
	tr   *audit.NotifyTrail
	n    int
	priv ed25519.PrivateKey
	pub  string
}

func newNotifyFixture(t *testing.T) *notifyFixture {
	_, priv, _ := ed25519.GenerateKey(nil)
	return newNotifyFixtureAt(t, priv)
}

func newNotifyFixtureAt(t *testing.T, priv ed25519.PrivateKey) *notifyFixture {
	es := audit.NewMemEventStore()
	return &notifyFixture{t: t, dir: t.TempDir(), es: es, tr: audit.NewNotifyTrail(es), priv: priv,
		pub: keyText(audit.Ed25519Signer{Priv: priv})}
}

func (f *notifyFixture) add(recs ...agent.NotifyRecord) *notifyFixture {
	f.t.Helper()
	for _, r := range recs {
		r.RunID = "r1"
		if err := f.tr.Append(context.Background(), nTrail, f.n, r); err != nil {
			f.t.Fatal(err)
		}
		f.n++
	}
	return f
}

// event adds an event observed 1s after it began, claimed, and acknowledged ackSec after the start.
func (f *notifyFixture) event(key string, kind agent.NotificationKind, ackSec int) *notifyFixture {
	return f.add(
		agent.NotifyRecord{Kind: agent.NotifyObserved, Key: key, At: nat(1), EventKind: string(kind), EventAt: nat(0), DeadlineAt: nat(1).Add(5 * time.Minute)},
		agent.NotifyRecord{Kind: agent.NotifyClaimed, Key: key, At: nat(1), Holder: "p1", Until: nat(121)},
		agent.NotifyRecord{Kind: agent.NotifyAcked, Key: key, At: nat(ackSec), FirstSentAt: nat(ackSec), Attempts: 1, Status: 200})
}

func (f *notifyFixture) files() (trail, sth string) {
	f.t.Helper()
	x, err := audit.ExportNotifyTrail(context.Background(), f.es, nTrail)
	if err != nil {
		f.t.Fatal(err)
	}
	h, err := audit.SignNotifyTrail(context.Background(), f.es, nTrail, audit.Ed25519Signer{Priv: f.priv}, nat(30))
	if err != nil {
		f.t.Fatal(err)
	}
	trail, sth = filepath.Join(f.dir, "trail.json"), filepath.Join(f.dir, "sth.json")
	writeJSON(f.t, trail, x)
	writeJSON(f.t, sth, h)
	return trail, sth
}

func (f *notifyFixture) verify(extra ...string) (int, string, string) {
	trail, sth := f.files()
	return runCLI(append([]string{"notifications", "-trail", trail, "-sth", sth, "-pubkey", f.pub, "-as-of", nat(60).Format(time.RFC3339)}, extra...)...)
}

func TestNotificationsVerb_CompleteTrailVerifies(t *testing.T) {
	f := newNotifyFixture(t).event(agent.ApprovalKey("r1", "c1", 0), agent.NotificationApprovalPending, 3)
	code, out, errOut := f.verify()
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{"PASS  acked", "approval_pending", "signed head over the first 3", "PASS: 1 event(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestNotificationsVerb_JSONReportCarriesTheVerdicts(t *testing.T) {
	key := agent.ApprovalKey("r1", "c1", 0)
	f := newNotifyFixture(t).event(key, agent.NotificationApprovalPending, 3)
	code, out, _ := f.verify("-json")
	var rep struct {
		ExitCode int
		Verified bool
		Artifact audit.NotifyReport
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil || code != 0 || !rep.Verified {
		t.Fatalf("exit %d err %v\n%s", code, err, out)
	}
	if len(rep.Artifact.Events) != 1 || rep.Artifact.Events[0].Key != key || rep.Artifact.Events[0].Status != audit.NotifyStatusAcked || !rep.Artifact.Trails[0].Anchored {
		t.Fatalf("artifact = %+v", rep.Artifact)
	}
}

func TestNotificationsVerb_LateAckAndTheBoundFail(t *testing.T) {
	key := agent.ApprovalKey("r1", "c1", 0)
	f := newNotifyFixture(t).event(key, agent.NotificationApprovalPending, 400) // past the 5 minute deadline
	code, out, _ := f.verify()
	if code != 1 || !strings.Contains(out, "FAIL  acked_late") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	g := newNotifyFixture(t).event(key, agent.NotificationApprovalPending, 30)
	if code, out, _ := g.verify(); code != 0 {
		t.Fatalf("within the deadline: exit %d\n%s", code, out)
	}
	if code, out, _ := g.verify("-max-ack", "10s"); code != 1 || !strings.Contains(out, "acked_late") {
		t.Fatalf("with -max-ack 10s: exit %d\n%s", code, out)
	}
	if code, _, _ := g.verify("-max-ack", "-1s"); code != 2 {
		t.Fatalf("a negative bound: exit %d, want 2", code)
	}
}

func TestNotificationsVerb_TamperedTrailsFail(t *testing.T) {
	f := newNotifyFixture(t).
		event(agent.ApprovalKey("r1", "c1", 0), agent.NotificationApprovalPending, 3).
		event(agent.ApprovalKey("r1", "c2", 0), agent.NotificationApprovalPending, 4)
	trail, sth := f.files()
	var x audit.NotifyTrailExport
	b, _ := os.ReadFile(trail)
	if err := json.Unmarshal(b, &x); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(audit.NotifyTrailExport) audit.NotifyTrailExport{
		"a deleted ack": func(x audit.NotifyTrailExport) audit.NotifyTrailExport {
			x.Leaves = slices.Delete(slices.Clone(x.Leaves), 2, 3)
			return x
		},
		"reordered records": func(x audit.NotifyTrailExport) audit.NotifyTrailExport {
			x.Leaves = slices.Clone(x.Leaves)
			x.Leaves[1], x.Leaves[4] = x.Leaves[4], x.Leaves[1]
			return x
		},
		"a truncated trail": func(x audit.NotifyTrailExport) audit.NotifyTrailExport { x.Leaves = x.Leaves[:3]; return x },
	} {
		p := filepath.Join(t.TempDir(), "t.json")
		writeJSON(t, p, mod(x))
		code, out, _ := runCLI("notifications", "-trail", p, "-sth", sth, "-pubkey", f.pub, "-as-of", nat(60).Format(time.RFC3339))
		if code != 1 || !strings.Contains(out, "FAIL") {
			t.Errorf("%s: exit %d\n%s", name, code, out)
		}
	}
	// The same trail without its head is not attested.
	if code, out, _ := runCLI("notifications", "-trail", trail, "-pubkey", f.pub, "-as-of", nat(60).Format(time.RFC3339)); code != 1 || !strings.Contains(out, "no signed head") {
		t.Errorf("no head: exit %d\n%s", code, out)
	}
	// With -unanchored it is read, and says it is not attested.
	code, out, _ := runCLI("notifications", "-trail", trail, "-unanchored", "-as-of", nat(60).Format(time.RFC3339))
	if code != 0 || !strings.Contains(out, "UNANCHORED") {
		t.Errorf("-unanchored: exit %d\n%s", code, out)
	}
}

func TestNotificationsVerb_UnusableAndUsage(t *testing.T) {
	f := newNotifyFixture(t).event(agent.ApprovalKey("r1", "c1", 0), agent.NotificationApprovalPending, 3)
	trail, sth := f.files()
	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, []byte(`{"format":"bide.audit.notify-trail-export.v1","trail":"notify:r1","leaves":["aGVsbG8="]}`), 0o644)
	old := filepath.Join(t.TempDir(), "old.json")
	os.WriteFile(old, []byte(`{"format":"bide.audit.notify-trail-export.v0","trail":"notify:r1","leaves":[]}`), 0o644)
	for name, tc := range map[string]struct {
		args []string
		want int
	}{
		"a leaf that is not a record": {[]string{"-trail", bad, "-unanchored"}, 4},
		"another format":              {[]string{"-trail", old, "-unanchored"}, 4},
		"a missing file":              {[]string{"-trail", filepath.Join(t.TempDir(), "none.json"), "-unanchored"}, 4},
		"nothing to read":             {[]string{"-unanchored"}, 2},
		"no key and not unanchored":   {[]string{"-trail", trail}, 2},
		"a head with no key":          {[]string{"-trail", trail, "-sth", sth, "-unanchored"}, 2},
		"-run with files":             {[]string{"-trail", trail, "-unanchored", "-run", "r1"}, 2},
		"-db in a build with none":    {[]string{"-db", "x.db", "-unanchored"}, 2},
		"a bad -as-of":                {[]string{"-trail", trail, "-unanchored", "-as-of", "yesterday"}, 2},
		"a stray argument":            {[]string{"-trail", trail, "-unanchored", "extra"}, 2},
	} {
		if code, out, errOut := runCLI(append([]string{"notifications"}, tc.args...)...); code != tc.want {
			t.Errorf("%s: exit %d, want %d\n%s\n%s", name, code, tc.want, out, errOut)
		}
	}
}

// ---- the journal cross-check ----

// gateJournal runs an agent to an approval gate with no notifier and returns its journal export.
func gateJournal(t *testing.T, run string) audit.JournalExport {
	t.Helper()
	tool := agent.MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithApproval(agent.SingleApproval()))
	j, _ := agent.NewJournal(agent.NewMemStore())
	a, err := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{}`), agenttest.TextTurn("done")), j, agent.WithTools(tool))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), run, agent.UserText("go")); !errors.As(err, new(*agent.ApprovalPending)) {
		t.Fatalf("Run = %v", err)
	}
	return exportJournal(t, j, run)
}

func TestNotificationsVerb_AnUnnotifiedGateFailsTheAudit(t *testing.T) {
	f := newNotifyFixture(t).event(agent.ApprovalKey("r1", "other", 0), agent.NotificationApprovalPending, 3)
	jp := filepath.Join(f.dir, "journal.json")
	writeJSON(t, jp, gateJournal(t, "r1"))
	late := time.Now().Add(time.Hour).Format(time.RFC3339)
	trail, sth := f.files()
	code, out, _ := runCLI("notifications", "-trail", trail, "-sth", sth, "-pubkey", f.pub, "-journal", jp, "-as-of", late)
	if code != 1 || !strings.Contains(out, "FAIL  never_notified") || !strings.Contains(out, ApprovalKeyForTest("r1", "c1")) {
		t.Fatalf("exit %d\n%s", code, out)
	}
	// Without the journal the same trail is clean: the cross-check is what finds it.
	if code, out, _ := runCLI("notifications", "-trail", trail, "-sth", sth, "-pubkey", f.pub, "-as-of", late); code != 0 {
		t.Fatalf("trail alone: exit %d\n%s", code, out)
	}
}

func ApprovalKeyForTest(run, call string) string { return agent.ApprovalKey(run, call, 0) }

// ---- reading from a store ----

type memNotifyStore struct {
	journal *agent.MemStore
	events  *audit.MemEventStore
	closed  *bool
}

func fakeStores(t *testing.T, s memNotifyStore) {
	t.Helper()
	open := func(string) (NotifyStores, error) {
		return NotifyStores{Journal: s.journal, Events: s.events, Close: func() error { *s.closed = true; return nil }}, nil
	}
	Stores = &StoreOpeners{SQLite: open, Postgres: open}
	t.Cleanup(func() { Stores = nil })
}

func TestNotificationsVerb_ReadsEveryRunFromAStore(t *testing.T) {
	ctx := context.Background()
	mem, es := agent.NewMemStore(), audit.NewMemEventStore()
	j, _ := agent.NewJournal(mem)
	tool := agent.MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithApproval(agent.SingleApproval()))
	for _, run := range []string{"a", "b"} {
		a, _ := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{}`)), j, agent.WithTools(tool),
			agent.WithNotifier(&okSink{}, audit.NewNotifyTrail(es), agent.WithNotifyHolder("p1")))
		if _, err := a.Run(ctx, run, agent.UserText("go")); !errors.As(err, new(*agent.ApprovalPending)) {
			t.Fatal(err)
		}
		if err := a.FlushNotifications(ctx); err != nil {
			t.Fatal(err)
		}
	}
	closed := false
	fakeStores(t, memNotifyStore{mem, es, &closed})

	code, out, errOut := runCLI("notifications", "-db", "x.db", "-unanchored")
	if code != 0 || !closed || !strings.Contains(out, "trail notify:a") || !strings.Contains(out, "trail notify:b") || !strings.Contains(out, "PASS: 2 event(s) in 4 trail(s)") {
		t.Fatalf("exit %d closed=%v\n%s\n%s", code, closed, out, errOut)
	}
	// -run reads one run only.
	code, out, _ = runCLI("notifications", "-db", "x.db", "-unanchored", "-run", "a")
	if code != 0 || strings.Contains(out, "notify:b") || !strings.Contains(out, "notify:a") {
		t.Fatalf("-run a: exit %d\n%s", code, out)
	}
	if code, _, errOut := runCLI("notifications", "-db", "x.db", "-unanchored", "-run", "nope"); code != 4 || !strings.Contains(errOut, "no such run") {
		t.Fatalf("-run nope: exit %d\n%s", code, errOut)
	}
	// A run that needed no one has no trail, and is not listed as one.
	done, _ := agent.New(agenttest.NewScriptedModel(agenttest.TextTurn("done")), j)
	if _, err := done.Run(ctx, "d", agent.UserText("go")); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := runCLI("notifications", "-db", "x.db", "-unanchored"); code != 0 || strings.Contains(out, "notify:d") {
		t.Fatalf("a run with no events: exit %d\n%s", code, out)
	}
	// A session's turn is the session's to drive: the notifier does not report it, and neither does the audit.
	driveToGate := func(run string) { // an engine cannot drive these IDs: copy the entries of a run at a gate
		t.Helper()
		tm := agent.NewMemStore() // a store of its own, so the template run is not one the audit reads
		tj, _ := agent.NewJournal(tm)
		g, _ := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{}`)), tj, agent.WithTools(tool))
		if _, err := g.Run(ctx, "template", agent.UserText("go")); !errors.As(err, new(*agent.ApprovalPending)) {
			t.Fatalf("%s: %v", run, err)
		}
		for e, err := range tm.Load(ctx, "template", -1) {
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := mem.Insert(ctx, run, e.Name, e.Data); err != nil {
				t.Fatal(err)
			}
		}
	}
	driveToGate("s>@turn/1")
	late := time.Now().Add(time.Hour).Format(time.RFC3339)
	if code, out, _ := runCLI("notifications", "-db", "x.db", "-unanchored", "-as-of", late); code != 0 || strings.Contains(out, "s>@turn") {
		t.Fatalf("a session turn: exit %d\n%s", code, out)
	}
	// -run a covers a's sub-runs: a gate in one of them that no one was told of is found.
	sub := agent.SubRunID("a", "x1")
	driveToGate(sub)
	code, out, _ = runCLI("notifications", "-db", "x.db", "-unanchored", "-as-of", late, "-run", "a")
	if code != 1 || !strings.Contains(out, "never_notified") || !strings.Contains(out, "approval:a") {
		t.Fatalf("-run a with an un-notified sub-run gate: exit %d\n%s", code, out)
	}
	if code, out, _ := runCLI("notifications", "-db", "x.db", "-unanchored", "-as-of", late, "-run", "b"); code != 0 {
		t.Fatalf("-run b must not read a's sub-run: exit %d\n%s", code, out)
	}
	// A run whose gate was never notified is found in the store, with no trail for it.
	tool2 := agent.MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithApproval(agent.SingleApproval()))
	a, _ := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{}`)), j, agent.WithTools(tool2))
	if _, err := a.Run(ctx, "c", agent.UserText("go")); !errors.As(err, new(*agent.ApprovalPending)) {
		t.Fatal(err)
	}
	code, out, _ = runCLI("notifications", "-db", "x.db", "-unanchored", "-as-of", time.Now().Add(time.Hour).Format(time.RFC3339))
	if code != 1 || !strings.Contains(out, "never_notified") || !strings.Contains(out, "approval:c:") {
		t.Fatalf("an unnotified run in the store: exit %d\n%s", code, out)
	}
}

type okSink struct{}

func (*okSink) Notify(context.Context, agent.Notification) error { return nil }

func TestNotificationsVerb_PostgresDSNComesFromTheEnvironment(t *testing.T) {
	closed := false
	fakeStores(t, memNotifyStore{agent.NewMemStore(), audit.NewMemEventStore(), &closed})
	t.Setenv("BIDE_TEST_DSN", "")
	if code, _, errOut := runCLI("notifications", "-postgres-env", "BIDE_TEST_DSN", "-unanchored"); code != 4 || !strings.Contains(errOut, "BIDE_TEST_DSN") {
		t.Fatalf("an empty variable: exit %d\n%s", code, errOut)
	}
	t.Setenv("BIDE_TEST_DSN", "postgres://u:secret@h/db")
	code, out, errOut := runCLI("notifications", "-postgres-env", "BIDE_TEST_DSN", "-unanchored")
	if code != 0 || strings.Contains(out+errOut, "secret") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if code, _, _ := runCLI("notifications", "-postgres-env", "BIDE_TEST_DSN", "-db", "x", "-unanchored"); code != 2 {
		t.Fatalf("both -db and -postgres-env: exit %d, want 2", code)
	}
}

// A decided gate no notifier observed does not fail the audit, and is shown as a warning.
func TestNotificationsVerb_UnobservedResolvedIsAWarning(t *testing.T) {
	ctx := context.Background()
	tool := agent.MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithApproval(agent.SingleApproval()))
	mem := agent.NewMemStore()
	j, _ := agent.NewJournal(mem)
	a, _ := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{}`)), j, agent.WithTools(tool))
	if _, err := a.Run(ctx, "w", agent.UserText("go")); !errors.As(err, new(*agent.ApprovalPending)) {
		t.Fatal(err)
	}
	if _, _, err := mem.Insert(ctx, "w", agent.ToolResultStep("c1"), []byte(`{"name":"`+agent.ToolResultStep("c1")+`","kind":"tool_result","tool_use_id":"c1"}`)); err != nil {
		t.Fatal(err)
	}
	fakeStores(t, memNotifyStore{mem, audit.NewMemEventStore(), new(bool)})
	code, out, _ := runCLI("notifications", "-db", "x.db", "-unanchored", "-as-of", time.Now().Add(time.Hour).Format(time.RFC3339))
	if code != 0 || !strings.Contains(out, "note: WARNING unobserved_resolved") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	// -strict lets a bank fail on it.
	code, out, _ = runCLI("notifications", "-db", "x.db", "-unanchored", "-strict", "-as-of", time.Now().Add(time.Hour).Format(time.RFC3339))
	if code != 1 || !strings.Contains(out, "FAIL  unobserved_resolved") {
		t.Fatalf("-strict: exit %d\n%s", code, out)
	}
}

func TestNotificationsVerb_ShowsTheReceiverOfEachEventAndASummaryPerReceiver(t *testing.T) {
	gate := agent.ApprovalKey("r1", "c1", 0)
	step := agent.EscalationKey(gate, 1)
	f := newNotifyFixture(t)
	f.add(
		agent.NotifyRecord{Kind: agent.NotifyObserved, Key: gate, At: nat(1), EventKind: "approval_pending", EventAt: nat(0), DeadlineAt: nat(1).Add(30 * time.Second), Receiver: "approvers"},
		agent.NotifyRecord{Kind: agent.NotifyClaimed, Key: gate, At: nat(1), Holder: "p1", Until: nat(121)},
		agent.NotifyRecord{Kind: agent.NotifyAcked, Key: gate, At: nat(3), FirstSentAt: nat(3), Attempts: 1, Status: 200},
		agent.NotifyRecord{Kind: agent.NotifyObserved, Key: step, At: nat(40), EventKind: "escalated", EventAt: nat(40), DeadlineAt: nat(40).Add(time.Minute), Receiver: "lead", Level: 1},
		agent.NotifyRecord{Kind: agent.NotifyClaimed, Key: step, At: nat(40), Holder: "p1", Until: nat(160)},
		agent.NotifyRecord{Kind: agent.NotifyAcked, Key: step, At: nat(42), FirstSentAt: nat(42), Attempts: 1, Status: 200})
	code, out, errOut := f.verify()
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{"receiver approvers", "receiver lead", "level 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "INFO  approvers: 1 acked") || !strings.Contains(out, "INFO  lead: 1 acked") {
		t.Errorf("no summary per receiver:\n%s", out)
	}
}

// A notifier that anchors its trail leaves one head per size in its anchor log; the verb takes any
// number of them for a trail, checks each, and reports the records the newest does not attest.
func TestNotificationsVerb_ManyHeadsAndTheUnattestedGap(t *testing.T) {
	f := newNotifyFixture(t).event(agent.ApprovalKey("r1", "c1", 0), agent.NotificationApprovalPending, 3)
	sign := func(name string) string {
		h, err := audit.SignNotifyTrail(context.Background(), f.es, nTrail, audit.Ed25519Signer{Priv: f.priv}, nat(30))
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(f.dir, name)
		writeJSON(t, p, h)
		return p
	}
	early := sign("early.json")
	f.event(agent.ApprovalKey("r1", "c2", 0), agent.NotificationApprovalPending, 4)
	late := sign("late.json")
	trail, _ := f.files()
	run := func(extra ...string) (int, string) {
		code, out, _ := runCLI(append([]string{"notifications", "-trail", trail, "-pubkey", f.pub, "-as-of", nat(60).Format(time.RFC3339)}, extra...)...)
		return code, out
	}

	if code, out := run("-sth", early, "-sth", late); code != 0 || !strings.Contains(out, "the newest of 2 that all verify") || strings.Contains(out, "not attested") {
		t.Fatalf("both heads: exit %d\n%s", code, out)
	}
	code, out := run("-sth", early)
	if code != 0 || !strings.Contains(out, "signed head over the first 3") || !strings.Contains(out, "the last 3 are not attested by it (the oldest, from 2026-10-13T09:00:01Z, for 59s)") {
		t.Fatalf("the early head only: exit %d\n%s", code, out)
	}
	if code, out := run("-sth", early, "-max-unattested", "30s"); code != 1 || !strings.Contains(out, "gone unattested for 59s") {
		t.Fatalf("-max-unattested 30s: exit %d\n%s", code, out)
	}
	if code, out := run("-sth", early, "-max-unattested", "2m"); code != 0 {
		t.Fatalf("-max-unattested 2m: exit %d\n%s", code, out)
	}
	if code, _ := run("-sth", late, "-max-unattested", "-1s"); code != 2 {
		t.Fatalf("a negative -max-unattested: exit %d, want 2", code)
	}
	// One head that does not match the trail fails the audit although the other does.
	other := newNotifyFixtureAt(t, f.priv).event(agent.ApprovalKey("r1", "c9", 0), agent.NotificationApprovalPending, 3)
	_, wrong := other.files()
	if code, out := run("-sth", late, "-sth", wrong); code != 1 || !strings.Contains(out, "FAIL  trail notify:r1: signed head of size 3") {
		t.Fatalf("a head of another history: exit %d\n%s", code, out)
	}
}

// A run whose events the open trail lists but whose journal is not in the store (the journal was
// deleted, or the trail came from another database) is reported, not skipped: its trail is
// verified all the same, and -strict fails on it.
func TestNotificationsVerb_ARunListedOnTheOpenTrailWithNoJournalIsReported(t *testing.T) {
	ctx := context.Background()
	mem, es := agent.NewMemStore(), audit.NewMemEventStore()
	other, _ := agent.NewJournal(agent.NewMemStore()) // the journal of "z" is somewhere else
	tool := agent.MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithApproval(agent.SingleApproval()))
	drive := func(j *agent.Journal, run string) {
		a, _ := agent.New(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{}`)), j, agent.WithTools(tool),
			agent.WithNotifier(&okSink{}, audit.NewNotifyTrail(es), agent.WithNotifyHolder("p1")))
		if _, err := a.Run(ctx, run, agent.UserText("go")); !errors.As(err, new(*agent.ApprovalPending)) {
			t.Fatal(err)
		}
		if err := a.FlushNotifications(ctx); err != nil {
			t.Fatal(err)
		}
	}
	j, _ := agent.NewJournal(mem)
	drive(j, "a")
	drive(other, "z")
	closed := false
	fakeStores(t, memNotifyStore{mem, es, &closed})

	code, out, errOut := runCLI("notifications", "-db", "x.db", "-unanchored")
	if code != 0 || !strings.Contains(out, "trail notify:z") || !strings.Contains(out, "WARNING") || !strings.Contains(out, "z") || !strings.Contains(out, "no journal") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if code, out, _ := runCLI("notifications", "-db", "x.db", "-unanchored", "-strict"); code != 1 || !strings.Contains(out, "no journal") {
		t.Fatalf("-strict: exit %d\n%s", code, out)
	}
	// -run a reads only a: z is not this audit's.
	if code, out, _ := runCLI("notifications", "-db", "x.db", "-unanchored", "-run", "a", "-strict"); code != 0 || strings.Contains(out, "notify:z") {
		t.Fatalf("-run a: exit %d\n%s", code, out)
	}
}

// The segments are found by following the index and the seals, wherever they lead: a seal into a
// segment years ahead of the clock (a peer whose clock was wrong) is read with its year's index, not
// skipped because it is past a fixed bound.
func TestNotificationsVerb_TheOpenTrailIsFollowedPastAnyFixedYearBound(t *testing.T) {
	ctx := context.Background()
	mem, es := agent.NewMemStore(), audit.NewMemEventStore()
	tr := audit.NewNotifyTrail(es)
	now := time.Now().UTC()
	far := now.AddDate(5, 0, 0)
	segNow, segFar := agent.OpenSegment(now), agent.OpenSegment(far)
	put := func(name string, recs ...agent.NotifyRecord) {
		t.Helper()
		for i, r := range recs {
			if err := tr.Append(ctx, name, i, r); err != nil {
				t.Fatal(err)
			}
		}
	}
	put(agent.OpenIndex(now.Year()), agent.NotifyRecord{Kind: agent.NotifySegment, At: now, Next: segNow})
	put(segNow, agent.NotifyRecord{Kind: agent.NotifyCheckpoint, At: now}, agent.NotifyRecord{Kind: agent.NotifySeal, At: far, Next: segFar})
	put(segFar, agent.NotifyRecord{Kind: agent.NotifyCheckpoint, At: far})
	put(agent.OpenIndex(far.Year()), agent.NotifyRecord{Kind: agent.NotifySegment, At: far, Next: segFar})
	closed := false
	fakeStores(t, memNotifyStore{mem, es, &closed})
	code, out, errOut := runCLI("notifications", "-db", "x.db", "-unanchored")
	for _, want := range []string{"trail " + segFar, "trail " + agent.OpenIndex(far.Year())} {
		if !strings.Contains(out, want) {
			t.Fatalf("exit %d: %q is not read\n%s\n%s", code, want, out, errOut)
		}
	}
}
