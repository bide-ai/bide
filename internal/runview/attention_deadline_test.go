package runview

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// started is the run:start record a run begins with.
func started() agent.Record {
	return agent.Record{Name: "run:start", Kind: agent.StepValue, Result: []byte(`{"started_at":"` + stamp(dlT0) + `"}`)}
}

// The Needs attention list puts waiting gates with a deadline first, nearest deadline first (an
// overdue one before the rest), flags a gate due within the hour, and shows how long is left.
func TestAttentionSortsGatesByDeadline(t *testing.T) {
	src := Exports{}
	now := dlT0.Add(30 * time.Minute)
	// "none" has no deadline and is the newest ask; "far" is due in 9h30m, "soon" in 30m, "late" passed 10m ago.
	for id, timeout := range map[string]time.Duration{"far": 10 * time.Hour, "soon": time.Hour, "late": 20 * time.Minute, "none": 0} {
		recs := gateRecs(timeout)
		if timeout == 0 {
			recs = recs[:2:2]
		}
		src[id] = append([]agent.Record{started()}, recs...)
	}
	l := attentionOf(t, Handler(Options{Source: src, Now: func() time.Time { return now }}))
	var order []string
	byRun := map[string]AttentionItem{}
	for _, it := range itemsOf(l, AttentionApproval) {
		order = append(order, it.RunID)
		byRun[it.RunID] = it
	}
	if want := []string{"late", "soon", "far", "none"}; !slices.Equal(order, want) {
		t.Fatalf("order = %v; want nearest deadline first, then the gate with none: %v", order, want)
	}
	for id, c := range map[string]struct {
		soon, overdue bool
		due           string
	}{
		"late": {true, true, "deadline passed 10m 00s ago (2026-10-15 09:20:00 UTC); the expiry is recorded when the run is next driven"},
		"soon": {true, false, "due in 30m 00s (2026-10-15 10:00:00 UTC)"},
		"far":  {false, false, "due in 9h 30m (2026-10-15 19:00:00 UTC)"},
		"none": {false, false, ""},
	} {
		g := byRun[id].Gate
		if g.DueSoon != c.soon || g.Overdue != c.overdue || g.Due != c.due {
			t.Errorf("%s: gate due_soon=%v overdue=%v due=%q; want %v %v %q", id, g.DueSoon, g.Overdue, g.Due, c.soon, c.overdue, c.due)
		}
	}
	if it := byRun["late"]; it.Command != "" || !strings.Contains(it.Summary, "passed with no decision") {
		t.Errorf("an overdue gate refuses decisions, so it has no approve command: command %q, summary %q", it.Command, it.Summary)
	}
}

// The page says which gates are due soon or overdue.
func TestAttentionPageShowsDeadlines(t *testing.T) {
	src := Exports{"soon": append([]agent.Record{started()}, gateRecs(time.Hour)...)}
	h := Handler(Options{Source: src, Now: func() time.Time { return dlT0.Add(30 * time.Minute) }, ApproveCommand: func(run, call string, single bool) (string, error) { return "bide approve " + run, nil }})
	_, body := get(t, h, "/attention")
	for _, want := range []string{"DUE SOON", "due in 30m 00s"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q:\n%s", want, body)
		}
	}
	h = Handler(Options{Source: src, Now: func() time.Time { return dlT0.Add(2 * time.Hour) }})
	_, body = get(t, h, "/attention")
	if !strings.Contains(body, "deadline passed 1h 00m ago") || !strings.Contains(body, "Decisions on this gate are refused") {
		t.Errorf("the overdue page:\n%s", body)
	}
}

// The run page shows the deadline and the time left, and a late decision as not counted.
func TestRunPageShowsDeadline(t *testing.T) {
	src := Exports{"r1": gateRecs(time.Hour,
		signed("ops", true, dlT0.Add(10*time.Minute), dlT0.Add(10*time.Minute)),
		signed("finance", true, dlT0.Add(59*time.Minute), dlT0.Add(61*time.Minute)))}
	h := Handler(Options{Source: src, Now: func() time.Time { return dlT0.Add(90 * time.Minute) }})
	_, body := get(t, h, "/run?id=r1")
	for _, want := range []string{"deadline passed 30m 00s ago", "times out after 1h", "not counted: recorded after the gate"} {
		if !strings.Contains(body, want) {
			t.Errorf("the run page lacks %q", want)
		}
	}
}

// A gate due in exactly the window is flagged due soon; one a second beyond it is not.
func TestDueSoonWindowEdge(t *testing.T) {
	for _, c := range []struct {
		timeout time.Duration
		soon    bool
	}{{dueSoonWindow, true}, {dueSoonWindow + time.Second, false}} {
		src := Exports{"r": append([]agent.Record{started()}, gateRecs(c.timeout)...)}
		l := attentionOf(t, Handler(Options{Source: src, Now: func() time.Time { return dlT0 }}))
		if got := itemsOf(l, AttentionApproval)[0].Gate.DueSoon; got != c.soon {
			t.Errorf("due in %s: due_soon = %v, want %v", c.timeout, got, c.soon)
		}
	}
}
