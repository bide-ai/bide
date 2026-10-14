package runview

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

func clockAt(t time.Time) func() time.Time { return func() time.Time { return t } }

var (
	approvedAt = time.Date(2026, 10, 14, 9, 0, 5, 250_000_000, time.UTC)
	endedAt    = time.Date(2026, 10, 14, 9, 0, 9, 500_000_000, time.UTC)
	resolvedAt = time.Date(2026, 10, 14, 9, 30, 0, 125_000_000, time.UTC)
)

func viewOfJournal(t *testing.T, j *agent.Journal, runID string) *View {
	t.Helper()
	recs, err := j.History(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	v := Options{}.viewOf(context.Background(), runID, recs, nil)
	if v == nil {
		t.Fatal("no view")
	}
	return v
}

func eventNamed(t *testing.T, v *View, name string) Event {
	t.Helper()
	for _, e := range v.Events {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("no event %q in %d events", name, len(v.Events))
	return Event{}
}

func pageText(t *testing.T, v *View) string {
	t.Helper()
	w := httptest.NewRecorder()
	render(w, "run.html", v)
	if w.Code != 200 {
		t.Fatalf("render: %d %s", w.Code, w.Body)
	}
	return w.Body.String()
}

// A single approver's decision and a run's end carry the time they were written: the timeline
// places them on the run's clock, the JSON view and the verdict say it, and the gate shows when
// the decision was recorded.
func TestView_SingleApprovalAndEndTimes(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	refund := agent.MustFunc("refund", "refund", func(context.Context, struct{}) (string, error) { return "ok", nil }, agent.WithApproval(agent.SingleApproval()))
	run := func() error {
		m := agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "refund", `{}`), agenttest.TextTurn("done"))
		_, err := agenttest.MustNew(m, j, agent.WithTools(refund), agent.WithClock(clockAt(endedAt))).Run(ctx, "r", agent.UserText("go"))
		return err
	}
	if err := run(); err == nil {
		t.Fatal("want the approval pause")
	}
	if err := agent.Approve(ctx, j, "r", "c1", true, agent.WithClock(clockAt(approvedAt))); err != nil {
		t.Fatal(err)
	}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	v := viewOfJournal(t, j, "r")

	approval := eventNamed(t, v, "approval:c1")
	if approval.RecordedAt != "2026-10-14T09:00:05.250Z" || !approval.At.Equal(approvedAt) || approval.Abs != "2026-10-14 09:00:05.250 UTC" || approval.Rel == "" {
		t.Errorf("approval event = %+v, want it placed at %v", approval, approvedAt)
	}
	end := eventNamed(t, v, "run:complete")
	if end.RecordedAt != "2026-10-14T09:00:09.500Z" || !end.At.Equal(endedAt) || end.Bar == nil && end.Rel == "" {
		t.Errorf("end event = %+v, want it placed at %v", end, endedAt)
	}
	if v.EndedAt != "2026-10-14T09:00:09.500Z" {
		t.Errorf("EndedAt = %q", v.EndedAt)
	}
	if len(v.Gates) != 1 || v.Gates[0].DecisionAt() != "2026-10-14 09:00:05.250" {
		t.Errorf("gates = %+v, want the decision's recorded time", v.Gates)
	}
	if !strings.Contains(strings.Join(v.Verdict.Details, "\n"), "Ended 2026-10-14 09:00:09.500 UTC") {
		t.Errorf("verdict details = %q, want the end time", v.Verdict.Details)
	}
	b, err := json.Marshal(v)
	if err != nil || !strings.Contains(string(b), `"ended_at":"2026-10-14T09:00:09.500Z"`) || !strings.Contains(string(b), `"recorded_at":"2026-10-14T09:00:05.250Z"`) {
		t.Errorf("JSON view lacks the times (%v): %s", err, b)
	}
	page := pageText(t, v)
	for _, want := range []string{"Ended 2026-10-14 09:00:09.500 UTC", "recorded 2026-10-14 09:00:05.250 UTC"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

// A resolved halt shows when it was resolved, in the timeline and the verdict.
func TestView_ResolvedHaltTime(t *testing.T) {
	ctx := context.Background()
	cs := &crashStore{Store: agent.NewMemStore(), lose: agent.ToolResultStep("c1"), armed: true}
	j := agenttest.MustJournal(cs)
	charge := agent.MustFunc("charge", "charge", func(context.Context, struct{}) (string, error) { return "ok", nil })
	newAgent := func() *agent.Agent {
		m := agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "charge", `{}`), agenttest.TextTurn("done"))
		return agenttest.MustNew(m, j, agent.WithTools(charge))
	}
	if _, err := newAgent().Run(ctx, "r", agent.UserText("go")); !errors.Is(err, errCrash) {
		t.Fatalf("Run = %v, want the lost write", err)
	}
	cs.armed = false
	ref := agent.HaltRef{RunID: "r", Op: agent.OpRef{Kind: agent.OpTool, ID: "c1", ToolName: "charge"}, Cause: agent.HaltCrashed}
	if err := agent.ResolveHalt(ctx, j, ref, agent.Outcome{Result: "charged elsewhere", Evidence: "provider record"}, agent.WithClock(clockAt(resolvedAt)), agent.WithoutLiveDriverCheck()); err != nil {
		t.Fatal(err)
	}
	if _, err := newAgent().Run(ctx, "r", agent.UserText("go")); err != nil {
		t.Fatal(err)
	}
	v := viewOfJournal(t, j, "r")
	res := eventNamed(t, v, agent.ToolResultStep("c1"))
	if res.RecordedAt != "2026-10-14T09:30:00.125Z" || !res.At.Equal(resolvedAt) {
		t.Errorf("resolution event = %+v, want it placed at %v", res, resolvedAt)
	}
	if got := strings.Join(v.Verdict.Details, "\n"); !strings.Contains(got, "A halt was resolved at 2026-10-14 09:30:00.125 UTC") {
		t.Errorf("verdict details = %q, want the resolution time", got)
	}
}

