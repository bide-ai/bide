package runview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// countingSource counts the History reads a page makes.
type countingSource struct {
	JournalSource
	reads atomic.Int64
}

func (c *countingSource) History(ctx context.Context, id string) ([]agent.Record, error) {
	c.reads.Add(1)
	return c.JournalSource.History(ctx, id)
}

// TestAttentionReadsNoFinishedRun: over a store the list reads the runs with no run:complete marker
// only, the store filtering the rest in its listing, so a store of finished runs costs it nothing.
func TestAttentionReadsNoFinishedRun(t *testing.T) {
	o, runs := sampleOpts(t)
	src := &countingSource{JournalSource: o.Source.(JournalSource)}
	o.Source = src
	l := o.attention(t.Context())
	completed := 0
	for _, r := range runs {
		recs, _ := src.JournalSource.History(t.Context(), r.ID)
		for _, rec := range recs {
			if rec.Name == "run:complete" {
				completed++
				break
			}
		}
	}
	src.reads.Store(0)
	l = o.attention(t.Context())
	if got, want := int(src.reads.Load()), len(runs)-completed; got != want || l.Runs != want {
		t.Errorf("read %d runs (%d counted), want the %d without run:complete", got, l.Runs, want)
	}
}

// TestPagesAreCached: a page asked for again within the cache's life is not read again, and
// concurrent requests share one read.
func TestPagesAreCached(t *testing.T) {
	o, _ := sampleOpts(t)
	src := &countingSource{JournalSource: o.Source.(JournalSource)}
	o.Source = src
	h := Handler(o)
	get(t, h, "/attention.json")
	n := src.reads.Load()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); get(t, h, "/attention"); get(t, h, "/attention.json") }()
	}
	wg.Wait()
	if src.reads.Load() != n {
		t.Errorf("the list was read again within its cache life: %d reads, then %d", n, src.reads.Load())
	}
	get(t, h, "/runs.json")
	m := src.reads.Load()
	get(t, h, "/runs.json")
	if src.reads.Load() != m {
		t.Error("the run list was read again within its cache life")
	}
}

// TestPageStatusAgreesWithHeadline: the JSON status is the headline's: attention for an item, clear
// only for a complete clear page, unknown otherwise; both pages, every case.
func TestPageStatusAgreesWithHeadline(t *testing.T) {
	o, _ := sampleOpts(t)
	done := Exports{}
	recs, _ := o.Source.History(context.Background(), "refund-1041")
	done["refund-1041"] = recs
	for name, c := range map[string]Options{
		"items":         {Source: o.Source, Events: audit.NewMemEventStore()},
		"clear":         {Source: done, Events: audit.NewMemEventStore()},
		"no trail":      {Source: done},
		"dead trail":    {Source: done, Events: failingEvents{}},
		"cannot list":   {Source: JournalSource{J: noListJournal(t)}, Events: audit.NewMemEventStore()},
		"none at all":   {Source: Exports{}},
		"unlisted runs": {Source: flakySource{Exports: done, runsErr: errors.New("x")}, Events: audit.NewMemEventStore()},
	} {
		c.Now = o.Now
		h := Handler(c)
		for _, page := range []struct{ html, json string }{{"/", "/runs.json"}, {"/attention", "/attention.json"}} {
			_, body := get(t, h, page.html)
			_, js := get(t, h, page.json)
			var s struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal([]byte(js), &s); err != nil {
				t.Fatal(err)
			}
			var word string
			var status string
			if page.html == "/" {
				v := c.runList(t.Context(), ListFilter{Limit: 50}).Verdict()
				word, status = v.Word, v.Status
			} else {
				v := c.attention(t.Context()).Verdict()
				word, status = v.Word, v.Status
			}
			if s.Status != status || !strings.Contains(body, `id="vword">`+word+`</p>`) {
				t.Errorf("%s %s: json status %q, headline %q (%s)", name, page.html, s.Status, word, status)
			}
			if (status == StatusClear) != (word == "ALL CLEAR" || word == "NOTHING NEEDS A PERSON" || word == "NO RUNS") {
				t.Errorf("%s %s: status %q for headline %q", name, page.html, status, word)
			}
		}
	}
}

