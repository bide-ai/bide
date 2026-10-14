package runview

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/sample"
)

// attnNow is the clock the attention tests read: a day and a bit after the sample's first run.
var attnNow = sample.Base.Add(26 * time.Hour)

func attentionOf(t *testing.T, h http.Handler) AttentionList {
	t.Helper()
	code, body := get(t, h, "/attention.json")
	if code != 200 {
		t.Fatalf("GET /attention.json = %d: %s", code, body)
	}
	var l AttentionList
	if err := json.Unmarshal([]byte(body), &l); err != nil {
		t.Fatal(err)
	}
	return l
}

func sampleOpts(t *testing.T) (Options, []sample.Run) {
	t.Helper()
	s := agent.NewMemStore()
	res, err := sample.Write(context.Background(), s, "")
	if err != nil {
		t.Fatal(err)
	}
	return Options{Source: JournalSource{J: agenttest.MustJournal(s)}, Now: func() time.Time { return attnNow }}, res.Runs
}

func itemsOf(l AttentionList, kind string) []AttentionItem {
	var out []AttentionItem
	for _, it := range l.Items {
		if it.Kind == kind {
			out = append(out, it)
		}
	}
	return out
}

// TestAttentionListsWhatNeedsAPerson: every halted and waiting run of the sample is on the list, a
// run that needs nobody is not, and the items are newest first.
func TestAttentionListsWhatNeedsAPerson(t *testing.T) {
	o, runs := sampleOpts(t)
	l := attentionOf(t, Handler(o))
	need := map[string]bool{}
	for _, r := range runs {
		// refund-1045 was cancelled while it waited: its gate no longer needs anyone. ship-2001 aborted.
		if r.Group == "halted" || r.Group == "waiting" || r.ID == "ship-2001" {
			need[r.ID] = true
		}
	}
	got := map[string]bool{}
	for _, it := range l.Items {
		got[it.RunID] = true
	}
	for id := range need {
		if !got[id] {
			t.Errorf("%s needs a person and is not listed", id)
		}
	}
	for id := range got {
		if !need[id] {
			t.Errorf("%s is listed and needs nobody", id)
		}
	}
	if l.Trail {
		t.Error("no EventStore was given, but the list claims a notify trail")
	}
	for i := 1; i < len(l.Items); i++ {
		a, b := l.Items[i-1], l.Items[i]
		if !b.Since.IsZero() && (a.Since.IsZero() || b.Since.After(a.Since)) {
			t.Errorf("item %d (%s, %v) is newer than item %d (%s, %v): not newest first", i, b.RunID, b.Since, i-1, a.RunID, a.Since)
		}
	}
}

// TestAttentionApproval: a waiting gate says its call and tool, k of n, who decided, who is pending
// and how long it has waited.
func TestAttentionApproval(t *testing.T) {
	o, _ := sampleOpts(t)
	l := attentionOf(t, Handler(o))
	var it *AttentionItem
	for i := range l.Items {
		if l.Items[i].RunID == "refund-1043" {
			it = &l.Items[i]
		}
	}
	if it == nil || it.Kind != "approval" || it.Gate == nil {
		t.Fatalf("refund-1043 is not an approval item with a gate: %+v", it)
	}
	g := it.Gate
	if g.Single || g.Need != 2 || g.Of != 3 || len(g.ApprovedBy) != 1 || len(g.Pending) != 2 || it.Tool == "" || it.Call == "" {
		t.Errorf("gate = %+v, tool %q, call %q; want 2 of 3 with one approved and two pending", g, it.Tool, it.Call)
	}
	if it.Since.IsZero() || it.Waiting == "" {
		t.Errorf("since %v, waiting %q: the item does not say how long the gate has waited", it.Since, it.Waiting)
	}
	if want := durText(attnNow.Sub(it.Since)); it.Waiting != want {
		t.Errorf("waiting %q, want %q (now - since)", it.Waiting, want)
	}
}

// TestAttentionHalt: a halt carries the ResolveHalt reference the run page shows.
func TestAttentionHalt(t *testing.T) {
	o, _ := sampleOpts(t)
	l := attentionOf(t, Handler(o))
	halts := itemsOf(l, "halt")
	i := slices.IndexFunc(halts, func(it AttentionItem) bool { return it.RunID == "refund-1042" })
	if i < 0 || halts[i].Halt == nil || !strings.Contains(halts[i].Halt.Ref, "agent.HaltRef{RunID: \"refund-1042\"") {
		t.Fatalf("refund-1042 has no halt item with its HaltRef: %+v", halts)
	}
	_, page := get(t, Handler(o), "/attention")
	if !strings.Contains(page, "agent.ResolveHalt") || !strings.Contains(page, "outcome unknown") {
		t.Error("the page does not give the ResolveHalt hint")
	}
}

