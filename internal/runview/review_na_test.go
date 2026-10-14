package runview

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/sample"
)

func sampleRecs(t *testing.T, id string) []agent.Record {
	t.Helper()
	s := agent.NewMemStore()
	if _, err := sample.Write(context.Background(), s, ""); err != nil {
		t.Fatal(err)
	}
	recs, err := JournalSource{J: agenttest.MustJournal(s)}.History(context.Background(), id)
	if err != nil || len(recs) == 0 {
		t.Fatal(err, len(recs))
	}
	return recs
}

// H1: a store that cannot list its runs gets "NOTHING NEEDS A PERSON" from the attention page.
func TestReviewNoListerIsNotClear(t *testing.T) {
	o := Options{Source: JournalSource{J: noListJournal(t)}, Events: audit.NewMemEventStore()}
	l := o.attention(t.Context())
	if v := l.Verdict(); v.Word != "CANNOT LIST" || v.Status != StatusUnknown || l.Listable {
		t.Errorf("attention verdict %q (%s), listable %v for a store that cannot list runs", v.Word, v.Status, l.Listable)
	}
	rl := o.runList(t.Context(), ListFilter{Limit: 50})
	if v := rl.Verdict(); v.Word != "CANNOT LIST" || v.Status != StatusUnknown || rl.Listable {
		t.Errorf("run list verdict %q (%s), listable %v for a store that cannot list runs", v.Word, v.Status, rl.Listable)
	}
}

// H2: more than maxSummarized runs: the unread runs hide an approval, headline still clear.
func TestReviewTruncationIsNotClear(t *testing.T) {
	done := sampleRecs(t, "refund-1041")
	waiting := sampleRecs(t, "refund-1043")
	x := Exports{}
	for i := 0; i < maxSummarized; i++ {
		x[fmt.Sprintf("a%05d", i)] = done
	}
	x["zzz-waiting"] = waiting // sorts after the cut
	o := Options{Source: x, Events: audit.NewMemEventStore()}
	// The Needs attention list has no cap: it reads the waiting run past the run list's cut.
	a := o.attention(t.Context())
	if len(itemsByKind(a.Items, AttentionApproval)) != 1 || a.Truncated != 0 || a.Verdict().Status != StatusAttention {
		t.Errorf("attention items %+v, truncated %d, status %s; want the waiting run found past the cut", a.Items, a.Truncated, a.Verdict().Status)
	}
	// The run list is cut, and says so in its fields and its headline.
	l := o.runList(t.Context(), ListFilter{Limit: 50})
	if v := l.Verdict(); l.Truncated != 1 || v.Status == StatusClear || v.Word == "ALL CLEAR" {
		t.Errorf("run list verdict %q (%s), truncated %d although one run was never read", v.Word, v.Status, l.Truncated)
	}
}

// partial source: Runs fails after listing, or History fails for one run.
type flakySource struct {
	Exports
	runsErr    error
	historyBad map[string]bool
}

func (f flakySource) Runs(ctx context.Context) ([]string, error) {
	ids, _ := f.Exports.Runs(ctx)
	return ids, f.runsErr
}
func (f flakySource) History(ctx context.Context, id string) ([]agent.Record, error) {
	if f.historyBad[id] {
		return nil, errors.New("read failed")
	}
	return f.Exports.History(ctx, id)
}

// H3: the run list says ALL CLEAR although a listing error or an unreadable run is reported.
func TestReviewRunListReadErrorIsNotClear(t *testing.T) {
	done := sampleRecs(t, "refund-1041")
	waiting := sampleRecs(t, "refund-1043")
	es := audit.NewMemEventStore()
	for name, src := range map[string]flakySource{
		"runs error":    {Exports: Exports{"a": done}, runsErr: errors.New("listing broke")},
		"history error": {Exports: Exports{"a": done, "b": waiting}, historyBad: map[string]bool{"b": true}},
	} {
		l := Options{Source: src, Events: es}.runList(t.Context(), ListFilter{Limit: 50})
		if l.Err == "" {
			t.Fatalf("%s: setup: no error reported", name)
		}
		if v := l.Verdict(); v.Word == "ALL CLEAR" || v.Status == StatusClear || l.Unread == 0 {
			t.Errorf("%s: run list verdict %q (%s), unread %d with error %q", name, v.Word, v.Status, l.Unread, l.Err)
		}
	}
	// attention: an unreadable run among readable ones, none needing a person
	a := Options{Source: flakySource{Exports: Exports{"a": done, "b": waiting}, historyBad: map[string]bool{"b": true}}, Events: es}.attention(t.Context())
	if v := a.Verdict(); v.Status == StatusClear || a.Unread != 1 || a.Err == "" {
		t.Errorf("attention verdict %q (%s), unread %d, err %q with an unreadable run", v.Word, v.Status, a.Unread, a.Err)
	}
}