// TestAttentionJSONHasEmptyItems: a list with nothing in it has "items": [] and not null.
func TestAttentionJSONHasEmptyItems(t *testing.T) {
	_, js := get(t, Handler(Options{Source: Exports{}}), "/attention.json")
	if !strings.Contains(js, `"items": []`) {
		t.Errorf("empty list JSON: %s", js)
	}
}

// TestAttentionHidesStoreErrors: the page and the JSON say the class of a store error, not its text,
// which can carry a connection string.
func TestAttentionHidesStoreErrors(t *testing.T) {
	o, _ := sampleOpts(t)
	secret := errors.New(`pgx: failed to connect to host=db user=app password=hunter2`)
	for name, c := range map[string]Options{
		"trail":   {Source: o.Source, Events: leakyEvents{secret}},
		"listing": {Source: flakySource{Exports: Exports{}, runsErr: secret}},
		"history": {Source: flakySource{Exports: Exports{"a": sampleRecs(t, "refund-1041")}, historyBad: map[string]bool{"a": true}}, Events: audit.NewMemEventStore()},
	} {
		h := Handler(c)
		for _, path := range []string{"/", "/attention", "/runs.json", "/attention.json"} {
			if _, body := get(t, h, path); strings.Contains(body, "hunter2") || strings.Contains(body, "pgx") {
				t.Errorf("%s %s shows the store's own error text", name, path)
			}
		}
	}
}

type leakyEvents struct{ err error }

func (l leakyEvents) Append(context.Context, string, int, []byte) error { return l.err }
func (l leakyEvents) Load(context.Context, string) ([][]byte, error)    { return nil, l.err }

// TestAttentionEmptyRunsAreCounted: a listed run with no records is counted, noted and not silent.
func TestAttentionEmptyRunsAreCounted(t *testing.T) {
	x := Exports{"e1": nil, "e2": nil, "ok": sampleRecs(t, "refund-1043")}
	l := attentionOf(t, Handler(Options{Source: x, Now: func() time.Time { return attnNow }}))
	if l.Empty != 2 || !strings.Contains(strings.Join(l.Notes, " "), "2 listed runs hold no records") {
		t.Errorf("empty %d, notes %q", l.Empty, l.Notes)
	}
	if rl := listOf(t, Handler(Options{Source: x}), ""); rl.Empty != 2 {
		t.Errorf("run list empty %d, want 2", rl.Empty)
	}
}