// abortedRun journals a saga run that aborts, at the given time.
func abortedRun(t *testing.T, s agent.Store, runID string, at time.Time) {
	t.Helper()
	boom := agent.MustFunc("boom", "fails", func(context.Context, struct{}) (string, error) { return "", context.DeadlineExceeded })
	a := agenttest.MustNew(agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "boom", `{}`)), agenttest.MustJournal(s), agent.WithTools(boom))
	tick := at.Add(-time.Hour)
	clock := func() time.Time { tick = tick.Add(10 * time.Minute); return tick }
	_, err := a.Run(context.Background(), runID, agent.UserText("go"), agent.WithSaga(), agent.WithClock(clock))
	if err == nil {
		t.Fatal("setup: the saga did not abort")
	}
}

// TestAttentionAborted: an aborted run is listed with the time it aborted (not the time it
// started) and why.
func TestAttentionAborted(t *testing.T) {
	s := agent.NewMemStore()
	abortedRun(t, s, "abort-1", time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC))
	src := JournalSource{J: agenttest.MustJournal(s)}
	recs, _ := src.History(context.Background(), "abort-1")
	at, started := abortedAt(recs), runStarted(recs)
	if at.IsZero() || started.IsZero() || !at.After(started) {
		t.Fatalf("setup: aborted at %v, started %v; want an abort time after the start", at, started)
	}
	o := Options{Source: src, Now: func() time.Time { return at.Add(90 * time.Minute) }}
	ab := itemsOf(attentionOf(t, Handler(o)), "aborted")
	if len(ab) != 1 || ab[0].RunID != "abort-1" || !ab[0].Since.Equal(at) || ab[0].Waiting != "1h 30m" || ab[0].Reason == "" {
		t.Fatalf("aborted items = %+v; want abort-1 at %v, 1h 30m ago, with a reason", ab, at)
	}
}

// TestAttentionSinceFallsBackToStart: a gate that records no ask time is dated by its run's start.
func TestAttentionSinceFallsBackToStart(t *testing.T) {
	o, _ := sampleOpts(t)
	recs, _ := o.Source.History(context.Background(), "refund-1043")
	v := Build(context.Background(), "refund-1043", recs, nil)
	for i := range v.Gates {
		v.Gates[i].AskedAt = ""
	}
	items := o.runItems(v, recs)
	if len(items) != 1 || items[0].Since.IsZero() || !items[0].Since.Equal(runStarted(recs)) {
		t.Fatalf("items = %+v; want the one gate dated by the run's start %v", items, runStarted(recs))
	}
}

// TestNotifyStatusReminders: a gate's reminders are one gate: a later reminder's acknowledgement
// counts, and another call's events do not.
func TestNotifyStatusReminders(t *testing.T) {
	t0 := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	obs := func(key string) agent.NotifyRecord {
		return agent.NotifyRecord{Kind: agent.NotifyObserved, Key: key, RunID: "r", At: t0, EventKind: string(agent.NotificationApprovalPending)}
	}
	st := agent.FoldNotify([]agent.NotifyRecord{
		obs(agent.ApprovalKey("r", "c1", 0)), obs(agent.ApprovalKey("r", "c1", 1)),
		{Kind: agent.NotifyAcked, Key: agent.ApprovalKey("r", "c1", 1), RunID: "r", At: t0.Add(time.Hour), Status: 202},
		obs(agent.ApprovalKey("r", "c10", 0)),
		obs(agent.ApprovalKey("r", "c3", 0)),
		{Kind: agent.NotifyCleared, Key: agent.ApprovalKey("r", "c3", 0), RunID: "r", At: t0, Reason: "decided"},
		obs(agent.ApprovalKey("r", "c4", 0)),
		{Kind: agent.NotifyClaimed, Key: agent.ApprovalKey("r", "c4", 0), RunID: "r", At: t0, Until: t0.Add(time.Minute)},
	})
	it := func(call string) AttentionItem { return AttentionItem{Kind: AttentionApproval, Call: call} }
	if n := notifyStatus([]*agent.NotifyState{&st}, it("c1"), "r"); n.State != NotifyAcked || !strings.Contains(n.Text, "HTTP 202") {
		t.Errorf("c1 = %+v; want acked by its reminder", n)
	}
	if n := notifyStatus([]*agent.NotifyState{&st}, it("c10"), "r"); n.State != NotifyObserved {
		t.Errorf("c10 = %+v; want observed only (c1's acknowledgement is not its)", n)
	}
	if n := notifyStatus([]*agent.NotifyState{&st}, it("c3"), "r"); n.State != NotifyCleared || !strings.Contains(n.Text, "decided") {
		t.Errorf("c3 = %+v; want cleared (decided)", n)
	}
	if n := notifyStatus([]*agent.NotifyState{&st}, it("c4"), "r"); n.State != NotifyTried {
		t.Errorf("c4 = %+v; want delivery attempted", n)
	}
	if n := notifyStatus([]*agent.NotifyState{&st}, it("c2"), "r"); n.State != NotifyNone {
		t.Errorf("c2 = %+v; want none", n)
	}
}

