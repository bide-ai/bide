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

// TestRunListReadsRootTrailsOnly: a sub-run's trail is its root's, so the run list reads one trail
// per unfinished root run.
func TestRunListReadsRootTrailsOnly(t *testing.T) {
	wait := sampleRecs(t, "refund-1043")
	x := Exports{"root": wait, agent.SubRunID("root", "c1"): wait, agent.SubRunID("root", "c2"): wait}
	c := &countingEvents{EventStore: audit.NewMemEventStore()}
	Options{Source: x, Events: c}.runList(t.Context(), ListFilter{Limit: 50})
	if c.loads.Load() != 1 {
		t.Errorf("%d trail loads for one root run and two sub-runs, want 1", c.loads.Load())
	}
}