// TestNotifyStatusEarlierRound: an acknowledgement of an earlier attempt or round is not this one's.
func TestNotifyStatusEarlierRound(t *testing.T) {
	t0 := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	ack := func(key string, at time.Time, kind agent.NotificationKind) []agent.NotifyRecord {
		return []agent.NotifyRecord{
			{Kind: agent.NotifyObserved, Key: key, RunID: "r", At: at, EventKind: string(kind)},
			{Kind: agent.NotifyAcked, Key: key, RunID: "r", At: at, Status: 200},
		}
	}
	// A halt attempted at t0 and acknowledged, then attempted again an hour later (a new marker).
	h1, h2 := t0, t0.Add(time.Hour)
	st := agent.FoldNotify(ack(agent.HaltedKey("r", agent.OpTool, "c1", h1.UnixMilli()), h1, agent.NotificationOutcomeUnknown))
	it := AttentionItem{Kind: AttentionHalt, Call: "c1", Halt: &Halt{Op: "tool", ID: "c1", AttemptedAt: h2}, Since: h2}
	if n := notifyStatus([]*agent.NotifyState{&st}, it, "r"); n.State != NotifyNone {
		t.Errorf("the earlier attempt's acknowledgement marks the new attempt: %+v", n)
	}
	it.Halt.AttemptedAt, it.Since = h1, h1
	if n := notifyStatus([]*agent.NotifyState{&st}, it, "r"); n.State != NotifyAcked {
		t.Errorf("the attempt's own acknowledgement is not found: %+v", n)
	}
	// An approval gate asked again after an earlier acknowledged ask.
	st = agent.FoldNotify(ack(agent.ApprovalKey("r", "c1", 0), t0, agent.NotificationApprovalPending))
	ap := AttentionItem{Kind: AttentionApproval, Call: "c1", Since: t0.Add(time.Hour)}
	if n := notifyStatus([]*agent.NotifyState{&st}, ap, "r"); n.State != NotifyNone {
		t.Errorf("an acknowledgement from before the gate was asked marks it acknowledged: %+v", n)
	}
	// An untimed halt: only the newest event counts.
	var recs []agent.NotifyRecord
	recs = append(recs, ack(agent.HaltedKey("r", agent.OpStep, "s", 1), t0, agent.NotificationOutcomeUnknown)...)
	recs = append(recs, agent.NotifyRecord{Kind: agent.NotifyObserved, Key: agent.HaltedKey("r", agent.OpStep, "s", 2), RunID: "r", At: t0.Add(time.Hour), EventKind: string(agent.NotificationOutcomeUnknown)})
	st = agent.FoldNotify(recs)
	un := AttentionItem{Kind: AttentionHalt, Call: "s", Halt: &Halt{Op: "step", ID: "s"}}
	if n := notifyStatus([]*agent.NotifyState{&st}, un, "r"); n.State != NotifyObserved {
		t.Errorf("an untimed halt takes an older generation's acknowledgement: %+v", n)
	}
}

// TestNotifyStatusClearedWhileOpen: a notifier that cleared an event the journal still shows open is
// called out, in the warning color.
func TestNotifyStatusClearedWhileOpen(t *testing.T) {
	t0 := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	k := agent.ApprovalKey("r", "c1", 0)
	st := agent.FoldNotify([]agent.NotifyRecord{
		{Kind: agent.NotifyObserved, Key: k, RunID: "r", At: t0, EventKind: string(agent.NotificationApprovalPending)},
		{Kind: agent.NotifyCleared, Key: k, RunID: "r", At: t0, Reason: "decided"},
	})
	n := notifyStatus([]*agent.NotifyState{&st}, AttentionItem{Kind: AttentionApproval, Call: "c1"}, "r")
	if n.State != NotifyCleared || !strings.Contains(n.Text, "still shows it needing a person") || n.Class() != "bad" {
		t.Errorf("cleared while open: %+v (class %s)", n, n.Class())
	}
}

// TestRunListTrailReads: the run list reads the trail of an unfinished root run only, plus a probe when
// there is none, and a dead store is still noticed.
func TestRunListTrailReads(t *testing.T) {
	done := sampleRecs(t, "refund-1041")
	x := Exports{}
	for i := range 100 {
		x[fmt.Sprintf("r%03d", i)] = done
	}
	c := &countingEvents{EventStore: audit.NewMemEventStore()}
	l := Options{Source: x, Events: c}.runList(t.Context(), ListFilter{Limit: 50})
	if c.loads.Load() != 1 || l.Verdict().Word != "ALL CLEAR" {
		t.Errorf("%d trail loads for 100 finished runs, verdict %q; want one probe and ALL CLEAR", c.loads.Load(), l.Verdict().Word)
	}
	if l := (Options{Source: x, Events: failingEvents{}}).runList(t.Context(), ListFilter{Limit: 50}); l.TrailUnread == 0 || l.Verdict().Word == "ALL CLEAR" {
		t.Errorf("a dead trail store over finished runs: unread %d, verdict %q", l.TrailUnread, l.Verdict().Word)
	}
}