// TestAttentionNothing: a store whose runs all finished lists nothing, and the page says so.
func TestAttentionNothing(t *testing.T) {
	s := agent.NewMemStore()
	if _, err := sample.Write(context.Background(), s, ""); err != nil {
		t.Fatal(err)
	}
	// Only the completed runs.
	done := Exports{}
	for _, id := range []string{"refund-1041", "refund-1048"} {
		recs, _ := JournalSource{J: agenttest.MustJournal(s)}.History(context.Background(), id)
		done[id] = recs
	}
	h := Handler(Options{Source: done, Now: func() time.Time { return attnNow }})
	if l := attentionOf(t, h); len(l.Items) != 0 {
		t.Fatalf("items = %+v, want none", l.Items)
	}
	_, page := get(t, h, "/attention")
	if !strings.Contains(page, "NOTHING IN THE JOURNALS NEEDS A PERSON") || !strings.Contains(page, "not visible without the notify trail") {
		t.Error("the empty page without a trail does not say that recovery failures are not visible")
	}
	// With the whole trail it can say it.
	h = Handler(Options{Source: done, Events: audit.NewMemEventStore(), Now: func() time.Time { return attnNow }})
	_, page = get(t, h, "/attention")
	if !strings.Contains(page, "NOTHING NEEDS A PERSON") || strings.Contains(page, "NOTHING IN THE JOURNALS") {
		t.Error("the empty page with the trail does not say nothing needs a person")
	}
	h = Handler(Options{Source: done, Events: failingEvents{}, Now: func() time.Time { return attnNow }})
	_, page = get(t, h, "/attention")
	if strings.Contains(page, "NOTHING NEEDS A PERSON") {
		t.Error("the empty page says nothing needs a person although the trail could not be read")
	}
}

// trailWith appends records to runID's notify trail in es.
func trailWith(t *testing.T, es audit.EventStore, runID string, recs ...agent.NotifyRecord) {
	t.Helper()
	tr := audit.NewNotifyTrail(es)
	for i, r := range recs {
		if err := tr.Append(context.Background(), agent.NotifyTrailName(runID), i, r); err != nil {
			t.Fatal(err)
		}
	}
}

func notifyOf(l AttentionList, kind, run string) *NotifyStatus {
	for _, it := range l.Items {
		if it.Kind == kind && it.RunID == run {
			return it.Notify
		}
	}
	return nil
}