// H4: a sub-run's notification is recorded in the ROOT run's trail (agent/notify_hooks.go rootOr,
// notify_engine.go roundLocked) under the sub-run's key; the page reads only the item's own run's trail.
func TestReviewSubRunNotificationFound(t *testing.T) {
	const sub, root = "refund-1043", "root-1"
	// The journal records no root, so the root is read from the run ID (agent.SubRunID); a run whose
	// ID says nothing of its root is found by the trails of the other runs read, root-1 among them.
	x := Exports{sub: sampleRecs(t, sub), root: sampleRecs(t, "refund-1044")}
	l0 := Options{Source: x, Now: func() time.Time { return attnNow }}.attention(t.Context())
	var call string
	for _, it := range l0.Items {
		if it.Kind == "approval" {
			call = it.Call
		}
	}
	es := audit.NewMemEventStore()
	k := agent.ApprovalKey(sub, call, 0)
	t0 := attnNow.Add(-time.Hour)
	trailWith(t, es, root,
		agent.NotifyRecord{Kind: agent.NotifyObserved, Key: k, RunID: root, At: t0, EventKind: string(agent.NotificationApprovalPending), EventAt: t0, DeadlineAt: t0.Add(time.Hour)},
		agent.NotifyRecord{Kind: agent.NotifyAcked, Key: k, RunID: root, At: t0, Status: 204})
	l := Options{Source: x, Events: es, Now: func() time.Time { return attnNow }}.attention(t.Context())
	n := notifyOf(l, "approval", sub)
	if n == nil || n.State != NotifyAcked {
		t.Errorf("sub-run approval notify = %+v; the root's trail records it acknowledged", n)
	}
}

type countingEvents struct {
	audit.EventStore
	loads atomic.Int64
}

func (c *countingEvents) Load(ctx context.Context, name string) ([][]byte, error) {
	c.loads.Add(1)
	return c.EventStore.Load(ctx, name)
}

// M1: every page load reads one trail per run, even for runs that ended with nothing to do.
func TestReviewTrailLoadsPerRun(t *testing.T) {
	done := sampleRecs(t, "refund-1041")
	x := Exports{}
	for i := 0; i < 200; i++ {
		x[fmt.Sprintf("r%03d", i)] = done
	}
	c := &countingEvents{EventStore: audit.NewMemEventStore()}
	Options{Source: x, Events: c}.attention(t.Context())
	t.Logf("attention: %d trail loads for 200 finished runs", c.loads.Load())
	if c.loads.Load() > 1 {
		t.Errorf("%d trail loads for 200 finished runs with nothing to do", c.loads.Load())
	}
}

// no issue: hostile ids and names are escaped.
func TestReviewAttentionEscapes(t *testing.T) {
	recs := sampleRecs(t, "refund-1043")
	const evil = `<script>alert(1)</script>"'`
	x := Exports{evil: recs}
	o := Options{Source: x, Now: func() time.Time { return attnNow }, ApproveCommand: func(r, c string, s bool) (string, error) { return "x " + r + " " + c, nil }}
	h := Handler(o)
	code, page := get(t, h, "/attention")
	if code != 200 || strings.Contains(page, "<script>alert(1)") {
		t.Errorf("unescaped run id in page (code %d)", code)
	}
}

// Confirmation round. A flow gate records its pause with RootRunID = its own run ID (agent/flowgate.go),
// so a flow run whose ID holds '>' has its events in its own trail, while treeRoot names the part before '>'.
func TestReviewFlowGateRootIsItsOwnRun(t *testing.T) {
	const id = "wf>1"
	recs := sampleRecs(t, "refund-1043")
	x := Exports{id: recs}
	v := Build(t.Context(), id, recs, nil)
	call := v.Gates[0].ToolUseID
	es := audit.NewMemEventStore()
	k := agent.ApprovalKey(id, call, 0)
	t0 := attnNow.Add(-time.Hour)
	trailWith(t, es, id,
		agent.NotifyRecord{Kind: agent.NotifyObserved, Key: k, RunID: id, At: t0, EventKind: string(agent.NotificationApprovalPending), EventAt: t0},
		agent.NotifyRecord{Kind: agent.NotifyAcked, Key: k, RunID: id, At: t0.Add(time.Second), Status: 200})
	l := Options{Source: x, Events: es, Now: func() time.Time { return attnNow }}.attention(t.Context())
	if n := notifyOf(l, "approval", id); n == nil || n.State != NotifyAcked {
		t.Errorf("notify = %+v; the run's own trail records it acknowledged", n)
	}
}