// TestCoverageComplete: each way a page can fall short of reading everything makes it incomplete, and
// each is said in its gap line; errors are named by class.
func TestCoverageComplete(t *testing.T) {
	full := Coverage{Listable: true, Trail: true}
	if !full.complete() || full.Gap() != "" {
		t.Fatalf("a full coverage is incomplete: %q", full.Gap())
	}
	for name, c := range map[string]Coverage{
		"not listable": {Trail: true},
		"unread":       {Listable: true, Trail: true, Unread: 2},
		"err":          {Listable: true, Trail: true, Err: "listing failed (store error)"},
		"truncated":    {Listable: true, Trail: true, Truncated: 4},
		"filtered":     {Listable: true, Trail: true, Filtered: true},
		"trail":        {Listable: true, Trail: true, TrailUnread: 1},
		"no trail":     {Listable: true},
	} {
		if c.complete() {
			t.Errorf("%s: complete", name)
		}
		if name != "no trail" && c.Gap() == "" {
			t.Errorf("%s: no gap line", name)
		}
		if name != "trail" && name != "no trail" && c.journalsComplete() {
			t.Errorf("%s: the journals are complete", name)
		}
	}
	for err, want := range map[error]string{
		context.DeadlineExceeded:                                    "timed out",
		context.Canceled:                                            "canceled",
		fmt.Errorf("x: %w", agent.ErrStorage):                       "store error",
		errors.New("password=hunter2"):                              "read error",
		fmt.Errorf("w: %w (%w)", errors.New("a"), agent.ErrStorage): "store error",
	} {
		if got := errClass(err); got != want {
			t.Errorf("errClass(%v) = %q, want %q", err, got, want)
		}
	}
}

// TestRunListReadsEachUnfinishedRunsOwnTrail: recovery is recorded under the run it was run for, so the
// run list reads one trail per unfinished run, and none for a finished one.
func TestRunListReadsEachUnfinishedRunsOwnTrail(t *testing.T) {
	wait := sampleRecs(t, "refund-1043")
	x := Exports{"root": wait, agent.SubRunID("root", "c1"): wait, agent.SubRunID("root", "c2"): wait}
	c := &countingEvents{EventStore: audit.NewMemEventStore()}
	Options{Source: x, Events: c}.runList(t.Context(), ListFilter{Limit: 50})
	if c.loads.Load() != 3 {
		t.Errorf("%d trail loads for three unfinished runs, want 3", c.loads.Load())
	}
}

// TestAttentionFlowGateTrails: a flow gate files its events under its own run's trail even when the
// run's ID holds '>', and a sub-agent's under its root's; both are read, for items and for recovery.
func TestAttentionFlowGateTrails(t *testing.T) {
	const own = "wf>1"
	recs := sampleRecs(t, "refund-1043")
	call := Build(t.Context(), own, recs, nil).Gates[0].ToolUseID
	t0 := attnNow.Add(-time.Hour)
	es := audit.NewMemEventStore()
	k := agent.ApprovalKey(own, call, 0)
	trailWith(t, es, own,
		agent.NotifyRecord{Kind: agent.NotifyObserved, Key: k, RunID: own, At: t0, EventKind: string(agent.NotificationApprovalPending), EventAt: t0},
		agent.NotifyRecord{Kind: agent.NotifyAcked, Key: k, RunID: own, At: t0.Add(time.Second), Status: 200},
		agent.NotifyRecord{Kind: agent.NotifyFailingSince, RunID: own, At: t0, Reason: "resume_error"})
	o := Options{Source: Exports{own: recs}, Events: es, Now: func() time.Time { return attnNow }}
	l := o.attention(t.Context())
	if n := notifyOf(l, "approval", own); n == nil || n.State != NotifyAcked {
		t.Errorf("the run's own trail was not read: %+v", n)
	}
	if rec := itemsByKind(l.Items, AttentionRecovery); len(rec) != 1 || rec[0].RunID != own {
		t.Errorf("recovery items %+v; want the run's own failing recovery", rec)
	}
	if rl := o.runList(t.Context(), ListFilter{Limit: 50}); rl.Recovering != 1 {
		t.Errorf("run list recovering %d, want 1", rl.Recovering)
	}
	// Neither trail records it: none, not a read failure.
	l = Options{Source: Exports{own: recs}, Events: audit.NewMemEventStore(), Now: o.Now}.attention(t.Context())
	if n := notifyOf(l, "approval", own); n == nil || n.State != NotifyNone || l.TrailUnread != 0 {
		t.Errorf("empty trails: %+v, unread %d", n, l.TrailUnread)
	}
}