// TestAttentionNotifyTrail: with the notify trail, each item says whether it was notified and
// acknowledged; an item no notifier saw says so, and a failing recovery is listed from the trail.
func TestAttentionNotifyTrail(t *testing.T) {
	o, _ := sampleOpts(t)
	before := attentionOf(t, Handler(o))
	var gate, halt AttentionItem
	for _, it := range before.Items {
		switch {
		case it.RunID == "refund-1043" && it.Kind == "approval":
			gate = it
		case it.RunID == "refund-1042" && it.Kind == "halt":
			halt = it
		}
	}
	es := audit.NewMemEventStore()
	t0 := attnNow.Add(-time.Hour)
	// The gate: observed, claimed, acknowledged, with a deadline.
	ak := agent.ApprovalKey("refund-1043", gate.Call, 0)
	trailWith(t, es, "refund-1043",
		agent.NotifyRecord{Kind: agent.NotifyObserved, Key: ak, RunID: "refund-1043", At: t0, EventKind: string(agent.NotificationApprovalPending), EventAt: t0, DeadlineAt: t0.Add(4 * time.Hour)},
		agent.NotifyRecord{Kind: agent.NotifyClaimed, Key: ak, RunID: "refund-1043", At: t0, Holder: "n1", Until: t0.Add(time.Minute)},
		agent.NotifyRecord{Kind: agent.NotifyAcked, Key: ak, RunID: "refund-1043", At: t0.Add(time.Second), FirstSentAt: t0, Attempts: 1, Status: 204},
		agent.NotifyRecord{Kind: agent.NotifyFailingSince, RunID: "refund-1043", At: t0, Reason: "store_error"})
	// The halt: observed and claimed, never acknowledged, delivery failing.
	hk := agent.HaltedKey("refund-1042", agent.OpTool, halt.Call, halt.Halt.AttemptedAt.UnixMilli())
	trailWith(t, es, "refund-1042",
		agent.NotifyRecord{Kind: agent.NotifyObserved, Key: hk, RunID: "refund-1042", At: t0, EventKind: string(agent.NotificationOutcomeUnknown), EventAt: t0, DeadlineAt: t0.Add(time.Minute)},
		agent.NotifyRecord{Kind: agent.NotifyClaimed, Key: hk, RunID: "refund-1042", At: t0, Holder: "n1", Until: t0.Add(time.Minute)},
		agent.NotifyRecord{Kind: agent.NotifyFailing, Key: hk, RunID: "refund-1042", At: t0, Failures: 3},
		agent.NotifyRecord{Kind: agent.NotifyOverdue, Key: hk, RunID: "refund-1042", At: t0.Add(time.Minute)})
	// A run that is not otherwise waiting: its recovery keeps failing.
	// A finished run whose recovery once failed needs nobody.
	trailWith(t, es, "refund-1041",
		agent.NotifyRecord{Kind: agent.NotifyFailingSince, RunID: "refund-1041", At: t0, Reason: "store_error"})
	o.Events = es
	l := attentionOf(t, Handler(o))
	if !l.Trail {
		t.Fatal("the EventStore was given, but the list says there is no notify trail")
	}
	if n := notifyOf(l, "approval", "refund-1043"); n == nil || n.State != "acked" || !strings.Contains(n.Text, "acknowledged") || n.Deadline.IsZero() {
		t.Errorf("gate notify = %+v; want acked with a deadline", n)
	}
	if n := notifyOf(l, "halt", "refund-1042"); n == nil || n.State != "failing" || !n.Overdue || strings.Contains(n.Text, "acknowledged at") {
		t.Errorf("halt notify = %+v; want failing, overdue, not acknowledged", n)
	}
	// A run with items and no trail records: not notified.
	for _, it := range l.Items {
		if it.Notify == nil {
			t.Errorf("%s %s: no notify status although the trail is available", it.Kind, it.RunID)
		}
	}
	rec := itemsOf(l, "recovery")
	if len(rec) != 1 || rec[0].RunID != "refund-1043" || !rec[0].Since.Equal(t0) || rec[0].Reason != "store_error" {
		t.Errorf("recovery items = %+v; want refund-1043 failing since %v (store_error)", rec, t0)
	}
	_, page := get(t, Handler(o), "/attention")
	for _, want := range []string{"acknowledged", "delivery failing", "overdue", "recovery"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not say %q", want)
		}
	}
}

// TestAttentionTrailUnreadable: a notify trail that cannot be read is said on the page, and the
// list still shows what the journals say.
func TestAttentionTrailUnreadable(t *testing.T) {
	o, _ := sampleOpts(t)
	o.Events = failingEvents{}
	l := attentionOf(t, Handler(o))
	if len(l.Items) == 0 || len(l.Notes) == 0 || !strings.Contains(strings.Join(l.Notes, " "), "notify trail") {
		t.Errorf("items %d, notes %q; want the items and a note that the trail could not be read", len(l.Items), l.Notes)
	}
}

type failingEvents struct{ audit.EventStore }

func (failingEvents) Load(context.Context, string) ([][]byte, error) {
	return nil, context.DeadlineExceeded
}