// A recorded_at that is not in the canonical form is shown in the JSON view as written but does not
// place the row, give the verdict an end line, or appear on the page as a time.
func TestView_NonCanonicalRecordedAtIsNotATime(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	for _, bad := range []string{"yesterday", "2026-10-14T09:00:05Z", "2026-10-14T09:00:05.250+00:00"} {
		id := "r-" + strings.NewReplacer(":", "", "+", "p", ".", "d").Replace(bad)
		m := agenttest.NewScriptedModel(agenttest.TextTurn("done"))
		if _, err := agenttest.MustNew(m, j).Run(ctx, id, agent.UserText("go")); err != nil {
			t.Fatal(err)
		}
		v := Options{}.viewOf(ctx, id, rewritten(t, j, id, func(r *agent.Record) {
			if r.Name == "run:complete" {
				r.ApproverSignature = &agent.ApproverSignature{RecordedAt: bad}
			}
		}), nil)
		end := eventNamed(t, v, "run:complete")
		if end.RecordedAt != bad || !end.At.IsZero() || end.Rel != "" {
			t.Errorf("%q: end event = %+v, want the text kept and no place on the clock", bad, end)
		}
		if strings.Contains(strings.Join(v.Verdict.Details, "\n"), "Ended") {
			t.Errorf("%q: verdict shows an end time: %q", bad, v.Verdict.Details)
		}
	}
}

// A run whose records carry no recorded_at (an earlier version's journal) shows no times, and
// invents none.
func TestView_EarlierJournalShowsNoTimes(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	m := agenttest.NewScriptedModel(agenttest.TextTurn("done"))
	if _, err := agenttest.MustNew(m, j).Run(ctx, "r", agent.UserText("go")); err != nil {
		t.Fatal(err)
	}
	v := Options{}.viewOf(ctx, "r", rewritten(t, j, "r", func(r *agent.Record) { r.ApproverSignature = nil }), nil)
	if v.EndedAt != "" || eventNamed(t, v, "run:complete").RecordedAt != "" || strings.Contains(strings.Join(v.Verdict.Details, "\n"), "Ended") {
		t.Errorf("an earlier journal shows a time: %+v", v.Verdict)
	}
}

// rewritten returns the records of runID with edit applied to each, stored again and read back, so
// the view is built from the edited bytes.
func rewritten(t *testing.T, j *agent.Journal, runID string, edit func(*agent.Record)) []agent.Record {
	t.Helper()
	ctx := context.Background()
	recs, err := j.History(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	store := agent.NewMemStore()
	for _, r := range recs {
		edit(&r)
		b, err := agent.EncodeRecord(r)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Insert(ctx, runID, r.Name, b); err != nil {
			t.Fatal(err)
		}
	}
	out, err := agenttest.MustJournal(store).History(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A resolved halt whose recorded_at is not canonical gets no resolution line, and a one-decision
// gate alone reports a decision time: an m-of-n gate's times are on its approver cards.
func TestView_ResolutionLineAndGateTimeNeedCanonicalSingle(t *testing.T) {
	v := &View{State: "completed", Events: []Event{
		{Class: "tool", Title: "tool result: charge (c1)", Badges: []string{"reconciled by ResolveHalt"}, RecordedAt: "yesterday"},
		{Class: "tool", Title: "tool result: other (c2)", RecordedAt: "2026-10-14T09:30:00.125Z"},
	}}
	if got := runVerdict(v).Details; len(got) != 0 {
		t.Errorf("details = %q, want none for a garbled time and for an unreconciled result", got)
	}
	multi := Gate{Single: false, Decisions: []Decision{{Approver: "ops", Signed: true, RecordedAt: "2026-10-14T09:00:05.250Z"}}}
	if got := multi.DecisionAt(); got != "" {
		t.Errorf("m-of-n DecisionAt = %q, want none", got)
	}
}