// namedEvents records the trail names read.
type namedEvents struct {
	audit.EventStore
	mu    sync.Mutex
	names []string
}

func (n *namedEvents) Load(ctx context.Context, name string) ([][]byte, error) {
	n.mu.Lock()
	n.names = append(n.names, name)
	n.mu.Unlock()
	return n.EventStore.Load(ctx, name)
}

// TestProbeReadsTheRunsTrail: the probe that notices a dead store reads notify:<run>, not a name with
// the prefix twice.
func TestProbeReadsTheRunsTrail(t *testing.T) {
	done := sampleRecs(t, "refund-1041")
	for name, read := range map[string]func(Options){
		"attention": func(o Options) { o.attention(t.Context()) },
		"run list":  func(o Options) { o.runList(t.Context(), ListFilter{Limit: 50}) },
	} {
		ev := &namedEvents{EventStore: audit.NewMemEventStore()}
		read(Options{Source: Exports{"only": done}, Events: ev})
		if len(ev.names) != 1 || ev.names[0] != "notify:only" {
			t.Errorf("%s: trail reads %q, want [notify:only]", name, ev.names)
		}
	}
}

// hangSource blocks every read until its context ends.
type hangSource struct{ Exports }

func (hangSource) History(ctx context.Context, _ string) ([]agent.Record, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestCachedBuildTimesOut: a store that hangs gives a page that says the runs timed out, not a hang
// and not a clear page.
func TestCachedBuildTimesOut(t *testing.T) {
	old := buildTimeout
	buildTimeout = 50 * time.Millisecond
	defer func() { buildTimeout = old }()
	h := Handler(Options{Source: hangSource{Exports{"a": nil}}, Events: audit.NewMemEventStore()})
	for _, path := range []string{"/attention.json", "/runs.json"} {
		_, js := get(t, h, path)
		var s struct {
			Status string `json:"status"`
			Error  string `json:"error"`
			Unread int    `json:"unread"`
		}
		if err := json.Unmarshal([]byte(js), &s); err != nil {
			t.Fatal(err)
		}
		if s.Status != StatusUnknown || !strings.Contains(s.Error, "timed out") || s.Unread != 1 {
			t.Errorf("%s: %+v", path, s)
		}
	}
}

// onlyFailing fails the load of one trail name.
type onlyFailing struct {
	audit.EventStore
	name string
}

func (o onlyFailing) Load(ctx context.Context, name string) ([][]byte, error) {
	if name == o.name {
		return nil, context.DeadlineExceeded
	}
	return o.EventStore.Load(ctx, name)
}

// TestAttentionOwnTrailUnreadIsCounted: when the run's own trail cannot be read but its root's can, the
// list still counts the run's trail as unread and is not clear.
func TestAttentionOwnTrailUnreadIsCounted(t *testing.T) {
	const own = "wf>1"
	recs := sampleRecs(t, "refund-1043")
	o := Options{Source: Exports{own: recs}, Events: onlyFailing{audit.NewMemEventStore(), agent.NotifyTrailName(own)}, Now: func() time.Time { return attnNow }}
	if l := o.attention(t.Context()); l.TrailUnread != 1 {
		t.Errorf("trail unread %d, want 1", l.TrailUnread)
	}
}