// TestAttentionApproveCommand: a pending approval shows the command from the ApproveCommand hook, and
// says why there is none without it.
func TestAttentionApproveCommand(t *testing.T) {
	o, _ := sampleOpts(t)
	_, plain := get(t, Handler(o), "/attention")
	if strings.Contains(plain, `class="attn-cmd">bide approve`) || !strings.Contains(plain, "No approve command") {
		t.Error("without the hook the page shows a bide approve command or does not say there is none")
	}
	var seen []string
	o.ApproveCommand = func(runID, toolUseID string, single bool) string {
		seen = append(seen, runID+"/"+toolUseID)
		return "bide approve -db j.db -run " + runID + " -call " + toolUseID + " <&>"
	}
	_, page := get(t, Handler(o), "/attention")
	if !strings.Contains(page, "bide approve -db j.db -run refund-1043 -call ") || !strings.Contains(page, "&lt;&amp;&gt;") {
		t.Errorf("the page does not show the (escaped) command; hook saw %v", seen)
	}
	l := attentionOf(t, Handler(o))
	for _, it := range l.Items {
		if (it.Kind == "approval") != (it.Command != "") {
			t.Errorf("%s %s: command %q (only an approval has one)", it.Kind, it.RunID, it.Command)
		}
	}
}

// TestAttentionLive: the page of a store source refreshes itself without a script, and an export's
// does not.
func TestAttentionLive(t *testing.T) {
	o, _ := sampleOpts(t)
	_, page := get(t, Handler(o), "/attention")
	if !strings.Contains(page, `http-equiv="refresh"`) {
		t.Error("a live source's page does not refresh")
	}
	if l := attentionOf(t, Handler(o)); !l.Live {
		t.Error("a live source's list does not say it is live")
	}
	exp := Exports{}
	recs, _ := o.Source.History(context.Background(), "refund-1043")
	exp["refund-1043"] = recs
	h := Handler(Options{Source: exp, Now: o.Now})
	_, static := get(t, h, "/attention")
	if strings.Contains(static, `http-equiv="refresh"`) {
		t.Error("a source that does not grow refreshes")
	}
	if l := attentionOf(t, h); l.Live || len(l.Items) != 1 {
		t.Errorf("export: live %v, %d items; want static with refund-1043's gate", l.Live, len(l.Items))
	}
}

// TestAttentionNoScript: the page works without JavaScript: it has no inline script and the list is
// in the HTML.
func TestAttentionNoScript(t *testing.T) {
	o, _ := sampleOpts(t)
	_, page := get(t, Handler(o), "/attention")
	if strings.Contains(page, "<script>") || !strings.Contains(page, "refund-1043") {
		t.Error("the list is not plain HTML")
	}
}

// TestRunListLinksToAttention: the run list's headline and its "need a person" number link to the
// list.
func TestRunListLinksToAttention(t *testing.T) {
	o, _ := sampleOpts(t)
	_, page := get(t, Handler(o), "/")
	if n := strings.Count(page, `href="/attention"`); n < 2 {
		t.Errorf("the run list links to /attention %d times, want the banner and the stat", n)
	}
	clear := Exports{}
	h := Handler(Options{Source: clear})
	_, page = get(t, h, "/")
	if strings.Contains(page, `href="/attention"`) {
		t.Error("an empty store links to the attention list")
	}
}

// TestRunListHeadlineAndTheTrail: the run list is ALL CLEAR only when the notify trail was read for
// every run and none has a failing recovery; without it, or with it unreadable, it says what it
// cannot rule out; with a failing recovery it says so, and links to the list.
func TestRunListHeadlineAndTheTrail(t *testing.T) {
	_, runs := sampleOpts(t)
	s := agent.NewMemStore()
	if _, err := sample.Write(context.Background(), s, ""); err != nil {
		t.Fatal(err)
	}
	src := JournalSource{J: agenttest.MustJournal(s)}
	// A store of finished runs only.
	done := Exports{}
	for _, id := range []string{"refund-1041", "refund-1048"} {
		done[id], _ = src.History(context.Background(), id)
	}
	word := func(o Options) string {
		l := listOf(t, Handler(o), "")
		return l.Verdict().Word
	}
	if w := word(Options{Source: done}); w != "NOTHING IN THE JOURNALS NEEDS A PERSON" {
		t.Errorf("no trail: %q", w)
	}
	if w := word(Options{Source: done, Events: audit.NewMemEventStore()}); w != "ALL CLEAR" {
		t.Errorf("empty trail: %q", w)
	}
	if w := word(Options{Source: done, Events: failingEvents{}}); w != "NOTHING IN THE JOURNALS NEEDS A PERSON" {
		t.Errorf("unreadable trail: %q", w)
	}
	// An in-progress run whose recovery keeps failing.
	var inProgress string
	for _, r := range runs {
		if r.Group == "in-progress" {
			inProgress = r.ID
		}
	}
	if inProgress == "" {
		t.Fatal("setup: the sample has no run in progress")
	}
	both := Exports{inProgress: nil}
	both[inProgress], _ = src.History(context.Background(), inProgress)
	for k, v := range done {
		both[k] = v
	}
	es := audit.NewMemEventStore()
	trailWith(t, es, inProgress, agent.NotifyRecord{Kind: agent.NotifyFailingSince, RunID: inProgress, At: attnNow, Reason: "store_error"})
	trailWith(t, es, "refund-1041", agent.NotifyRecord{Kind: agent.NotifyFailingSince, RunID: "refund-1041", At: attnNow}) // finished: not counted
	h := Handler(Options{Source: both, Events: es})
	if w := listOf(t, h, "").Verdict().Word; w != "1 RECOVERY FAILING" {
		t.Errorf("a failing recovery: %q", w)
	}
	if _, page := get(t, h, "/"); !strings.Contains(page, `<p class="vlink"><a href="/attention">`) {
		t.Error("the headline does not link to the list for a failing recovery")
	}
	// A kind filter that admits no run does not count its recovery.
	if l := listOf(t, h, "?kind=flow"); l.Recovering != 0 {
		t.Errorf("a filtered-out run's recovery is counted: %+v", l.Recovering)
	}
}

// TestTreeRootMatchesAgent: treeRoot is the root of a sub-run ID that agent.SubRunID builds, and of
// a session's runs.
func TestTreeRootMatchesAgent(t *testing.T) {
	for _, c := range []struct{ id, want string }{
		{"order-1", "order-1"},
		{agent.SubRunID("order-1", "call/1"), "order-1"},
		{agent.SubRunID(agent.SubRunID("order-1", "a"), "b"), "order-1"},
		{"sess>@turn/2", "sess>@turn/2"},
		{agent.SubRunID("sess>@turn/2", "c"), "sess>@turn/2"},
		{"sess>@session", "sess>@session"},
	} {
		if got := treeRoot(c.id); got != c.want {
			t.Errorf("treeRoot(%q) = %q, want %q", c.id, got, c.want)
		}
	}
}

// TestAttentionSubRunNotification: a notification a notifier recorded for a sub-agent's run lives in
// its root run's trail; the sub-run's item is matched to it, and the root's failing recovery is not
// repeated on the sub-run.
func TestAttentionSubRunNotification(t *testing.T) {
	o, _ := sampleOpts(t)
	recs, _ := o.Source.History(context.Background(), "refund-1043")
	child := agent.SubRunID("parent-1", "call-9")
	// The parent run itself: the gate's records under another ID are all the viewer needs.
	exp := Exports{child: recs, "parent-1": recs}
	v := Build(context.Background(), child, recs, nil)
	if len(v.Gates) != 1 {
		t.Fatalf("setup: %d gates", len(v.Gates))
	}
	call := v.Gates[0].ToolUseID
	es := audit.NewMemEventStore()
	t0 := attnNow.Add(-time.Hour)
	ak := agent.ApprovalKey(child, call, 0) // keyed by the run that paused, filed under the root's trail
	trailWith(t, es, "parent-1",
		agent.NotifyRecord{Kind: agent.NotifyObserved, Key: ak, RunID: "parent-1", At: t0, EventKind: string(agent.NotificationApprovalPending), EventAt: t0},
		agent.NotifyRecord{Kind: agent.NotifyClaimed, Key: ak, RunID: "parent-1", At: t0, Holder: "n", Until: t0.Add(time.Minute)},
		agent.NotifyRecord{Kind: agent.NotifyAcked, Key: ak, RunID: "parent-1", At: t0.Add(time.Second), Status: 200},
		agent.NotifyRecord{Kind: agent.NotifyFailingSince, RunID: "parent-1", At: t0})
	l := attentionOf(t, Handler(Options{Source: exp, Events: es, Now: o.Now}))
	if n := notifyOf(l, "approval", child); n == nil || n.State != NotifyAcked {
		t.Errorf("sub-run notify = %+v, want acked from the root's trail", n)
	}
	for _, it := range itemsByKind(l.Items, AttentionRecovery) {
		if it.RunID == child {
			t.Errorf("the sub-run repeats its root's failing recovery: %+v", it)
		}
	}
	if len(itemsByKind(l.Items, AttentionRecovery)) != 1 {
		t.Errorf("recovery items = %+v, want the root's alone", itemsByKind(l.Items, AttentionRecovery))
	}
}
